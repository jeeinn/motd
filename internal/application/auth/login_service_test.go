package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// P0-17 用例层回归：内存 fake store（带真实事务回滚语义 + 唯一约束模拟）
// + fake client（记录网络调用是否误入写事务）。不依赖真实 SQLite / 网络。

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type loginAccount struct {
	id       int64
	username string
	email    string
	status   domainuser.Status
}

// loginStore LoginStore 的内存实现。Write 用互斥锁串行化，模拟单写连接；
// 闭包失败时整体回滚（accounts / binds / seq 一起恢复），不留半成品。
type loginStore struct {
	mu       sync.Mutex
	accounts map[int64]loginAccount
	binds    map[string]int64 // provider|providerUID -> userID
	seq      int64
	inWrite  bool
	writes   int

	identityLookups int
	usernameProbes  int
	emailLookups    int
	insertUsers     int
	insertIdents    int

	// 冲突注入：前 N 次插入返回唯一约束冲突，用于验证「整体重试」。
	conflictIdentityN  int
	conflictUsernameN  int
	failInsertIdentity error

	// committedByRival 模拟「另一个并发事务已提交」的身份绑定。
	// 它不属于本事务，事务回滚不清除；重试时 FindIdentityByUID 应命中它。
	committedByRival map[string]int64
	rivalAccounts    map[int64]loginAccount
}

var (
	_ LoginStore       = (*loginStore)(nil)
	_ LoginWriteTx     = (*loginTx)(nil)
	_ DeviceFlowClient = (*loginClient)(nil)
)

func newLoginStore() *loginStore {
	return &loginStore{
		accounts:         map[int64]loginAccount{},
		binds:            map[string]int64{},
		committedByRival: map[string]int64{},
		rivalAccounts:    map[int64]loginAccount{},
	}
}

func bindKey(provider, uid string) string { return provider + "|" + uid }

func (s *loginStore) snapshot() (map[int64]loginAccount, map[string]int64, int64) {
	a := make(map[int64]loginAccount, len(s.accounts))
	for k, v := range s.accounts {
		a[k] = v
	}
	b := make(map[string]int64, len(s.binds))
	for k, v := range s.binds {
		b[k] = v
	}
	return a, b, s.seq
}

