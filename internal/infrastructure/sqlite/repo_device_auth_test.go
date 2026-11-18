package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

// P0-16 仓储回归：真实 SQLite + 真实迁移 + 真实写入队列。
// 重点验证 CAS 语义、DDL 状态值域、同会话批量作废、事务原子性，
// 以及状态机用例接上真实仓储后的端到端行为。

func setupDeviceAuthRepo(t *testing.T) (*DeviceAuthRepos, *Pools, *Writer) {
	t.Helper()
	pools := openTestPools(t)
	if _, err := Migrate(context.Background(), pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)
	return NewDeviceAuthRepos(pools, w), pools, w
}

var repoFixedNow = time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)

func newPending(code, sess string) auth.NewDeviceAuthState {
	return auth.NewDeviceAuthState{
		Provider:        auth.DeviceProviderGitHub,
		DeviceCode:      code,
		UserCode:        "UC-" + code,
		PollingInterval: auth.DefaultPollInterval,
		ExpiresAt:       repoFixedNow.Add(900 * time.Second),
		Status:          auth.StatusPending,
		SessionRef:      sess,
		CreatedAt:       repoFixedNow,
	}
}

func insertRow(t *testing.T, repo *DeviceAuthRepos, n auth.NewDeviceAuthState) int64 {
	t.Helper()
	var id int64
	err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		var err error
		id, err = tx.Insert(context.Background(), n)
		return err
	})
	if err != nil {
		t.Fatalf("插入设备码状态失败: %v", err)
	}
	return id
}

func TestDeviceAuthInsertAndGetRoundTrip(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	n := newPending("dc-round", "sess-1")
	id := insertRow(t, repo, n)

	st, err := repo.GetByDeviceCode(context.Background(), "dc-round")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if st.ID != id || st.DeviceCode != "dc-round" || st.Provider != auth.DeviceProviderGitHub {
		t.Errorf("基础字段不一致: %+v", st)
	}
	if st.UserCode != n.UserCode || st.PollingInterval != n.PollingInterval || st.SessionRef != "sess-1" {
		t.Errorf("载荷字段不一致: %+v", st)
	}
	if st.Status != auth.StatusPending {
		t.Errorf("状态应为 pending，实际 %s", st.Status)
	}
	if !st.ExpiresAt.Equal(n.ExpiresAt) || !st.CreatedAt.Equal(n.CreatedAt) {
		t.Errorf("Unix 秒往返应无损: exp=%v crea=%v", st.ExpiresAt, st.CreatedAt)
	}
}

func TestDeviceAuthGetMissingReturnsNotFound(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	_, err := repo.GetByDeviceCode(context.Background(), "no-such-code")
	if !errors.Is(err, auth.ErrDeviceAuthNotFound) {
		t.Fatalf("应返回 ErrDeviceAuthNotFound，实际 %v", err)
	}
}

func TestDeviceAuthEmptySessionRefStoredAsNULL(t *testing.T) {
	repo, pools, _ := setupDeviceAuthRepo(t)
	n := newPending("dc-null", "")
	insertRow(t, repo, n)

	st, err := repo.GetByDeviceCode(context.Background(), "dc-null")
	if err != nil || st.SessionRef != "" {
		t.Fatalf("空 session_ref 应读回空串，实际 (%+v, %v)", st, err)
	}
	var raw sql.NullString
	if err := pools.QueryRowContext(context.Background(),
		`SELECT session_ref FROM device_auth_states WHERE device_code = ?`, "dc-null").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Valid {
		t.Error("空 session_ref 应落 NULL，而不是空字符串")
	}
}

