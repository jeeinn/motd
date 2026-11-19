package auth

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

// ---- 测试替身 ----

type fakeKeyRow struct {
	userID  int64
	revoked bool
	pub     string
	created int64
}

// fakeKeyBindingStore 用「先复制快照、闭包成功后才替换」模拟事务回滚：
// 闭包返回错误时不提交，正好用来验证「失败不落半成品」。
type fakeKeyBindingStore struct {
	rows        map[string]fakeKeyRow
	writes      int
	listErr     error
	writeErr    error
	insertErr   error
	conflictN   int
	conflictHit int
	revokes     int
}

func newFakeKeyBindingStore() *fakeKeyBindingStore {
	return &fakeKeyBindingStore{rows: make(map[string]fakeKeyRow)}
}

func cloneKeyRows(in map[string]fakeKeyRow) map[string]fakeKeyRow {
	out := make(map[string]fakeKeyRow, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *fakeKeyBindingStore) ListActive(_ context.Context, userID int64) ([]KeyBinding, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	keys := make([]string, 0, len(s.rows))
	for fp := range s.rows {
		keys = append(keys, fp)
	}
	sort.Strings(keys)
	var out []KeyBinding
	for _, fp := range keys {
		r := s.rows[fp]
		if r.userID == userID && !r.revoked {
			out = append(out, KeyBinding{
				Fingerprint:   fp,
				CreatedAtUnix: r.created,
			})
		}
	}
	return out, nil
}

func (s *fakeKeyBindingStore) GetByFingerprint(_ context.Context, userID int64, fp string) (KeyBinding, error) {
	r, ok := s.rows[fp]
	if !ok || r.userID != userID || r.revoked {
		return KeyBinding{}, ErrKeyNotFound
	}
	return KeyBinding{Fingerprint: fp, CreatedAtUnix: r.created}, nil
}

func (s *fakeKeyBindingStore) Write(_ context.Context, fn func(KeyBindingWriteTx) error) error {
	s.writes++
	if s.writeErr != nil {
		return s.writeErr
	}
	tx := &fakeKeyBindingTx{s: s, rows: cloneKeyRows(s.rows)}
	if err := fn(tx); err != nil {
		return err // 不替换 s.rows == 整体回滚
	}
	s.rows = tx.rows
	return nil
}

type fakeKeyBindingTx struct {
	s    *fakeKeyBindingStore
	rows map[string]fakeKeyRow
}

func (t *fakeKeyBindingTx) FindByFingerprint(_ context.Context, fp string) (KeyBindingView, error) {
	r, ok := t.rows[fp]
	if !ok {
		return KeyBindingView{}, nil
	}
	return KeyBindingView{Found: true, UserID: r.userID, Revoked: r.revoked}, nil
}

func (t *fakeKeyBindingTx) Insert(_ context.Context, b NewKeyBinding) error {
	if t.s.conflictHit < t.s.conflictN {
		t.s.conflictHit++
		return ErrKeyConflict
	}
	if t.s.insertErr != nil {
		return t.s.insertErr
	}
	if _, exists := t.rows[b.Fingerprint]; exists {
		return ErrKeyConflict
	}
	t.rows[b.Fingerprint] = fakeKeyRow{userID: b.UserID, pub: b.PublicKey, created: b.CreatedAt.Unix()}
	return nil
}

func (t *fakeKeyBindingTx) Revoke(_ context.Context, userID int64, fp string, _ time.Time) (int64, error) {
	r, ok := t.rows[fp]
	if !ok || r.userID != userID || r.revoked {
		return 0, nil
	}
	r.revoked = true
	t.rows[fp] = r
	t.s.revokes++
	return 1, nil
}

type fakeRevoker struct {
	calls []string
	n     int
}

func (r *fakeRevoker) RevokeSessions(_ context.Context, fp string) int {
	r.calls = append(r.calls, fp)
	return r.n
}

func newFakeKeyService(store *fakeKeyBindingStore, revoker *fakeRevoker) *KeyBindingService {
	return NewKeyBindingService(store, revoker)
}

func newBindReq(userID int64, fp string) BindRequest {
	return BindRequest{UserID: userID, Fingerprint: fp, PublicKey: "ssh-ed25519 AAAAExample wei@host"}
}

// ---- 构造与 List ----

func TestNewKeyBindingServiceRejectsNilDependencies(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("store 为 nil 必须 panic")
		}
	}()
	_ = NewKeyBindingService(nil, &fakeRevoker{})
}