func (s *loginStore) Write(_ context.Context, fn func(LoginWriteTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	beforeA, beforeB, beforeSeq := s.snapshot()
	s.writes++
	s.inWrite = true
	tx := &loginTx{s: s}
	err := fn(tx)
	s.inWrite = false
	if err != nil {
		s.accounts, s.binds, s.seq = beforeA, beforeB, beforeSeq
	}
	return err
}

func (s *loginStore) inWriteTxn() bool { return s.inWrite }

// seedAccount 预置一个账号（测试夹具用，不算写入事务）。
func (s *loginStore) seedAccount(username, email string, status domainuser.Status) int64 {
	s.seq++
	id := s.seq
	s.accounts[id] = loginAccount{id: id, username: username, email: email, status: status}
	return id
}

func (s *loginStore) seedIdentity(provider, uid string, userID int64) {
	s.binds[bindKey(provider, uid)] = userID
}

type loginTx struct{ s *loginStore }

func (t *loginTx) FindIdentityByUID(_ context.Context, provider, uid string) (AccountView, error) {
	t.s.identityLookups++
	key := bindKey(provider, uid)
	if id, ok := t.s.committedByRival[key]; ok {
		a := t.s.rivalAccounts[id]
		return AccountView{UserID: id, Username: a.username, Status: a.status}, nil
	}
	if id, ok := t.s.binds[key]; ok {
		a, exists := t.s.accounts[id]
		if !exists {
			return AccountView{}, ErrLoginNotFound
		}
		return AccountView{UserID: id, Username: a.username, Status: a.status}, nil
	}
	return AccountView{}, ErrLoginNotFound
}

func (t *loginTx) FindAccountByEmail(_ context.Context, email string) (AccountView, error) {
	t.s.emailLookups++
	for _, a := range t.s.accounts {
		if strings.EqualFold(a.email, email) && a.email != "" {
			return AccountView{UserID: a.id, Username: a.username, Status: a.status}, nil
		}
	}
	return AccountView{}, ErrLoginNotFound
}

func (t *loginTx) UsernameExists(_ context.Context, username string) (bool, error) {
	t.s.usernameProbes++
	for _, a := range t.s.accounts {
		if strings.EqualFold(a.username, username) {
			return true, nil
		}
	}
	return false, nil
}

func (t *loginTx) InsertUser(_ context.Context, n NewAccount) (int64, error) {
	if t.s.conflictUsernameN > 0 {
		t.s.conflictUsernameN--
		return 0, ErrUsernameConflict
	}
	// NOCASE UNIQUE 兜底：探测漏掉的大小写变体在这里也会被拦。
	for _, a := range t.s.accounts {
		if strings.EqualFold(a.username, n.Username) {
			return 0, ErrUsernameConflict
		}
	}
	t.s.insertUsers++
	t.s.seq++
	id := t.s.seq
	t.s.accounts[id] = loginAccount{
		id: id, username: n.Username, email: n.PrimaryEmail, status: n.Status,
	}
	return id, nil
}

func (t *loginTx) InsertIdentity(_ context.Context, i NewIdentity) error {
	t.s.insertIdents++
	key := bindKey(i.Provider, i.ProviderUID)
	if t.s.conflictIdentityN > 0 {
		t.s.conflictIdentityN--
		// 模拟「另一个并发事务抢先提交」：把赢家账号放进 rival 视图，
		// 重试时 FindIdentityByUID 必须命中它并转为登录。
		rivalID := int64(9000)
		t.s.rivalAccounts[rivalID] = loginAccount{
			id: rivalID, username: "rival", email: i.Email, status: domainuser.StatusActive,
		}
		t.s.committedByRival[key] = rivalID
		return ErrIdentityConflict
	}
	if t.s.failInsertIdentity != nil {
		return t.s.failInsertIdentity
	}
	if _, exists := t.s.binds[key]; exists {
		return ErrIdentityConflict
	}
	t.s.binds[key] = i.UserID
	return nil
}

// loginClient 只实现 GetUser/GetEmails；其余两个端点若被调用即视为缺陷。
type loginClient struct {
	store *loginStore

	user      GitHubUser
	emails    []GitHubEmail
	userErr   error
	emailsErr error

	userCalls   int
	emailsCalls int
	deviceCalls int
	pollCalls   int
	netInWrite  bool
	lastToken   string
}

func (c *loginClient) RequestDeviceCode(context.Context) (DeviceCode, error) {
	c.deviceCalls++
	return DeviceCode{}, errors.New("P0-17 不应调用 RequestDeviceCode")
}

func (c *loginClient) PollAccessToken(context.Context, string) (AccessToken, error) {
	c.pollCalls++
	return AccessToken{}, errors.New("P0-17 不应调用 PollAccessToken")
}

func (c *loginClient) GetUser(_ context.Context, token string) (GitHubUser, error) {
	c.userCalls++
	c.lastToken = token
	if c.store.inWriteTxn() {
		c.netInWrite = true
	}
	return c.user, c.userErr
}

func (c *loginClient) GetEmails(_ context.Context, _ string) ([]GitHubEmail, error) {
	c.emailsCalls++
	if c.store.inWriteTxn() {
		c.netInWrite = true
	}
	return c.emails, c.emailsErr
}

func newLoginFixture() (*loginStore, *loginClient, *LoginService) {
	store := newLoginStore()
	client := &loginClient{
		store: store,
		user:  GitHubUser{ID: 1001, Login: "alice", Email: "alice@example.com"},
		emails: []GitHubEmail{
			{Email: "alice@example.com", Primary: true, Verified: true},
		},
	}
	svc := NewLoginService(client, store)
	svc.now = func() time.Time { return fixedNow }
	return store, client, svc
}

// ---------------------------------------------------------------------------
// 前置拒绝 / 网络边界
// ---------------------------------------------------------------------------

func TestLoginEmptyTokenRejectedWithoutNetworkOrWrite(t *testing.T) {
	store, client, svc := newLoginFixture()
	_, err := svc.Login(context.Background(), "   ")
	if !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("空 token 应返回 ErrEmptyToken，实际 %v", err)
	}
	if client.userCalls != 0 || client.emailsCalls != 0 {
		t.Fatal("空 token 不得触发任何网络调用")
	}
	if store.writes != 0 {
		t.Fatal("空 token 不得开写事务")
	}
}

func TestLoginNetworkCallsAreOutsideWriteTransaction(t *testing.T) {
	_, client, svc := newLoginFixture()
	if _, err := svc.Login(context.Background(), "gho_token"); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if client.netInWrite {
		t.Fatal("GetUser / GetEmails 不得在写事务内调用（ADR-022 边界）")
	}
	if client.deviceCalls != 0 || client.pollCalls != 0 {
		t.Fatal("P0-17 只应调用 GetUser / GetEmails 两个端点")
	}
	if client.userCalls != 1 || client.emailsCalls != 1 {
		t.Fatalf("各应调用 1 次，实际 user=%d emails=%d", client.userCalls, client.emailsCalls)
	}
	if client.lastToken != "gho_token" {
		t.Fatalf("token 应原样交给客户端，实际 %q", client.lastToken)
	}
}

