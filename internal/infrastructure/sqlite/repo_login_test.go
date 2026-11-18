package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// P0-17 仓储回归：真实 SQLite + 真实迁移 + 真实写入队列。
// 重点验证 §4.3 三步判定落到真实表上的行为、NOCASE 唯一约束、
// 事务原子性、并发同 UID 幂等，以及令牌绝不落库。

func setupLoginRepo(t *testing.T) (*LoginRepos, *Pools, *Writer) {
	t.Helper()
	pools := openTestPools(t)
	if _, err := Migrate(context.Background(), pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)
	return NewLoginRepos(w), pools, w
}

var loginFixedNow = time.Date(2026, 11, 18, 15, 10, 0, 0, time.UTC)

// loginStubClient 只回答 GetUser / GetEmails，且并发安全（只读字段）。
type loginStubClient struct {
	user   auth.GitHubUser
	emails []auth.GitHubEmail
	err    error
}

func (c *loginStubClient) RequestDeviceCode(context.Context) (auth.DeviceCode, error) {
	return auth.DeviceCode{}, errors.New("P0-17 不应调用 RequestDeviceCode")
}

func (c *loginStubClient) PollAccessToken(context.Context, string) (auth.AccessToken, error) {
	return auth.AccessToken{}, errors.New("P0-17 不应调用 PollAccessToken")
}

func (c *loginStubClient) GetUser(context.Context, string) (auth.GitHubUser, error) {
	return c.user, c.err
}

func (c *loginStubClient) GetEmails(context.Context, string) ([]auth.GitHubEmail, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.emails, nil
}

func newLoginE2E(t *testing.T, user auth.GitHubUser, emails []auth.GitHubEmail) (*auth.LoginService, *Pools, *LoginRepos) {
	t.Helper()
	repo, pools, _ := setupLoginRepo(t)
	svc := auth.NewLoginService(&loginStubClient{user: user, emails: emails}, repo)
	return svc, pools, repo
}