func TestNewKeyBindingServiceRejectsNilRevoker(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("revoker 为 nil 必须 panic")
		}
	}()
	_ = NewKeyBindingService(newFakeKeyBindingStore(), nil)
}

func TestListRequiresLogin(t *testing.T) {
	svc := newFakeKeyService(newFakeKeyBindingStore(), &fakeRevoker{})
	if _, err := svc.List(context.Background(), 0); !errors.Is(err, ErrBindingRequiresLogin) {
		t.Fatalf("访客不得列出公钥，实际 %v", err)
	}
}

func TestListReturnsOnlyActiveBindings(t *testing.T) {
	store := newFakeKeyBindingStore()
	otherFP := "SHA256:" + "C" + kbTestFingerprint[len("SHA256:")+1:]
	store.rows[kbTestFingerprint] = fakeKeyRow{userID: 7, created: 100}
	store.rows[otherFP] = fakeKeyRow{userID: 7, revoked: true}
	store.rows["SHA256:"+stringsRepeatD()] = fakeKeyRow{userID: 8}
	svc := newFakeKeyService(store, &fakeRevoker{})

	list, err := svc.List(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Fingerprint != kbTestFingerprint {
		t.Fatalf("只应返回本人未吊销的一把钥匙: %+v", list)
	}
	if store.writes != 0 {
		t.Fatal("List 必须零写入")
	}
}

func stringsRepeatD() string {
	out := make([]byte, 43)
	for i := range out {
		out[i] = 'D'
	}
	return string(out)
}

// ---- Bind ----

func TestBindRejectsGuestWithoutWrite(t *testing.T) {
	store := newFakeKeyBindingStore()
	svc := newFakeKeyService(store, &fakeRevoker{})
	req := newBindReq(7, kbTestFingerprint)
	req.UserID = 0
	if _, err := svc.Bind(context.Background(), req); !errors.Is(err, ErrBindingRequiresLogin) {
		t.Fatalf("访客绑定必须被拒绝，实际 %v", err)
	}
	if store.writes != 0 {
		t.Fatal("校验失败不得开写事务")
	}
}

func TestBindRejectsForgedPublicKeyTextAsFingerprint(t *testing.T) {
	store := newFakeKeyBindingStore()
	svc := newFakeKeyService(store, &fakeRevoker{})
	for _, fp := range []string{"ssh-rsa AAAAB3NzaC1yc2E...", "SHA256:short", "\x1b[2J"} {
		if _, err := svc.Bind(context.Background(), newBindReq(7, fp)); err == nil {
			t.Errorf("伪造/非法指纹 %q 不得进入绑定", fp)
		}
	}
	if store.writes != 0 {
		t.Fatal("格式校验必须在开事务之前完成")
	}
}

func TestBindRejectsInvalidPublicKeyText(t *testing.T) {
	store := newFakeKeyBindingStore()
	svc := newFakeKeyService(store, &fakeRevoker{})
	for _, pub := range []string{"", "ssh-ed25519 AAAA\nBBBB", "\x1b[2J"} {
		req := newBindReq(7, kbTestFingerprint)
		req.PublicKey = pub
		if _, err := svc.Bind(context.Background(), req); !errors.Is(err, ErrInvalidPublicKey) {
			t.Errorf("非法公钥文本 %q 应被拒绝，实际 %v", pub, err)
		}
	}
	if store.writes != 0 || len(store.rows) != 0 {
		t.Fatal("校验失败不得开事务或落半成品")
	}
}

func TestBindCreatesOnceAndDoesNotRevokeSessions(t *testing.T) {
	store := newFakeKeyBindingStore()
	rev := &fakeRevoker{n: 3}
	svc := newFakeKeyService(store, rev)
	res, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint))
	if err != nil || !res.Created || res.UserID != 7 {
		t.Fatalf("绑定应成功: %+v err=%v", res, err)
	}
	if len(store.rows) != 1 || store.writes != 1 {
		t.Fatalf("应恰好一个写事务一行绑定: rows=%d writes=%d", len(store.rows), store.writes)
	}
	if len(rev.calls) != 0 {
		t.Fatal("Bind 不得触发会话撤销")
	}
}

func TestBindIsIdempotent(t *testing.T) {
	store := newFakeKeyBindingStore()
	svc := newFakeKeyService(store, &fakeRevoker{})
	if _, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint)); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint))
	if err != nil || res.Created {
		t.Fatalf("重复绑定应幂等（Created=false）: %+v err=%v", res, err)
	}
	if len(store.rows) != 1 {
		t.Fatal("幂等不得产生第二行")
	}
}