// TestDeviceAuthDDLRejectsUnknownStatus 确认 DDL CHECK 与领域枚举是同一值域，
// 脏状态值无法通过任何旁路写入。
func TestDeviceAuthDDLRejectsUnknownStatus(t *testing.T) {
	_, pools, _ := setupDeviceAuthRepo(t)
	_, err := pools.writer.ExecContext(context.Background(), `
		INSERT INTO device_auth_states
			(provider, device_code, user_code, polling_interval, expires_at, status, session_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"github", "dc-bad-status", "UC", 5, repoFixedNow.Unix(), "bogus", "s", repoFixedNow.Unix())
	if err == nil {
		t.Fatal("DDL CHECK 应拒绝未知 status")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Errorf("应由 CHECK 约束拒绝，实际 %v", err)
	}
}

func TestDeviceAuthDeviceCodeIsUnique(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	n := newPending("dc-dup", "sess-1")
	insertRow(t, repo, n)

	err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		_, err := tx.Insert(context.Background(), n)
		return err
	})
	if err == nil {
		t.Fatal("device_code 重复应被 UNIQUE 拒绝")
	}
	var n2 int
	_ = repo.pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM device_auth_states WHERE device_code = ?`, "dc-dup").Scan(&n2)
	if n2 != 1 {
		t.Errorf("失败事务不应留下重复行，实际 %d 行", n2)
	}
}

func TestDeviceAuthSupersedeSessionOnlyPendingRows(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	insertRow(t, repo, newPending("dc-a1", "sess-1")) // 同会话 pending
	insertRow(t, repo, newPending("dc-a2", "sess-1")) // 同会话 pending
	other := newPending("dc-other", "sess-2")
	insertRow(t, repo, other) // 不同会话 pending

	// 让一个同会话记录先进入 authorized（终态不受作废影响）。
	authorized := newPending("dc-a3", "sess-1")
	authorized.Status = auth.StatusAuthorized
	insertRow(t, repo, authorized)

	var affected int64
	err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		var err error
		affected, err = tx.SupersedeSessionPending(context.Background(), "sess-1", auth.StatusExpired)
		return err
	})
	if err != nil {
		t.Fatalf("批量作废失败: %v", err)
	}
	if affected != 2 {
		t.Errorf("应只作废 2 条同会话 pending，实际 %d", affected)
	}
	for _, c := range []struct {
		code string
		want auth.DeviceAuthStatus
	}{
		{"dc-a1", auth.StatusExpired}, {"dc-a2", auth.StatusExpired},
		{"dc-a3", auth.StatusAuthorized}, {"dc-other", auth.StatusPending},
	} {
		st, err := repo.GetByDeviceCode(context.Background(), c.code)
		if err != nil {
			t.Errorf("读 %s 失败: %v", c.code, err)
			continue
		}
		if st.Status != c.want {
			t.Errorf("%s 状态应为 %s，实际 %s", c.code, c.want, st.Status)
		}
	}
}

func TestDeviceAuthSupersedeRejectsNonTerminalTarget(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		_, err := tx.SupersedeSessionPending(context.Background(), "sess-1", auth.StatusPending)
		return err
	})
	if !errors.Is(err, auth.ErrInvalidTransition) {
		t.Fatalf("作废目标必须是终态，实际 %v", err)
	}
}

func TestDeviceAuthTransitionCAS(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	id := insertRow(t, repo, newPending("dc-cas", "sess-1"))

	if err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		return tx.Transition(context.Background(), id, auth.StatusPending, auth.StatusAuthorized)
	}); err != nil {
		t.Fatalf("合法迁移失败: %v", err)
	}
	st, _ := repo.GetByDeviceCode(context.Background(), "dc-cas")
	if st.Status != auth.StatusAuthorized {
		t.Fatalf("状态应为 authorized，实际 %s", st.Status)
	}

	// 迟到的结果用旧状态做期望值：CAS 必须拒绝，不覆盖终态。
	err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		return tx.Transition(context.Background(), id, auth.StatusPending, auth.StatusDenied)
	})
	if !errors.Is(err, auth.ErrStateConflict) {
		t.Fatalf("应返回 ErrStateConflict，实际 %v", err)
	}
	st, _ = repo.GetByDeviceCode(context.Background(), "dc-cas")
	if st.Status != auth.StatusAuthorized {
		t.Errorf("终态不得被迟到结果覆盖，实际 %s", st.Status)
	}
}

