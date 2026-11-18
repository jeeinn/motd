package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

// P0-18 仓储回归：真实 SQLite + 真实迁移 + 真实写入队列。
// 重点验证一钥只属一号、软吊销（禁物理删除）、归属边界、事务回滚、
// 并发绑定幂等，以及解绑后 P0-14 身份解析确实进入「吊销」态。

var kbFingerprint = "SHA256:" + repeatByte('B', 43)
var kbFingerprint2 = "SHA256:" + repeatByte('D', 43)

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

func setupKeyBindingRepo(t *testing.T) (*KeyBindingRepos, *Pools, *Writer) {
	t.Helper()
	pools := openTestPools(t)
	if _, err := Migrate(context.Background(), pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)
	return NewKeyBindingRepos(pools, w), pools, w
}

func seedKeyBindingUser(t *testing.T, w *Writer, username string) int64 {
	t.Helper()
	var id int64
	err := w.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users (username, status, created_at, updated_at)
			VALUES (?, 'active', ?, ?)`, username, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("造测试用户失败: %v", err)
	}
	return id
}

type countingRevoker struct {
	mu    sync.Mutex
	calls []string
	n     int
}

func (r *countingRevoker) RevokeSessions(_ context.Context, fp string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fp)
	return r.n
}

func (r *countingRevoker) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func newKeyBindingService(t *testing.T, repos *KeyBindingRepos, rev *countingRevoker) *auth.KeyBindingService {
	t.Helper()
	return auth.NewKeyBindingService(repos, rev)
}

func bindReq(userID int64, fp string) auth.BindRequest {
	return auth.BindRequest{
		UserID:      userID,
		Fingerprint: fp,
		PublicKey:   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample wei@host",
	}
}

func TestKeyBindingRepoRoundTrip(t *testing.T) {
	repos, pools, w := setupKeyBindingRepo(t)
	uid := seedKeyBindingUser(t, w, "alice")
	svc := newKeyBindingService(t, repos, &countingRevoker{})

	res, err := svc.Bind(context.Background(), bindReq(uid, kbFingerprint))
	if err != nil || !res.Created {
		t.Fatalf("绑定失败: %+v err=%v", res, err)
	}

	list, err := svc.List(context.Background(), uid)
	if err != nil || len(list) != 1 || list[0].Fingerprint != kbFingerprint {
		t.Fatalf("/keys 应返回一行: %+v err=%v", list, err)
	}
	if list[0].Label != "" || list[0].HasLastUsed {
		t.Fatalf("P0 label 为空、last_used_at 未使用: %+v", list[0])
	}
	if list[0].CreatedAtUnix <= 0 {
		t.Fatal("created_at 应落成 Unix 秒")
	}

	// public_key 落的是服务端序列化文本，不是客户端任意字符串。
	var pub string
	if err := pools.QueryRowContext(context.Background(),
		`SELECT public_key FROM ssh_keys WHERE fingerprint = ?`, kbFingerprint).Scan(&pub); err != nil {
		t.Fatalf("读 public_key: %v", err)
	}
	if pub != bindReq(uid, kbFingerprint).PublicKey {
		t.Fatalf("public_key 落库不符: %q", pub)
	}
}

func TestKeyBindingRepoUniqueFingerprintRollsBackWholeTx(t *testing.T) {
	repos, pools, w := setupKeyBindingRepo(t)
	uid1 := seedKeyBindingUser(t, w, "alice")
	uid2 := seedKeyBindingUser(t, w, "bobbie")

	// 同一事务内插两次同指纹：第二次撞 UNIQUE → ErrKeyConflict → 整体回滚。
	err := repos.Write(context.Background(), func(tx auth.KeyBindingWriteTx) error {
		if err := tx.Insert(context.Background(), auth.NewKeyBinding{
			UserID: uid1, Fingerprint: kbFingerprint, PublicKey: "ssh-ed25519 AAAA", CreatedAt: time.Now(),
		}); err != nil {
			return err
		}
		return tx.Insert(context.Background(), auth.NewKeyBinding{
			UserID: uid2, Fingerprint: kbFingerprint, PublicKey: "ssh-ed25519 BBBB", CreatedAt: time.Now(),
		})
	})
	if !errors.Is(err, auth.ErrKeyConflict) {
		t.Fatalf("第二次插入必须映射为 ErrKeyConflict，实际 %v", err)
	}

	var n int
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ssh_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("事务整体回滚后不得留下任何绑定行，实际 %d", n)
	}
}

func TestKeyBindingRepoRevokeIsSoftNotDelete(t *testing.T) {
	repos, pools, w := setupKeyBindingRepo(t)
	uid := seedKeyBindingUser(t, w, "alice")
	rev := &countingRevoker{n: 2}
	svc := newKeyBindingService(t, repos, rev)

	if _, err := svc.Bind(context.Background(), bindReq(uid, kbFingerprint)); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Unbind(context.Background(), auth.UnbindRequest{UserID: uid, Fingerprint: kbFingerprint})
	if err != nil || !res.Revoked || res.SessionsClosed != 2 {
		t.Fatalf("解绑失败: %+v err=%v", res, err)
	}
	if rev.count() != 1 {
		t.Fatalf("事务提交后应撤销一次会话，实际 %d", rev.count())
	}

	var (
		count   int
		revoked sql.NullInt64
	)
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*), MIN(revoked_at) FROM ssh_keys WHERE fingerprint = ?`, kbFingerprint).
		Scan(&count, &revoked); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !revoked.Valid {
		t.Fatalf("软吊销：行必须仍在、revoked_at 非空（物理删除会让吊销退化成访客放行）: count=%d revoked=%v", count, revoked)
	}

	// P0-14 的三状态：解绑后必须命中「已吊销」，而不是「未命中 → 访客」。
	st, err := NewSSHKeyLookup(pools).Lookup(context.Background(), kbFingerprint)
	if err != nil || !st.Found || !st.Revoked {
		t.Fatalf("身份解析应看到吊销态: %+v err=%v", st, err)
	}
	// 用例侧也应拒绝重新绑定。
	if _, err := svc.Bind(context.Background(), bindReq(uid, kbFingerprint)); !errors.Is(err, auth.ErrKeyRevokedBinding) {
		t.Fatalf("已吊销公钥不得重新绑定，实际 %v", err)
	}
}