func TestBindRejectsKeyOwnedByOtherUser(t *testing.T) {
	store := newFakeKeyBindingStore()
	svc := newFakeKeyService(store, &fakeRevoker{})
	if _, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Bind(context.Background(), newBindReq(8, kbTestFingerprint)); !errors.Is(err, ErrKeyBoundToOtherUser) {
		t.Fatalf("一钥只属一号：第二个账号必须被拒绝，实际 %v", err)
	}
	if store.rows[kbTestFingerprint].userID != 7 {
		t.Fatal("拒绝不得改动原绑定归属")
	}
}

func TestBindRejectsRevokedKey(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.rows[kbTestFingerprint] = fakeKeyRow{userID: 7, revoked: true}
	svc := newFakeKeyService(store, &fakeRevoker{})
	if _, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint)); !errors.Is(err, ErrKeyRevokedBinding) {
		t.Fatalf("已吊销公钥不得重新绑定，实际 %v", err)
	}
	if !store.rows[kbTestFingerprint].revoked {
		t.Fatal("吊销态不得被清除")
	}
}

func TestBindRetriesOnUniqueConflict(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.conflictN = 1 // 第一次 Insert 撞 UNIQUE，整体回滚；第二轮重新判定并成功
	svc := newFakeKeyService(store, &fakeRevoker{})
	res, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint))
	if err != nil || !res.Created {
		t.Fatalf("冲突后整体重试应成功: %+v err=%v", res, err)
	}
	if store.writes != 2 || len(store.rows) != 1 {
		t.Fatalf("应恰好两个事务、最终一行: writes=%d rows=%d", store.writes, len(store.rows))
	}
}

func TestBindRetriesExhaustedLeavesNoRow(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.conflictN = 99
	svc := newFakeKeyService(store, &fakeRevoker{})
	if _, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint)); !errors.Is(err, ErrKeyRetriesExhausted) {
		t.Fatalf("重试耗尽应返回哨兵，实际 %v", err)
	}
	if store.writes != maxKeyBindingAttempts || len(store.rows) != 0 {
		t.Fatalf("每轮都必须回滚，不得留下半成品: writes=%d rows=%d", store.writes, len(store.rows))
	}
}

func TestBindWriteFailureLeavesNoHalfProduct(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.writeErr = errors.New("db down")
	svc := newFakeKeyService(store, &fakeRevoker{})
	if _, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint)); err == nil {
		t.Fatal("写事务失败必须透传")
	}
	if len(store.rows) != 0 {
		t.Fatal("失败不得留下半成品绑定")
	}
}

func TestBindInsertFailureLeavesNoHalfProduct(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.insertErr = errors.New("insert failed")
	svc := newFakeKeyService(store, &fakeRevoker{})
	if _, err := svc.Bind(context.Background(), newBindReq(7, kbTestFingerprint)); err == nil {
		t.Fatal("插入失败必须透传")
	}
	if len(store.rows) != 0 {
		t.Fatal("事务整体回滚，不得留下半成品")
	}
}

// ---- Unbind ----

func TestUnbindRejectsGuestAndInvalidFingerprint(t *testing.T) {
	store := newFakeKeyBindingStore()
	svc := newFakeKeyService(store, &fakeRevoker{})
	if _, err := svc.Unbind(context.Background(), UnbindRequest{}); !errors.Is(err, ErrBindingRequiresLogin) {
		t.Fatalf("访客解绑必须被拒绝，实际 %v", err)
	}
	if _, err := svc.Unbind(context.Background(), UnbindRequest{UserID: 7, Fingerprint: "ssh-rsa AAAA"}); err == nil {
		t.Fatal("公钥文本不得作为解绑指纹")
	}
	if store.writes != 0 {
		t.Fatal("校验失败不得开写事务")
	}
}

func TestUnbindRevokesAndClosesSessionsAfterCommit(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.rows[kbTestFingerprint] = fakeKeyRow{userID: 7}
	rev := &fakeRevoker{n: 2}
	svc := newFakeKeyService(store, rev)
	res, err := svc.Unbind(context.Background(), UnbindRequest{UserID: 7, Fingerprint: kbTestFingerprint})
	if err != nil || !res.Revoked || res.SessionsClosed != 2 {
		t.Fatalf("解绑应成功且带回撤销会话数: %+v err=%v", res, err)
	}
	if !store.rows[kbTestFingerprint].revoked {
		t.Fatal("软吊销标记必须落库")
	}
	if len(rev.calls) != 1 || rev.calls[0] != kbTestFingerprint {
		t.Fatalf("事务提交后才应撤销一次会话: %v", rev.calls)
	}
}