func countRows(t *testing.T, pools *Pools, table string) int {
	t.Helper()
	var n int
	if err := pools.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("统计 %s: %v", table, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// 基本落库与幂等
// ---------------------------------------------------------------------------

func TestLoginE2ECreatesUserAndIdentityWithRealSQLite(t *testing.T) {
	svc, pools, _ := newLoginE2E(t,
		auth.GitHubUser{ID: 4242, Login: "Alice", Email: "alice@example.com"},
		[]auth.GitHubEmail{{Email: "Alice@Example.com", Primary: true, Verified: true}},
	)

	res, err := svc.Login(context.Background(), "gho_token")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if !res.Created {
		t.Fatal("首登应新建账号")
	}

	var (
		username, displayName, email, status string
	)
	if err := pools.QueryRowContext(context.Background(), `
		SELECT username, display_name, primary_email, status FROM users WHERE id = ?`, res.UserID).
		Scan(&username, &displayName, &email, &status); err != nil {
		t.Fatalf("读取账号: %v", err)
	}
	if username != "Alice" {
		t.Fatalf("username 应保留原大小写，实际 %q", username)
	}
	if displayName != "Alice" {
		t.Fatalf("display_name 应落 GitHub login，实际 %q", displayName)
	}
	if email != "alice@example.com" {
		t.Fatalf("primary_email 应 normalize 为小写，实际 %q", email)
	}
	if status != string(domainuser.StatusActive) {
		t.Fatalf("新账号应为 active，实际 %q", status)
	}

	var (
		uid      string
		idEmail  string
		verified int
	)
	if err := pools.QueryRowContext(context.Background(), `
		SELECT provider_uid, COALESCE(email,''), email_verified FROM identities WHERE user_id = ?`, res.UserID).
		Scan(&uid, &idEmail, &verified); err != nil {
		t.Fatalf("读取身份: %v", err)
	}
	if uid != "4242" {
		t.Fatalf("provider_uid 必须是数字 id，实际 %q", uid)
	}
	if idEmail != "alice@example.com" || verified != 1 {
		t.Fatalf("identity 邮箱应为已验证的 alice@example.com，实际 %q / %d", idEmail, verified)
	}
}

func TestLoginE2ERepeatIsIdempotent(t *testing.T) {
	svc, pools, _ := newLoginE2E(t,
		auth.GitHubUser{ID: 7, Login: "bob"},
		nil,
	)
	first, err := svc.Login(context.Background(), "tok")
	if err != nil || !first.Created {
		t.Fatalf("首次登录失败: %+v / %v", first, err)
	}
	second, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("再次登录失败: %v", err)
	}
	if second.Created || second.UserID != first.UserID {
		t.Fatalf("再次登录应幂等命中，实际 %+v", second)
	}
	if n := countRows(t, pools, "users"); n != 1 {
		t.Fatalf("users 应只有 1 行，实际 %d", n)
	}
	if n := countRows(t, pools, "identities"); n != 1 {
		t.Fatalf("identities 应只有 1 行，实际 %d", n)
	}
}

// ---------------------------------------------------------------------------
// §4.3 步骤 2 + §7 R16 / R7：邮箱相同 UID 不同 → 两个账号
// ---------------------------------------------------------------------------

func TestLoginE2ESameEmailDifferentUIDCreatesTwoAccounts(t *testing.T) {
	emails := []auth.GitHubEmail{{Email: "shared@example.com", Primary: true, Verified: true}}

	svc, pools, repo := newLoginE2E(t, auth.GitHubUser{ID: 1, Login: "first"}, emails)
	first, err := svc.Login(context.Background(), "tok")
	if err != nil || !first.Created {
		t.Fatalf("首次登录失败: %+v / %v", first, err)
	}

	// 同一仓储、换 UID（重建 service 只为换 client 的 user 字段）。
	svc2 := auth.NewLoginService(&loginStubClient{
		user:   auth.GitHubUser{ID: 2, Login: "second"},
		emails: emails,
	}, repo)
	second, err := svc2.Login(context.Background(), "tok")
	if err != nil || !second.Created {
		t.Fatalf("第二次登录失败: %+v / %v", second, err)
	}
	if first.UserID == second.UserID {
		t.Fatal("邮箱相同但 UID 不同必须建成两个账号（验收）")
	}
	if second.Notice != auth.EmailOccupiedNotice {
		t.Fatalf("第二次应带不自动合并 Notice，实际 %q", second.Notice)
	}
	if n := countRows(t, pools, "users"); n != 2 {
		t.Fatalf("users 应有 2 行（R7：primary_email 无唯一约束），实际 %d", n)
	}
	// 两个账号的 primary_email 相同，这正是「禁止按邮箱自动合并」的结构保证。
	var same int
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM users WHERE lower(primary_email) = 'shared@example.com'`).Scan(&same); err != nil {
		t.Fatal(err)
	}
	if same != 2 {
		t.Fatalf("两个账号应共享同一 primary_email，实际 %d", same)
	}
}

// ---------------------------------------------------------------------------
// 用户名：NOCASE 冲突 + 后缀 + 仓储二次校验
// ---------------------------------------------------------------------------

func TestLoginE2EUsernameConflictAppendsSuffixCaseInsensitive(t *testing.T) {
	svc, pools, repo := newLoginE2E(t, auth.GitHubUser{ID: 1, Login: "alice"}, nil)
	first, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("首次登录失败: %v", err)
	}
	if first.Username != "alice" {
		t.Fatalf("首次应为 alice，实际 %q", first.Username)
	}

	// 不同 UID、login 仅大小写不同 → NOCASE UNIQUE 必须判冲突并加后缀。
	svc2 := auth.NewLoginService(&loginStubClient{user: auth.GitHubUser{ID: 2, Login: "Alice"}}, repo)
	second, err := svc2.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("第二次登录失败: %v", err)
	}
	if strings.EqualFold(second.Username, "alice") {
		t.Fatalf("大小写变体应判冲突并加后缀，实际 %q", second.Username)
	}
	if err := domainuser.ValidateUsername(second.Username); err != nil {
		t.Fatalf("后缀用户名必须合法: %v", err)
	}
	if n := countRows(t, pools, "users"); n != 2 {
		t.Fatalf("应有两个账号，实际 %d", n)
	}
}

func TestLoginE2ERejectsInvalidUsernameAtRepository(t *testing.T) {
	repo, _, _ := setupLoginRepo(t)
	// 仓储是最后一道防线：即使调用方绕过 domain 校验，也必须拒绝写库。
	err := repo.Write(context.Background(), func(tx auth.LoginWriteTx) error {
		_, err := tx.InsertUser(context.Background(), auth.NewAccount{
			Username:  "has space",
			Status:    domainuser.StatusActive,
			CreatedAt: loginFixedNow,
		})
		return err
	})
	if err == nil || !errors.Is(err, domainuser.ErrInvalidUsername) {
		t.Fatalf("非法用户名应被仓储拒绝，实际 %v", err)
	}
}

func TestLoginE2ERejectsNonActiveStatusAtRepository(t *testing.T) {
	repo, _, _ := setupLoginRepo(t)
	err := repo.Write(context.Background(), func(tx auth.LoginWriteTx) error {
		_, err := tx.InsertUser(context.Background(), auth.NewAccount{
			Username:  "pendinguser",
			Status:    domainuser.StatusPending,
			CreatedAt: loginFixedNow,
		})
		return err
	})
	if err == nil {
		t.Fatal("非 active 状态应被仓储拒绝")
	}
}

// ---------------------------------------------------------------------------
// 事务原子性
// ---------------------------------------------------------------------------

func TestLoginE2EIdentityFailureRollsBackUser(t *testing.T) {
	repo, pools, _ := setupLoginRepo(t)
	// 预置一个同 (provider, provider_uid) 的身份，让 InsertIdentity 撞 UNIQUE。
	seedUser := func() int64 {
		var id int64
		if err := repo.Write(context.Background(), func(tx auth.LoginWriteTx) error {
			var err error
			id, err = tx.InsertUser(context.Background(), auth.NewAccount{
				Username:  "seeduser",
				Status:    domainuser.StatusActive,
				CreatedAt: loginFixedNow,
			})
			if err != nil {
				return err
			}
			return tx.InsertIdentity(context.Background(), auth.NewIdentity{
				UserID: id, Provider: auth.ProviderGitHub, ProviderUID: "1001",
				CreatedAt: loginFixedNow,
			})
		}); err != nil {
			t.Fatalf("预置失败: %v", err)
		}
		return id
	}
	seedID := seedUser()

	// 现在让用例走一条「步骤 1 查不到、插入 identity 却撞车」的路径：
	// 直接调仓储模拟（绕过 FindIdentityByUID 的命中），验证事务回滚。
	err := repo.Write(context.Background(), func(tx auth.LoginWriteTx) error {
		uid, err := tx.InsertUser(context.Background(), auth.NewAccount{
			Username: "seconduser", Status: domainuser.StatusActive, CreatedAt: loginFixedNow,
		})
		if err != nil {
			return err
		}
		return tx.InsertIdentity(context.Background(), auth.NewIdentity{
			UserID: uid, Provider: auth.ProviderGitHub, ProviderUID: "1001", CreatedAt: loginFixedNow,
		})
	})
	if !errors.Is(err, auth.ErrIdentityConflict) {
		t.Fatalf("撞 UNIQUE 应返回 ErrIdentityConflict，实际 %v", err)
	}
	if n := countRows(t, pools, "users"); n != 1 {
		t.Fatalf("事务必须整体回滚，users 应仍只有预置的 1 行，实际 %d", n)
	}
	if n := countRows(t, pools, "identities"); n != 1 {
		t.Fatalf("identities 应仍只有预置的 1 行，实际 %d", n)
	}
	var seedName string
	if err := pools.QueryRowContext(context.Background(),
		`SELECT username FROM users WHERE id = ?`, seedID).Scan(&seedName); err != nil {
		t.Fatal(err)
	}
	if seedName != "seeduser" {
		t.Fatalf("预置账号不应被改动，实际 %q", seedName)
	}
}

// ---------------------------------------------------------------------------
// 并发同 UID 首登
// ---------------------------------------------------------------------------

func TestLoginE2EConcurrentSameUIDCreatesOneAccount(t *testing.T) {
	svc, pools, _ := newLoginE2E(t,
		auth.GitHubUser{ID: 8888, Login: "concurrent"},
		[]auth.GitHubEmail{{Email: "c@example.com", Primary: true, Verified: true}},
	)

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	created := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := svc.Login(context.Background(), "tok")
			errs[i], created[i] = err, res.Created
		}(i)
	}
	wg.Wait()

	ok := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("并发登录 %d 失败: %v", i, errs[i])
		}
		if created[i] {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("并发同 UID 应恰好 1 次建号，实际 %d", ok)
	}
	if n := countRows(t, pools, "users"); n != 1 {
		t.Fatalf("users 应只有 1 行，实际 %d", n)
	}
	if n := countRows(t, pools, "identities"); n != 1 {
		t.Fatalf("identities 应只有 1 行，实际 %d", n)
	}
}

// ---------------------------------------------------------------------------
// 令牌不落库
// ---------------------------------------------------------------------------

func TestLoginE2ETokenNeverPersisted(t *testing.T) {
	const secret = "gho_supersecret_should_never_be_stored"
	svc, pools, _ := newLoginE2E(t,
		auth.GitHubUser{ID: 31, Login: "tokencheck"},
		[]auth.GitHubEmail{{Email: "t@example.com", Primary: true, Verified: true}},
	)
	if _, err := svc.Login(context.Background(), secret); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	// 扫 users / identities 的全部文本列与 profile_json，确认令牌没有落进去。
	rows, err := pools.QueryContext(context.Background(), `
		SELECT id, username, display_name, COALESCE(primary_email,''), status FROM users
		UNION ALL
		SELECT user_id, provider, provider_uid, COALESCE(email,''), profile_json FROM identities`)
	if err != nil {
		t.Fatalf("扫描表: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c, d, e string
		if err := rows.Scan(&a, &b, &c, &d, &e); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join([]string{a, b, c, d, e}, "|")
		if strings.Contains(joined, secret) || strings.Contains(strings.ToLower(joined), "gho_") {
			t.Fatalf("令牌不得落库，命中行: %q", joined)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 端口基础语义
// ---------------------------------------------------------------------------

func TestLoginE2EFindIdentityNotFound(t *testing.T) {
	repo, _, _ := setupLoginRepo(t)
	err := repo.Write(context.Background(), func(tx auth.LoginWriteTx) error {
		_, err := tx.FindIdentityByUID(context.Background(), auth.ProviderGitHub, "nope")
		if !errors.Is(err, auth.ErrLoginNotFound) {
			t.Fatalf("未命中应返回 ErrLoginNotFound，实际 %v", err)
		}
		_, err = tx.FindAccountByEmail(context.Background(), "nobody@example.com")
		if !errors.Is(err, auth.ErrLoginNotFound) {
			t.Fatalf("邮箱未命中应返回 ErrLoginNotFound，实际 %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

func TestLoginE2EUsernameExistsIsCaseInsensitive(t *testing.T) {
	repo, _, _ := setupLoginRepo(t)
	if err := repo.Write(context.Background(), func(tx auth.LoginWriteTx) error {
		if _, err := tx.InsertUser(context.Background(), auth.NewAccount{
			Username: "MixedCase", Status: domainuser.StatusActive, CreatedAt: loginFixedNow,
		}); err != nil {
			return err
		}
		for _, probe := range []string{"MixedCase", "mixedcase", "MIXEDCASE"} {
			exists, err := tx.UsernameExists(context.Background(), probe)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("探测 %q 应命中", probe)
			}
		}
		exists, err := tx.UsernameExists(context.Background(), "othername")
		if err != nil {
			return err
		}
		if exists {
			return errors.New("不同名不应命中")
		}
		return nil
	}); err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

func TestLoginE2EInactiveAccountCannotLogin(t *testing.T) {
	repo, pools, _ := setupLoginRepo(t)
	// 预置 active 账号 + 身份，然后把状态改成 pending（模拟注销流程进行中）。
	var userID int64
	if err := repo.Write(context.Background(), func(tx auth.LoginWriteTx) error {
		id, err := tx.InsertUser(context.Background(), auth.NewAccount{
			Username: "pendingone", Status: domainuser.StatusActive, CreatedAt: loginFixedNow,
		})
		if err != nil {
			return err
		}
		userID = id
		return tx.InsertIdentity(context.Background(), auth.NewIdentity{
			UserID: id, Provider: auth.ProviderGitHub, ProviderUID: "5555", CreatedAt: loginFixedNow,
		})
	}); err != nil {
		t.Fatalf("预置失败: %v", err)
	}
	if _, err := pools.writer.ExecContext(context.Background(),
		`UPDATE users SET status = 'pending' WHERE id = ?`, userID); err != nil {
		t.Fatalf("改状态失败: %v", err)
	}

	svc := auth.NewLoginService(&loginStubClient{user: auth.GitHubUser{ID: 5555, Login: "pendingone"}}, repo)
	_, err := svc.Login(context.Background(), "tok")
	if !errors.Is(err, auth.ErrAccountInactive) {
		t.Fatalf("pending 账号必须拒绝登录，实际 %v", err)
	}
	if n := countRows(t, pools, "users"); n != 1 {
		t.Fatalf("拒绝登录不得新建账号，实际 %d", n)
	}
}