func TestKeyBindingRepoRevokeRejectsOtherUser(t *testing.T) {
	repos, pools, w := setupKeyBindingRepo(t)
	uid1 := seedKeyBindingUser(t, w, "alice")
	uid2 := seedKeyBindingUser(t, w, "bobbie")
	svc := newKeyBindingService(t, repos, &countingRevoker{})

	if _, err := svc.Bind(context.Background(), bindReq(uid1, kbFingerprint)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Unbind(context.Background(), auth.UnbindRequest{UserID: uid2, Fingerprint: kbFingerprint}); !errors.Is(err, auth.ErrKeyBoundToOtherUser) {
		t.Fatalf("不得解绑他人钥匙，实际 %v", err)
	}
	var revoked sql.NullInt64
	if err := pools.QueryRowContext(context.Background(),
		`SELECT revoked_at FROM ssh_keys WHERE fingerprint = ?`, kbFingerprint).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked.Valid {
		t.Fatal("越权尝试不得改动绑定")
	}
}

func TestKeyBindingRepoListExcludesRevokedAndOtherUsers(t *testing.T) {
	repos, _, w := setupKeyBindingRepo(t)
	uid1 := seedKeyBindingUser(t, w, "alice")
	uid2 := seedKeyBindingUser(t, w, "bobbie")
	svc := newKeyBindingService(t, repos, &countingRevoker{})

	for _, fp := range []string{kbFingerprint, kbFingerprint2} {
		if _, err := svc.Bind(context.Background(), bindReq(uid1, fp)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Bind(context.Background(), bindReq(uid2, "SHA256:"+repeatByte('E', 43))); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Unbind(context.Background(), auth.UnbindRequest{UserID: uid1, Fingerprint: kbFingerprint2}); err != nil {
		t.Fatal(err)
	}

	list, err := svc.List(context.Background(), uid1)
	if err != nil || len(list) != 1 || list[0].Fingerprint != kbFingerprint {
		t.Fatalf("只应剩本人未吊销的一把: %+v err=%v", list, err)
	}
}

func TestKeyBindingRepoInsertValidatesInput(t *testing.T) {
	repos, pools, w := setupKeyBindingRepo(t)
	uid := seedKeyBindingUser(t, w, "alice")
	cases := []struct {
		name string
		b    auth.NewKeyBinding
	}{
		{"非法用户 ID", auth.NewKeyBinding{UserID: 0, Fingerprint: kbFingerprint, PublicKey: "ssh-ed25519 AAAA"}},
		{"非法指纹", auth.NewKeyBinding{UserID: uid, Fingerprint: "ssh-rsa AAAA", PublicKey: "ssh-ed25519 AAAA"}},
		{"空公钥文本", auth.NewKeyBinding{UserID: uid, Fingerprint: kbFingerprint, PublicKey: ""}},
		{"含控制的公钥文本", auth.NewKeyBinding{UserID: uid, Fingerprint: kbFingerprint, PublicKey: "ssh-ed25519 AAAA\nBBBB"}},
	}
	for _, c := range cases {
		c.b.CreatedAt = time.Now()
		err := repos.Write(context.Background(), func(tx auth.KeyBindingWriteTx) error {
			return tx.Insert(context.Background(), c.b)
		})
		if err == nil {
			t.Errorf("%s 必须被仓储拒绝", c.name)
		}
	}
	var n int
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ssh_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("四次非法插入都不得落行，实际 %d", n)
	}
}

func TestKeyBindingConcurrentBindSameUserOnlyOneRow(t *testing.T) {
	repos, pools, w := setupKeyBindingRepo(t)
	uid := seedKeyBindingUser(t, w, "alice")
	svc := newKeyBindingService(t, repos, &countingRevoker{})

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Bind(context.Background(), bindReq(uid, kbFingerprint))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("同账号并发绑定应全部幂等成功，实际 %v", err)
		}
	}

	var n int
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ssh_keys WHERE fingerprint = ?`, kbFingerprint).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fingerprint UNIQUE 应只留一行，实际 %d", n)
	}
}

func TestKeyBindingConcurrentBindDifferentUsersOnlyOneWins(t *testing.T) {
	repos, pools, w := setupKeyBindingRepo(t)
	uids := make([]int64, 6)
	for i := range uids {
		uids[i] = seedKeyBindingUser(t, w, fmt.Sprintf("user_%02d", i))
	}
	svc := newKeyBindingService(t, repos, &countingRevoker{})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		denied  int
		other   []error
	)
	for _, uid := range uids {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			res, err := svc.Bind(context.Background(), bindReq(uid, kbFingerprint))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && res.Created:
				created++
			case err == nil || errors.Is(err, auth.ErrKeyBoundToOtherUser):
				denied++
			default:
				other = append(other, err)
			}
		}(uid)
	}
	wg.Wait()
	if len(other) > 0 {
		t.Fatalf("只允许创建或拒绝两类结果，异常: %v", other)
	}
	if created != 1 || denied != len(uids)-1 {
		t.Fatalf("一钥只属一号：应恰好 1 个创建、%d 个拒绝，实际 created=%d denied=%d",
			len(uids)-1, created, denied)
	}
	var n int
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ssh_keys WHERE fingerprint = ?`, kbFingerprint).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("并发后仍应只有一行，实际 %d", n)
	}
}