func TestLoginGetUserFailureLeavesNoRows(t *testing.T) {
	store, client, svc := newLoginFixture()
	client.userErr = errors.New("HTTP 500")
	_, err := svc.Login(context.Background(), "tok")
	if err == nil {
		t.Fatal("GetUser 失败应返回错误")
	}
	if store.writes != 0 || len(store.accounts) != 0 {
		t.Fatal("网络失败不得开写事务、不得落库")
	}
	if client.emailsCalls != 0 {
		t.Fatal("GetUser 失败后不应再调 GetEmails")
	}
}

func TestLoginGetEmailsFailureLeavesNoRows(t *testing.T) {
	store, _, svc := newLoginFixture()
	// 直接构造 client 以注入 emailsErr。
	client := &loginClient{store: store, user: GitHubUser{ID: 5, Login: "bob"}, emailsErr: errors.New("HTTP 403")}
	svc = NewLoginService(client, store)
	_, err := svc.Login(context.Background(), "tok")
	if err == nil {
		t.Fatal("GetEmails 失败应返回错误")
	}
	if store.writes != 0 || len(store.accounts) != 0 {
		t.Fatal("网络失败不得开写事务、不得落库")
	}
}

func TestLoginRejectsInvalidGitHubUserBeforeWrite(t *testing.T) {
	store, client, svc := newLoginFixture()
	client.user = GitHubUser{ID: 0, Login: "alice"}
	_, err := svc.Login(context.Background(), "tok")
	if !errors.Is(err, ErrInvalidIdentityProfile) {
		t.Fatalf("非法数字 id 应返回 ErrInvalidIdentityProfile，实际 %v", err)
	}
	if store.writes != 0 {
		t.Fatal("资料不完整时不得开写事务")
	}
}

// ---------------------------------------------------------------------------
// §4.3 步骤 1：命中已有身份直接登录
// ---------------------------------------------------------------------------

func TestLoginExistingIdentityReturnsLoginWithoutWrite(t *testing.T) {
	store, _, svc := newLoginFixture()
	id := store.seedAccount("alice", "alice@example.com", domainuser.StatusActive)
	store.seedIdentity(ProviderGitHub, "1001", id)

	res, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if res.Created || res.UserID != id || res.Username != "alice" {
		t.Fatalf("命中已有身份应直接登录，实际 %+v", res)
	}
	if store.insertUsers != 0 || store.insertIdents != 0 {
		t.Fatal("命中已有身份不得再插入 users / identities")
	}
	if store.writes != 1 {
		t.Fatalf("应只开 1 次写事务，实际 %d", store.writes)
	}
}