func TestDeviceAuthTransitionMissingRowAndIllegalPath(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		return tx.Transition(context.Background(), 999999, auth.StatusPending, auth.StatusDenied)
	})
	if !errors.Is(err, auth.ErrDeviceAuthNotFound) {
		t.Errorf("不存在的行应返回 ErrDeviceAuthNotFound，实际 %v", err)
	}

	err = repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		return tx.Transition(context.Background(), 1, auth.StatusAuthorized, auth.StatusExpired)
	})
	if !errors.Is(err, auth.ErrInvalidTransition) {
		t.Errorf("终态出发的迁移应被拒绝，实际 %v", err)
	}
}

func TestDeviceAuthUpdateIntervalCAS(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	id := insertRow(t, repo, newPending("dc-iv", "sess-1"))

	if err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		return tx.UpdateInterval(context.Background(), id, auth.StatusPending, 15)
	}); err != nil {
		t.Fatalf("更新间隔失败: %v", err)
	}
	st, _ := repo.GetByDeviceCode(context.Background(), "dc-iv")
	if st.PollingInterval != 15 {
		t.Errorf("间隔应为 15，实际 %d", st.PollingInterval)
	}

	if err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		return tx.UpdateInterval(context.Background(), id, auth.StatusPending, 0)
	}); !errors.Is(err, auth.ErrInvalidInterval) {
		t.Errorf("非法间隔应被拒绝，实际 %v", err)
	}

	// 先把状态推进到终态，再用旧状态更新间隔 → CAS 拒绝。
	if err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		return tx.Transition(context.Background(), id, auth.StatusPending, auth.StatusDenied)
	}); err != nil {
		t.Fatal(err)
	}
	err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		return tx.UpdateInterval(context.Background(), id, auth.StatusPending, 20)
	})
	if !errors.Is(err, auth.ErrStateConflict) {
		t.Errorf("终态不得被迟到间隔更新命中，实际 %v", err)
	}
	st, _ = repo.GetByDeviceCode(context.Background(), "dc-iv")
	if st.PollingInterval != 15 {
		t.Errorf("间隔不应被改写，实际 %d", st.PollingInterval)
	}
}