func TestKeyBindingE2EBindListUnbindRevoked(t *testing.T) {
	repos, pools, w := setupKeyBindingRepo(t)
	uid := seedKeyBindingUser(t, w, "alice")
	rev := &countingRevoker{n: 1}
	svc := newKeyBindingService(t, repos, rev)

	if _, err := svc.Bind(context.Background(), bindReq(uid, kbFingerprint)); err != nil {
		t.Fatal(err)
	}
	// 绑定成功后 P0-14 身份解析应给出登录态（D3 的库侧前提）。
	st, err := NewSSHKeyLookup(pools).Lookup(context.Background(), kbFingerprint)
	if err != nil || !st.Found || st.Revoked || st.UserID != uid {
		t.Fatalf("绑定后应解析为登录态: %+v err=%v", st, err)
	}

	if _, err := svc.Unbind(context.Background(), auth.UnbindRequest{UserID: uid, Fingerprint: kbFingerprint}); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(context.Background(), uid)
	if err != nil || len(list) != 0 {
		t.Fatalf("解绑后 /keys 应为空: %+v err=%v", list, err)
	}

	// 幂等：再解绑一次不报错、不再写 revoked_at。
	res, err := svc.Unbind(context.Background(), auth.UnbindRequest{UserID: uid, Fingerprint: kbFingerprint})
	if err != nil || res.Revoked {
		t.Fatalf("重复解绑应幂等: %+v err=%v", res, err)
	}
	var count int
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ssh_keys WHERE fingerprint = ?`, kbFingerprint).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("幂等解绑不得删除历史行")
	}
}