func TestLoginRejectsInactiveAccount(t *testing.T) {
	for _, status := range []domainuser.Status{
		domainuser.StatusPending, domainuser.StatusDeleting, domainuser.StatusDeleted,
	} {
		t.Run(string(status), func(t *testing.T) {
			store, _, svc := newLoginFixture()
			id := store.seedAccount("alice", "", status)
			store.seedIdentity(ProviderGitHub, "1001", id)
			_, err := svc.Login(context.Background(), "tok")
			if !errors.Is(err, ErrAccountInactive) {
				t.Fatalf("状态 %q 应返回 ErrAccountInactive，实际 %v", status, err)
			}
			if store.insertUsers != 0 {
				t.Fatal("停用账号不得新建")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §4.3 步骤 3：原子创建
// ---------------------------------------------------------------------------

func TestLoginCreatesUserAndIdentityAtomically(t *testing.T) {
	store, _, svc := newLoginFixture()
	res, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if !res.Created || res.UserID <= 0 || res.Username != "alice" {
		t.Fatalf("首登应新建账号，实际 %+v", res)
	}
	a, ok := store.accounts[res.UserID]
	if !ok {
		t.Fatal("users 未落库")
	}
	if a.status != domainuser.StatusActive {
		t.Fatalf("新账号应为 active，实际 %q", a.status)
	}
	if a.email != "alice@example.com" {
		t.Fatalf("primary_email 应落库，实际 %q", a.email)
	}
	if store.binds[bindKey(ProviderGitHub, "1001")] != res.UserID {
		t.Fatal("identities 未落库或 provider_uid 错误")
	}
	if store.insertUsers != 1 || store.insertIdents != 1 {
		t.Fatalf("应各插入 1 次，实际 users=%d idents=%d", store.insertUsers, store.insertIdents)
	}
}

func TestLoginInsertIdentityFailureRollsBackUser(t *testing.T) {
	store, _, svc := newLoginFixture()
	store.failInsertIdentity = errors.New("磁盘故障")
	_, err := svc.Login(context.Background(), "tok")
	if err == nil {
		t.Fatal("InsertIdentity 失败应返回错误")
	}
	if len(store.accounts) != 0 || len(store.binds) != 0 {
		t.Fatal("事务必须整体回滚，不得留下有 users 无 identities 的半成品")
	}
}

func TestLoginNoVerifiedEmailSkipsEmailLookup(t *testing.T) {
	store, client, svc := newLoginFixture()
	client.emails = []GitHubEmail{{Email: "unverified@example.com", Primary: true, Verified: false}}
	res, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if !res.Created {
		t.Fatal("无验证邮箱不应阻断建号")
	}
	if store.emailLookups != 0 {
		t.Fatal("无验证邮箱时不得执行步骤 2 的邮箱查询")
	}
	if a := store.accounts[res.UserID]; a.email != "" {
		t.Fatalf("无验证邮箱时 primary_email 应为空，实际 %q", a.email)
	}
}

// ---------------------------------------------------------------------------
// §4.3 步骤 2 + §7 R16：邮箱命中不合并、不阻断，仍建独立账号
// ---------------------------------------------------------------------------

func TestLoginEmailOccupiedDoesNotMerge(t *testing.T) {
	store, client, svc := newLoginFixture()
	// 已有账号 A 持有该邮箱，但身份是另一个 UID。
	otherID := store.seedAccount("victim", "shared@example.com", domainuser.StatusActive)
	store.seedIdentity(ProviderGitHub, "7777", otherID)
	client.emails = []GitHubEmail{{Email: "shared@example.com", Primary: true, Verified: true}}

	res, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if !res.Created {
		t.Fatal("邮箱命中不得阻断新 UID 建号（§7 R16）")
	}
	if res.UserID == otherID {
		t.Fatal("绝不能把新身份合并进邮箱命中的旧账号（账号接管）")
	}
	if res.Notice != EmailOccupiedNotice {
		t.Fatalf("应返回不自动合并的 Notice，实际 %q", res.Notice)
	}
	if len(store.accounts) != 2 {
		t.Fatalf("应有两个独立账号，实际 %d", len(store.accounts))
	}
	if store.binds[bindKey(ProviderGitHub, "1001")] == otherID {
		t.Fatal("新身份必须绑定到新账号")
	}
}

func TestLoginSameEmailDifferentUIDCreatesTwoAccounts(t *testing.T) {
	store, client, svc := newLoginFixture()
	client.emails = []GitHubEmail{{Email: "shared@example.com", Primary: true, Verified: true}}

	first, err := svc.Login(context.Background(), "tok")
	if err != nil || !first.Created {
		t.Fatalf("首次登录失败: %+v / %v", first, err)
	}
	// 换一个 GitHub UID，邮箱保持相同。
	client.user = GitHubUser{ID: 1002, Login: "alice2"}
	second, err := svc.Login(context.Background(), "tok")
	if err != nil || !second.Created {
		t.Fatalf("第二次登录失败: %+v / %v", second, err)
	}
	if first.UserID == second.UserID {
		t.Fatal("邮箱相同但 UID 不同必须建成两个账号（验收）")
	}
	if len(store.accounts) != 2 || len(store.binds) != 2 {
		t.Fatalf("应有 2 个账号 / 2 个身份，实际 %d / %d", len(store.accounts), len(store.binds))
	}
	if second.Notice != EmailOccupiedNotice {
		t.Fatal("第二次应带不自动合并的 Notice")
	}
	for id, a := range store.accounts {
		if a.email != "shared@example.com" {
			t.Fatalf("账号 %d 的 primary_email 应为 shared@example.com，实际 %q", id, a.email)
		}
	}
}

func TestLoginEmailNoticeCarriesNoExternalData(t *testing.T) {
	store, client, svc := newLoginFixture()
	store.seedAccount("victim", "evil\x1b@example.com", domainuser.StatusActive)
	client.emails = []GitHubEmail{{Email: "evil@example.com", Primary: true, Verified: true}}
	res, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if res.Notice != "" && strings.ContainsAny(res.Notice, "\x1b@") {
		t.Fatalf("Notice 不得回显邮箱或控制字符，实际 %q", res.Notice)
	}
}

// ---------------------------------------------------------------------------
// 用户名冲突与并发重试
// ---------------------------------------------------------------------------

func TestLoginUsernameCollisionAppendsSuffix(t *testing.T) {
	store, _, svc := newLoginFixture()
	store.seedAccount("alice", "", domainuser.StatusActive)

	res, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if res.Username == "alice" {
		t.Fatal("同名已被占用时应追加数字后缀")
	}
	if err := domainuser.ValidateUsername(res.Username); err != nil {
		t.Fatalf("后缀用户名必须合法: %v", err)
	}
}

func TestLoginUsernameConflictRetriesWholeTransaction(t *testing.T) {
	store, _, svc := newLoginFixture()
	// 模拟 NOCASE UNIQUE 被并发撞上：第一次插入返回冲突，回滚后重试。
	store.conflictUsernameN = 1
	res, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("可重试冲突应最终成功，实际 %v", err)
	}
	if !res.Created {
		t.Fatal("重试后应成功建号")
	}
	if store.writes != 2 {
		t.Fatalf("应开 2 次事务（首次冲突 + 重试），实际 %d", store.writes)
	}
	if len(store.accounts) != 1 {
		t.Fatalf("回滚后不得留半成品，实际 %d 个账号", len(store.accounts))
	}
}

func TestLoginIdentityConflictRetriesThenLogsIn(t *testing.T) {
	store, _, svc := newLoginFixture()
	// 第一次 InsertIdentity 撞 UNIQUE：模拟另一并发首登抢先提交。
	// 重试时 FindIdentityByUID 应命中赢家账号 → Created=false，不再建号。
	store.conflictIdentityN = 1
	res, err := svc.Login(context.Background(), "tok")
	if err != nil {
		t.Fatalf("并发冲突应转为登录，实际 %v", err)
	}
	if res.Created {
		t.Fatal("重试命中已有身份时不得重复建号")
	}
	if res.UserID != 9000 {
		t.Fatalf("应登录到并发赢家账号，实际 %d", res.UserID)
	}
	if store.writes != 2 {
		t.Fatalf("应开 2 次事务，实际 %d", store.writes)
	}
	if len(store.accounts) != 0 {
		t.Fatal("首次事务必须整体回滚，不得留下 locals 账号")
	}
}

func TestLoginRetriesExhausted(t *testing.T) {
	store, _, svc := newLoginFixture()
	store.conflictUsernameN = maxLoginAttempts + 5
	_, err := svc.Login(context.Background(), "tok")
	if !errors.Is(err, ErrLoginRetriesExhausted) {
		t.Fatalf("持续冲突应返回 ErrLoginRetriesExhausted，实际 %v", err)
	}
	if store.writes != maxLoginAttempts {
		t.Fatalf("应恰好重试 %d 次，实际 %d", maxLoginAttempts, store.writes)
	}
	if len(store.accounts) != 0 {
		t.Fatal("耗尽重试后不得留半成品")
	}
}

func TestLoginConcurrentSameUIDProducesOneAccount(t *testing.T) {
	store, _, svc := newLoginFixture()
	const n = 8
	var wg sync.WaitGroup
	results := make([]LoginResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.Login(context.Background(), "tok")
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("并发登录 %d 失败: %v", i, errs[i])
		}
	}
	if len(store.accounts) != 1 || len(store.binds) != 1 {
		t.Fatalf("并发同 UID 首登只能产生 1 个账号 / 1 个身份，实际 %d / %d",
			len(store.accounts), len(store.binds))
	}
	created := 0
	for _, r := range results {
		if r.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("应恰好 1 个 goroutine 建号，其余转为登录，实际 created=%d", created)
	}
}

// ---------------------------------------------------------------------------
// 令牌与装配
// ---------------------------------------------------------------------------

func TestLoginNeverCallsDeviceOrPollEndpoints(t *testing.T) {
	_, client, svc := newLoginFixture()
	if _, err := svc.Login(context.Background(), "tok"); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if client.deviceCalls != 0 || client.pollCalls != 0 {
		t.Fatal("登录事务只消费 token，不再发起 Device Flow 端点调用")
	}
}

func TestNewLoginServicePanicsOnNilDependency(t *testing.T) {
	store, client, _ := newLoginFixture()
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s 应 panic", name)
			}
		}()
		fn()
	}
	assertPanics("nil client", func() { NewLoginService(nil, store) })
	assertPanics("nil store", func() { NewLoginService(client, nil) })
}