func TestDeviceAuthWriteRollsBackOnClosureError(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	sentinel := errors.New("用例主动失败")
	err := repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
		if _, err := tx.Insert(context.Background(), newPending("dc-rb", "sess-1")); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("闭包错误应原样返回，实际 %v", err)
	}
	if _, err := repo.GetByDeviceCode(context.Background(), "dc-rb"); !errors.Is(err, auth.ErrDeviceAuthNotFound) {
		t.Errorf("事务回滚后不应留下行，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 端到端：状态机用例 + 真实 SQLite 仓储 + stub 客户端
// ---------------------------------------------------------------------------

type repoStubClient struct {
	dc         auth.DeviceCode
	pollErr    error
	token      auth.AccessToken
	pollCalls  int
	reqCalls   int
	beforePoll func()
}

func (c *repoStubClient) RequestDeviceCode(context.Context) (auth.DeviceCode, error) {
	c.reqCalls++
	return c.dc, nil
}
func (c *repoStubClient) PollAccessToken(context.Context, string) (auth.AccessToken, error) {
	c.pollCalls++
	if c.beforePoll != nil {
		c.beforePoll()
	}
	return c.token, c.pollErr
}
func (c *repoStubClient) GetUser(context.Context, string) (auth.GitHubUser, error) {
	return auth.GitHubUser{}, errors.New("P0-16 不调用")
}
func (c *repoStubClient) GetEmails(context.Context, string) ([]auth.GitHubEmail, error) {
	return nil, errors.New("P0-16 不调用")
}

var _ auth.DeviceFlowClient = (*repoStubClient)(nil)

func TestDeviceAuthStateMachineEndToEndWithRealSQLite(t *testing.T) {
	repo, pools, _ := setupDeviceAuthRepo(t)
	client := &repoStubClient{
		dc: auth.DeviceCode{
			DeviceCode: "e2e-code", UserCode: "WXYZ-9999",
			VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5,
		},
		token: auth.AccessToken{Token: "gho_e2e", TokenType: "bearer", Scope: auth.DeviceScope},
	}
	clock := repoFixedNow
	svc := auth.NewDeviceFlowService(repo, client,
		auth.WithDeviceFlowClock(func() time.Time { return clock }))

	res, err := svc.Start(context.Background(), "sess-e2e")
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if res.UserCode != "WXYZ-9999" {
		t.Errorf("展示信息不正确: %+v", res)
	}

	// 第一次轮询：authorization_pending → 继续保持 pending。
	client.pollErr = auth.ClassifyDeviceError("authorization_pending", "access_token", 0)
	step, err := svc.Poll(context.Background(), "e2e-code")
	if err != nil {
		t.Fatalf("pending 轮询失败: %v", err)
	}
	if step.Outcome != auth.OutcomeContinue || step.State.Status != auth.StatusPending {
		t.Errorf("应继续等待，实际 %+v", step)
	}

	// 第二次轮询：slow_down → 间隔持久化到 DB，跨请求可见。
	client.pollErr = auth.ClassifyDeviceError("slow_down", "access_token", 0)
	step, err = svc.Poll(context.Background(), "e2e-code")
	if err != nil {
		t.Fatalf("slow_down 轮询失败: %v", err)
	}
	if step.NextInterval != 10 {
		t.Errorf("退避应为 10，实际 %d", step.NextInterval)
	}
	st, _ := repo.GetByDeviceCode(context.Background(), "e2e-code")
	if st.PollingInterval != 10 || st.Status != auth.StatusPending {
		t.Errorf("DB 应持久化 interval=10 且仍 pending，实际 %+v", st)
	}

	// 第三次轮询成功：authorized，令牌在进程内返回但不落库。
	client.pollErr = nil
	step, err = svc.Poll(context.Background(), "e2e-code")
	if err != nil {
		t.Fatalf("成功轮询失败: %v", err)
	}
	if step.Outcome != auth.OutcomeAuthorized || step.Token.Token != "gho_e2e" {
		t.Errorf("应给出 authorized 与令牌，实际 %+v", step)
	}
	st, _ = repo.GetByDeviceCode(context.Background(), "e2e-code")
	if st.Status != auth.StatusAuthorized {
		t.Errorf("DB 状态应为 authorized，实际 %s", st.Status)
	}
	// 直接查 DB 全部文本列：令牌绝不以任何形式落库（Q3 / §4.4）。
	var allText string
	if err := pools.QueryRowContext(context.Background(), `
		SELECT COALESCE(provider,'') || COALESCE(device_code,'') || COALESCE(user_code,'') ||
		       COALESCE(status,'') || COALESCE(session_ref,'')
		FROM device_auth_states WHERE device_code = ?`, "e2e-code").Scan(&allText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(allText, "gho_e2e") {
		t.Error("令牌不得落库")
	}

	// 终态之后不再轮询，也不再写库。
	calls := client.pollCalls
	step, err = svc.Poll(context.Background(), "e2e-code")
	if err != nil {
		t.Fatalf("终态查询不应报错: %v", err)
	}
	if step.Outcome != auth.OutcomeTerminal || client.pollCalls != calls {
		t.Error("终态不得再发起网络轮询")
	}
}

func TestDeviceAuthE2EExpiredCodeDoesNotPoll(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	client := &repoStubClient{
		dc: auth.DeviceCode{
			DeviceCode: "exp-code", UserCode: "EXPI-0001",
			VerificationURI: "https://github.com/login/device", ExpiresIn: 60, Interval: 5,
		},
	}
	clock := repoFixedNow
	svc := auth.NewDeviceFlowService(repo, client,
		auth.WithDeviceFlowClock(func() time.Time { return clock }))
	if _, err := svc.Start(context.Background(), "sess-exp"); err != nil {
		t.Fatal(err)
	}

	clock = repoFixedNow.Add(61 * time.Second)
	res, err := svc.Poll(context.Background(), "exp-code")
	if err != nil {
		t.Fatalf("过期态不应报错: %v", err)
	}
	if client.pollCalls != 0 {
		t.Error("过期码不得发起网络轮询")
	}
	if res.State.Status != auth.StatusExpired {
		t.Errorf("应迁移为 expired，实际 %s", res.State.Status)
	}
	st, _ := repo.GetByDeviceCode(context.Background(), "exp-code")
	if st.Status != auth.StatusExpired {
		t.Errorf("DB 应持久化 expired，实际 %s", st.Status)
	}
}

func TestDeviceAuthE2EStartRollsBackSupersedeOnDuplicateCode(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	client := &repoStubClient{
		dc: auth.DeviceCode{
			DeviceCode: "same-code", UserCode: "SAME-0001",
			VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5,
		},
	}
	svc := auth.NewDeviceFlowService(repo, client,
		auth.WithDeviceFlowClock(func() time.Time { return repoFixedNow }))
	if _, err := svc.Start(context.Background(), "sess-rb"); err != nil {
		t.Fatalf("首次 Start 失败: %v", err)
	}

	// 第二次仍返回同一个 device_code：作废旧码 + 插入同码会撞 UNIQUE，
	// 整个事务必须回滚，旧码不能被半作废。
	_, err := svc.Start(context.Background(), "sess-rb")
	if err == nil {
		t.Fatal("重复 device_code 应导致事务失败")
	}
	st, getErr := repo.GetByDeviceCode(context.Background(), "same-code")
	if getErr != nil {
		t.Fatalf("旧码记录应仍存在: %v", getErr)
	}
	if st.Status != auth.StatusPending {
		t.Errorf("事务回滚后旧码应仍是 pending，实际 %s", st.Status)
	}
}

func TestDeviceAuthE2ELateResultCannotOverwriteCancel(t *testing.T) {
	repo, _, _ := setupDeviceAuthRepo(t)
	client := &repoStubClient{
		dc: auth.DeviceCode{
			DeviceCode: "race-code", UserCode: "RACE-0001",
			VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5,
		},
		token: auth.AccessToken{Token: "gho_race"},
	}
	svc := auth.NewDeviceFlowService(repo, client,
		auth.WithDeviceFlowClock(func() time.Time { return repoFixedNow }))
	if _, err := svc.Start(context.Background(), "sess-race"); err != nil {
		t.Fatal(err)
	}

	// 网络调用返回前，另一请求已把状态改成 denied。
	client.beforePoll = func() {
		_ = repo.Write(context.Background(), func(tx auth.DeviceAuthWriteTx) error {
			st, e := repo.GetByDeviceCode(context.Background(), "race-code")
			if e != nil {
				return e
			}
			return tx.Transition(context.Background(), st.ID, auth.StatusPending, auth.StatusDenied)
		})
	}

	_, err := svc.Poll(context.Background(), "race-code")
	if !errors.Is(err, auth.ErrStateConflict) {
		t.Fatalf("迟到的成功结果应被 CAS 拒绝，实际 %v", err)
	}
	st, _ := repo.GetByDeviceCode(context.Background(), "race-code")
	if st.Status != auth.StatusDenied {
		t.Errorf("取消优先，状态应保持 denied，实际 %s", st.Status)
	}
}