func TestUnbindDoesNotRevokeSessionsOnWriteFailure(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.rows[kbTestFingerprint] = fakeKeyRow{userID: 7}
	store.writeErr = errors.New("db down")
	rev := &fakeRevoker{n: 5}
	svc := newFakeKeyService(store, rev)
	if _, err := svc.Unbind(context.Background(), UnbindRequest{UserID: 7, Fingerprint: kbTestFingerprint}); err == nil {
		t.Fatal("写事务失败必须透传")
	}
	if len(rev.calls) != 0 {
		t.Fatal("事务未提交成功时绝不能提前踢掉会话（不可回滚）")
	}
}

func TestUnbindNotFoundDoesNotRevokeSessions(t *testing.T) {
	store := newFakeKeyBindingStore()
	rev := &fakeRevoker{n: 5}
	svc := newFakeKeyService(store, rev)
	if _, err := svc.Unbind(context.Background(), UnbindRequest{UserID: 7, Fingerprint: kbTestFingerprint}); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("不存在的指纹应返回 ErrKeyNotFound，实际 %v", err)
	}
	if len(rev.calls) != 0 {
		t.Fatal("失败路径不得撤销会话")
	}
}

func TestUnbindCannotRevokeOtherUsersKey(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.rows[kbTestFingerprint] = fakeKeyRow{userID: 7}
	rev := &fakeRevoker{n: 5}
	svc := newFakeKeyService(store, rev)
	if _, err := svc.Unbind(context.Background(), UnbindRequest{UserID: 8, Fingerprint: kbTestFingerprint}); !errors.Is(err, ErrKeyBoundToOtherUser) {
		t.Fatalf("不得解绑他人钥匙，实际 %v", err)
	}
	if store.rows[kbTestFingerprint].revoked || len(rev.calls) != 0 {
		t.Fatal("越权尝试不得改动绑定，也不得撤销会话")
	}
}

func TestUnbindIsIdempotent(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.rows[kbTestFingerprint] = fakeKeyRow{userID: 7, revoked: true}
	rev := &fakeRevoker{n: 1}
	svc := newFakeKeyService(store, rev)
	res, err := svc.Unbind(context.Background(), UnbindRequest{UserID: 7, Fingerprint: kbTestFingerprint})
	if err != nil || res.Revoked {
		t.Fatalf("已吊销应幂等（Revoked=false）: %+v err=%v", res, err)
	}
	if len(rev.calls) != 1 {
		t.Fatal("幂等路径仍应清理可能残留的在线会话")
	}
}

func TestUnbindOnlyAffectsTargetKey(t *testing.T) {
	store := newFakeKeyBindingStore()
	otherFP := "SHA256:" + stringsRepeatD()
	store.rows[kbTestFingerprint] = fakeKeyRow{userID: 7}
	store.rows[otherFP] = fakeKeyRow{userID: 7}
	svc := newFakeKeyService(store, &fakeRevoker{})
	if _, err := svc.Unbind(context.Background(), UnbindRequest{UserID: 7, Fingerprint: kbTestFingerprint}); err != nil {
		t.Fatal(err)
	}
	if !store.rows[kbTestFingerprint].revoked || store.rows[otherFP].revoked {
		t.Fatal("解绑只影响该钥，另一把必须保持有效")
	}
	list, _ := svc.List(context.Background(), 7)
	if len(list) != 1 || list[0].Fingerprint != otherFP {
		t.Fatalf("/keys 列表应只剩另一把: %+v", list)
	}
}

func TestKeyDetailServiceGet(t *testing.T) {
	store := newFakeKeyBindingStore()
	store.rows[kbTestFingerprint] = fakeKeyRow{userID: 7, created: 1_700_000_000}
	svc := newFakeKeyService(store, &fakeRevoker{})

	got, err := svc.Get(context.Background(), 7, kbTestFingerprint)
	if err != nil || got.Fingerprint != kbTestFingerprint || got.CreatedAtUnix != 1_700_000_000 {
		t.Fatalf("详情查询失败: got=%+v err=%v", got, err)
	}
	if _, err := svc.Get(context.Background(), 8, kbTestFingerprint); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("跨用户应拒绝: %v", err)
	}
	if _, err := svc.Get(context.Background(), 7, "not-a-fingerprint"); !errors.Is(err, ErrInvalidFingerprint) {
		t.Fatalf("非法指纹格式应拒绝: %v", err)
	}
	if _, err := svc.Get(context.Background(), 0, kbTestFingerprint); !errors.Is(err, ErrBindingRequiresLogin) {
		t.Fatalf("访客应拒绝: %v", err)
	}
}
