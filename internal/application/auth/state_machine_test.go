package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// P0-16 状态机回归集。
//
// fakeStore 用内存行模拟 device_auth_states，但**保留真实语义**：
//   - Write 是事务：失败回滚快照（验「作废旧码 + 插新码」原子性）；
//   - Transition / UpdateInterval 带 CAS：状态不匹配返回 ErrStateConflict；
//   - 记录网络调用期间是否正处于写事务中（验「网络不占写连接」）。
// fakeClient 可在网络调用期间注入并发状态改动，模拟「用户按 Esc 取消后
// 迟到的轮询结果回来」这一真实竞态。

var fixedNow = time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)

type fakeStore struct {
	mu      sync.Mutex
	rows    map[int64]DeviceAuthState
	seq     int64
	writes  int
	inWrite bool

	failWrite      error
	failInsert     error
	failSupersede  error
	failTransition error
	failUpdate     error
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: make(map[int64]DeviceAuthState)}
}

func (s *fakeStore) snapshot() map[int64]DeviceAuthState {
	out := make(map[int64]DeviceAuthState, len(s.rows))
	for k, v := range s.rows {
		out[k] = v
	}
	return out
}

func (s *fakeStore) inWriteTxn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inWrite
}

func (s *fakeStore) GetByDeviceCode(_ context.Context, code string) (DeviceAuthState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.rows {
		if st.DeviceCode == code {
			return st, nil
		}
	}
	return DeviceAuthState{}, ErrDeviceAuthNotFound
}

func (s *fakeStore) Write(_ context.Context, fn func(DeviceAuthWriteTx) error) error {
	s.mu.Lock()
	before := s.snapshot()
	seqBefore := s.seq
	s.writes++
	s.inWrite = true
	s.mu.Unlock()

	tx := &fakeTx{s: s}
	err := fn(tx)

	s.mu.Lock()
	s.inWrite = false
	if err != nil || s.failWrite != nil {
		s.rows = before // 事务失败：整体回滚，不留半成品
		s.seq = seqBefore
	}
	s.mu.Unlock()

	if s.failWrite != nil {
		return s.failWrite
	}
	return err
}

// mutateConcurrently 供测试在网络调用期间模拟另一请求已提交的状态改动。
func (s *fakeStore) mutateConcurrently(code string, to DeviceAuthStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, st := range s.rows {
		if st.DeviceCode == code {
			st.Status = to
			s.rows[id] = st
		}
	}
}

func (s *fakeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

func (s *fakeStore) byCode(code string) (DeviceAuthState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.rows {
		if st.DeviceCode == code {
			return st, true
		}
	}
	return DeviceAuthState{}, false
}

type fakeTx struct{ s *fakeStore }

func (t *fakeTx) Insert(_ context.Context, n NewDeviceAuthState) (int64, error) {
	if t.s.failInsert != nil {
		return 0, t.s.failInsert
	}
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	for _, st := range t.s.rows {
		if st.DeviceCode == n.DeviceCode {
			return 0, fmt.Errorf("device_code 唯一约束冲突")
		}
	}
	t.s.seq++
	id := t.s.seq
	t.s.rows[id] = DeviceAuthState{
		ID: id, Provider: n.Provider, DeviceCode: n.DeviceCode, UserCode: n.UserCode,
		PollingInterval: n.PollingInterval, ExpiresAt: n.ExpiresAt, Status: n.Status,
		SessionRef: n.SessionRef, CreatedAt: n.CreatedAt,
	}
	return id, nil
}

func (t *fakeTx) SupersedeSessionPending(_ context.Context, sessionRef string, to DeviceAuthStatus) (int64, error) {
	if t.s.failSupersede != nil {
		return 0, t.s.failSupersede
	}
	if err := CanTransition(StatusPending, to); err != nil {
		return 0, err
	}
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	var n int64
	for id, st := range t.s.rows {
		if st.SessionRef == sessionRef && st.Status == StatusPending {
			st.Status = to
			t.s.rows[id] = st
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) Transition(_ context.Context, id int64, expectFrom, to DeviceAuthStatus) error {
	if t.s.failTransition != nil {
		return t.s.failTransition
	}
	if err := CanTransition(expectFrom, to); err != nil {
		return err
	}
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	st, ok := t.s.rows[id]
	if !ok {
		return ErrDeviceAuthNotFound
	}
	if st.Status != expectFrom {
		return ErrStateConflict
	}
	st.Status = to
	t.s.rows[id] = st
	return nil
}

func (t *fakeTx) UpdateInterval(_ context.Context, id int64, expectFrom DeviceAuthStatus, interval int) error {
	if t.s.failUpdate != nil {
		return t.s.failUpdate
	}
	if interval <= 0 {
		return ErrInvalidInterval
	}
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	st, ok := t.s.rows[id]
	if !ok {
		return ErrDeviceAuthNotFound
	}
	if st.Status != expectFrom {
		return ErrStateConflict
	}
	st.PollingInterval = interval
	t.s.rows[id] = st
	return nil
}

// fakeClient 记录网络调用次数，并在每次调用时检查是否已处于写事务中。
type fakeClient struct {
	store *fakeStore

	reqDC  DeviceCode
	reqErr error

	pollToken AccessToken
	pollErr   error

	reqCalls      int
	pollCalls     int
	netInWriteTxn bool // true 表示违反「网络调用不在写事务内」
	onPoll        func()
}

func (c *fakeClient) RequestDeviceCode(_ context.Context) (DeviceCode, error) {
	c.reqCalls++
	if c.store.inWriteTxn() {
		c.netInWriteTxn = true
	}
	return c.reqDC, c.reqErr
}

func (c *fakeClient) PollAccessToken(_ context.Context, _ string) (AccessToken, error) {
	c.pollCalls++
	if c.store.inWriteTxn() {
		c.netInWriteTxn = true
	}
	if c.onPoll != nil {
		c.onPoll()
	}
	return c.pollToken, c.pollErr
}

func (c *fakeClient) GetUser(_ context.Context, _ string) (GitHubUser, error) {
	return GitHubUser{}, errors.New("P0-16 不应调用 GetUser")
}

func (c *fakeClient) GetEmails(_ context.Context, _ string) ([]GitHubEmail, error) {
	return nil, errors.New("P0-16 不应调用 GetEmails")
}

var (
	_ DeviceAuthStore   = (*fakeStore)(nil)
	_ DeviceAuthWriteTx = (*fakeTx)(nil)
	_ DeviceFlowClient  = (*fakeClient)(nil)
)

func newFixture() (*fakeStore, *fakeClient, *DeviceFlowService) {
	store := newFakeStore()
	client := &fakeClient{
		store: store,
		reqDC: DeviceCode{
			DeviceCode: "dev-code-1", UserCode: "ABCD-1234",
			VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5,
		},
		pollToken: AccessToken{Token: "gho_secret", TokenType: "bearer", Scope: DeviceScope},
	}
	svc := NewDeviceFlowService(store, client, WithDeviceFlowClock(func() time.Time { return fixedNow }))
	return store, client, svc
}

func mustStart(t *testing.T, svc *DeviceFlowService, ref string) StartResult {
	t.Helper()
	res, err := svc.Start(context.Background(), ref)
	if err != nil {
		t.Fatalf("Start(%q) 失败: %v", ref, err)
	}
	return res
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

func TestStartPersistsPendingState(t *testing.T) {
	store, client, svc := newFixture()
	res := mustStart(t, svc, "sess-1")

	if res.UserCode != "ABCD-1234" || res.VerificationURI != "https://github.com/login/device" {
		t.Errorf("展示信息不正确: %+v", res)
	}
	if res.DeviceCode != "dev-code-1" || res.Interval != 5 {
		t.Errorf("服务端字段不正确: %+v", res)
	}
	if want := fixedNow.Add(900 * time.Second); !res.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v，期望 %v", res.ExpiresAt, want)
	}
	st, ok := store.byCode("dev-code-1")
	if !ok {
		t.Fatal("新设备码未落库")
	}
	if st.Status != StatusPending || st.Provider != DeviceProviderGitHub || st.SessionRef != "sess-1" {
		t.Errorf("落库状态不正确: %+v", st)
	}
	if st.PollingInterval != 5 || !st.ExpiresAt.Equal(res.ExpiresAt) {
		t.Errorf("落库间隔/过期时间不正确: %+v", st)
	}
	if store.writes != 1 {
		t.Errorf("Start 应恰好一个写事务，实际 %d", store.writes)
	}
	if client.netInWriteTxn {
		t.Error("RequestDeviceCode 不得在写事务内发起（会占住唯一写连接）")
	}
	if client.reqCalls != 1 {
		t.Errorf("RequestDeviceCode 调用次数 = %d，期望 1", client.reqCalls)
	}
}

func TestStartSupersedesOldCodeOnRepeatedRequest(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")

	client.reqDC = DeviceCode{
		DeviceCode: "dev-code-2", UserCode: "EFGH-5678",
		VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5,
	}
	second := mustStart(t, svc, "sess-1")
	if second.DeviceCode != "dev-code-2" {
		t.Fatalf("应以最后一次为准，实际 %q", second.DeviceCode)
	}

	old, ok := store.byCode("dev-code-1")
	if !ok {
		t.Fatal("旧码记录应保留（不物理删除），只是状态作废")
	}
	if old.Status != StatusExpired {
		t.Errorf("同会话旧码应作废为 expired，实际 %s", old.Status)
	}
	cur, _ := store.byCode("dev-code-2")
	if cur.Status != StatusPending {
		t.Errorf("新码应为 pending，实际 %s", cur.Status)
	}
	if store.count() != 2 {
		t.Errorf("应有两条记录（旧 expired + 新 pending），实际 %d", store.count())
	}
	// 另一个会话的 pending 不受影响。
	client.reqDC.DeviceCode = "dev-code-3"
	client.reqDC.UserCode = "IJKL-9012"
	mustStart(t, svc, "sess-2")
	if st, _ := store.byCode("dev-code-2"); st.Status != StatusPending {
		t.Error("不同会话发起不应作废 sess-1 的当前码")
	}
}

func TestStartRejectsEmptySessionRefWithoutNetwork(t *testing.T) {
	_, client, svc := newFixture()
	_, err := svc.Start(context.Background(), "  ")
	if !errors.Is(err, ErrSessionRefEmpty) {
		t.Fatalf("空会话标识应返回 ErrSessionRefEmpty，实际 %v", err)
	}
	if client.reqCalls != 0 {
		t.Error("参数校验失败时不应发起网络请求")
	}
}

func TestStartNetworkFailureLeavesNoRows(t *testing.T) {
	store, client, svc := newFixture()
	client.reqErr = errors.New("GitHub 5xx")
	_, err := svc.Start(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("网络失败应返回错误")
	}
	if store.count() != 0 || store.writes != 0 {
		t.Errorf("网络失败不应写库，rows=%d writes=%d", store.count(), store.writes)
	}
}

func TestStartRejectsMalformedDeviceCode(t *testing.T) {
	for _, c := range []struct {
		name string
		dc   DeviceCode
	}{
		{"缺 device_code", DeviceCode{UserCode: "ABCD-1234", ExpiresIn: 900, Interval: 5}},
		{"缺 user_code", DeviceCode{DeviceCode: "dc", ExpiresIn: 900, Interval: 5}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, client, svc := newFixture()
			client.reqDC = c.dc
			_, err := svc.Start(context.Background(), "sess-1")
			if !errors.Is(err, ErrMalformedDeviceCode) {
				t.Fatalf("应返回 ErrMalformedDeviceCode，实际 %v", err)
			}
			if store.count() != 0 {
				t.Error("响应不完整不应落库")
			}
		})
	}
}

func TestStartFallsBackOnMissingExpiresInAndInterval(t *testing.T) {
	store, client, svc := newFixture()
	client.reqDC = DeviceCode{DeviceCode: "dc", UserCode: "UC", ExpiresIn: 0, Interval: 0}
	res := mustStart(t, svc, "sess-1")
	if want := fixedNow.Add(DefaultExpiresIn * time.Second); !res.ExpiresAt.Equal(want) {
		t.Errorf("expires_in 缺失应回落 %ds，实际 %v", DefaultExpiresIn, res.ExpiresAt)
	}
	if res.Interval != DefaultPollInterval {
		t.Errorf("interval 缺失应回落 %ds，实际 %d", DefaultPollInterval, res.Interval)
	}
	st, _ := store.byCode("dc")
	if st.PollingInterval != DefaultPollInterval {
		t.Errorf("落库间隔应回落，实际 %d", st.PollingInterval)
	}
}

func TestStartTransactionRollsBackSupersedeOnInsertFailure(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")
	client.reqDC.DeviceCode = "dev-code-2"
	store.failInsert = errors.New("磁盘满")

	_, err := svc.Start(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("Insert 失败应返回错误")
	}
	store.failInsert = nil
	old, _ := store.byCode("dev-code-1")
	if old.Status != StatusPending {
		t.Errorf("事务回滚后旧码应仍是 pending，实际 %s（作废旧码不得半提交）", old.Status)
	}
	if store.count() != 1 {
		t.Errorf("事务整体回滚，行数应为 1，实际 %d", store.count())
	}
}

// ---------------------------------------------------------------------------
// Poll
// ---------------------------------------------------------------------------

func TestPollExpiredCodeDoesNotCallNetwork(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")

	// 时钟推进到 expires_at 之后。
	svc.now = func() time.Time { return fixedNow.Add(901 * time.Second) }
	res, err := svc.Poll(context.Background(), "dev-code-1")
	if err != nil {
		t.Fatalf("过期态应正常返回决策，实际 %v", err)
	}
	if client.pollCalls != 0 {
		t.Fatal("过期码不得再发起网络轮询（验收项）")
	}
	if res.Outcome != OutcomeTerminal || res.State.Status != StatusExpired {
		t.Errorf("应给出终态 expired 结论，实际 %+v", res)
	}
	if !strings.Contains(res.Message, "过期") {
		t.Errorf("应给出可读的过期提示，实际 %q", res.Message)
	}
	st, _ := store.byCode("dev-code-1")
	if st.Status != StatusExpired {
		t.Errorf("过期应持久化迁移为 expired，实际 %s", st.Status)
	}
}

func TestPollTerminalStateDoesNotCallNetworkOrWrite(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")
	store.mutateConcurrently("dev-code-1", StatusDenied)
	writesBefore := store.writes

	res, err := svc.Poll(context.Background(), "dev-code-1")
	if err != nil {
		t.Fatalf("终态应正常返回，实际 %v", err)
	}
	if client.pollCalls != 0 {
		t.Error("终态不得再轮询")
	}
	if store.writes != writesBefore {
		t.Error("终态不得再写库（避免重复刷状态）")
	}
	if res.Outcome != OutcomeTerminal || res.Message == "" {
		t.Errorf("应给出终态结论与提示，实际 %+v", res)
	}
}

func TestPollPendingContinuesWithInterval(t *testing.T) {
	_, client, svc := newFixture()
	mustStart(t, svc, "sess-1")
	client.pollErr = ClassifyDeviceError("authorization_pending", "access_token", 0)

	res, err := svc.Poll(context.Background(), "dev-code-1")
	if err != nil {
		t.Fatalf("authorization_pending 不应报错，实际 %v", err)
	}
	if res.Outcome != OutcomeContinue || res.NextInterval != 5 {
		t.Errorf("应继续轮询且间隔 >= 5，实际 %+v", res)
	}
	if res.State.Status != StatusPending {
		t.Errorf("pending 结果不应改变状态，实际 %s", res.State.Status)
	}
}

func TestPollSlowDownPersistsBackoff(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")
	client.pollErr = ClassifyDeviceError("slow_down", "access_token", 0)

	res, err := svc.Poll(context.Background(), "dev-code-1")
	if err != nil {
		t.Fatalf("slow_down 不应报错，实际 %v", err)
	}
	if res.Outcome != OutcomeContinue || res.NextInterval != 10 {
		t.Errorf("slow_down 应在 5 基础上 +5 = 10，实际 %+v", res)
	}
	st, _ := store.byCode("dev-code-1")
	if st.PollingInterval != 10 {
		t.Errorf("退避间隔必须持久化（跨请求存活），实际 %d", st.PollingInterval)
	}
	if st.Status != StatusPending {
		t.Errorf("slow_down 不应改变 pending 状态，实际 %s", st.Status)
	}
}

func TestPollSlowDownUsesResponseIntervalAndNeverShrinks(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")
	client.pollErr = ClassifyDeviceError("slow_down", "access_token", 20)

	res, err := svc.Poll(context.Background(), "dev-code-1")
	if err != nil {
		t.Fatalf("slow_down 不应报错，实际 %v", err)
	}
	if res.NextInterval != 20 {
		t.Errorf("应采纳响应新 interval=20，实际 %d", res.NextInterval)
	}
	st, _ := store.byCode("dev-code-1")
	if st.PollingInterval != 20 {
		t.Errorf("落库应为 20，实际 %d", st.PollingInterval)
	}

	// 再来一次响应给更小的值：不得缩小。
	client.pollErr = ClassifyDeviceError("slow_down", "access_token", 3)
	res, err = svc.Poll(context.Background(), "dev-code-1")
	if err != nil {
		t.Fatalf("第二次 slow_down 不应报错，实际 %v", err)
	}
	if res.NextInterval < 20 {
		t.Errorf("间隔不得缩小，实际 %d", res.NextInterval)
	}
}

func TestPollSuccessTransitionsAuthorizedAndTokenNotPersisted(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")

	res, err := svc.Poll(context.Background(), "dev-code-1")
	if err != nil {
		t.Fatalf("成功轮询不应报错，实际 %v", err)
	}
	if res.Outcome != OutcomeAuthorized || res.State.Status != StatusAuthorized {
		t.Errorf("应给出 authorized 结论，实际 %+v", res)
	}
	if res.Token.Token != "gho_secret" {
		t.Errorf("令牌应在进程内交给 P0-17，实际 %+v", res.Token)
	}
	st, _ := store.byCode("dev-code-1")
	if st.Status != StatusAuthorized {
		t.Errorf("落库状态应为 authorized，实际 %s", st.Status)
	}
	// 令牌绝不落库：DeviceAuthState 结构上没有 token 字段，用反射兜底确认
	// 序列化后的行里不含该值。
	if strings.Contains(fmt.Sprintf("%+v", st), "gho_secret") {
		t.Error("令牌不得出现在持久化状态里")
	}
	if client.netInWriteTxn {
		t.Error("PollAccessToken 不得在写事务内发起")
	}
}

func TestPollEmptyTokenIsRejected(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")
	client.pollToken = AccessToken{}
	_, err := svc.Poll(context.Background(), "dev-code-1")
	if !errors.Is(err, ErrMalformedToken) {
		t.Fatalf("空令牌应返回 ErrMalformedToken，实际 %v", err)
	}
	st, _ := store.byCode("dev-code-1")
	if st.Status != StatusPending {
		t.Errorf("空令牌不应改变状态，实际 %s", st.Status)
	}
}

func TestPollTerminalErrorCodesTransitions(t *testing.T) {
	cases := []struct {
		code     string
		interval int
		want     DeviceAuthStatus
	}{
		{"expired_token", 0, StatusExpired},
		{"token_expired", 0, StatusExpired},
		{"access_denied", 0, StatusDenied},
		{"device_flow_disabled", 0, StatusDenied},
		{"incorrect_client_credentials", 0, StatusDenied},
		{"incorrect_device_code", 0, StatusDenied},
		{"unsupported_grant_type", 0, StatusDenied},
		{"some_future_code", 0, StatusDenied},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			store, client, svc := newFixture()
			mustStart(t, svc, "sess-1")
			client.pollErr = ClassifyDeviceError(c.code, "access_token", c.interval)

			res, err := svc.Poll(context.Background(), "dev-code-1")
			if err != nil {
				t.Fatalf("终态错误码不应报错，实际 %v", err)
			}
			if res.Outcome != OutcomeTerminal || res.State.Status != c.want {
				t.Errorf("应迁移为 %s，实际 %+v", c.want, res)
			}
			st, _ := store.byCode("dev-code-1")
			if st.Status != c.want {
				t.Errorf("落库状态应为 %s，实际 %s", c.want, st.Status)
			}
			if res.Message == "" {
				t.Error("终态必须带可读提示")
			}
		})
	}
}

func TestPollTransportErrorDoesNotChangeState(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")
	client.pollErr = errors.New("connection reset") // 非 *PollError

	_, err := svc.Poll(context.Background(), "dev-code-1")
	if err == nil {
		t.Fatal("网络错误应返回给上层决定重试")
	}
	st, _ := store.byCode("dev-code-1")
	if st.Status != StatusPending {
		t.Errorf("一次网络抖动不得把登录判死，状态应保持 pending，实际 %s", st.Status)
	}
}

func TestPollLateResultCannotOverwriteCancelledState(t *testing.T) {
	store, client, svc := newFixture()
	mustStart(t, svc, "sess-1")

	// 网络调用期间用户在另一请求按 Esc 取消：状态先变成 denied。
	client.onPoll = func() {
		store.mutateConcurrently("dev-code-1", StatusDenied)
	}

	_, err := svc.Poll(context.Background(), "dev-code-1")
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("迟到的成功结果应被 CAS 拒绝，实际 %v", err)
	}
	st, _ := store.byCode("dev-code-1")
	if st.Status != StatusDenied {
		t.Errorf("取消优先：状态应保持 denied，实际 %s", st.Status)
	}
}

func TestPollRejectsEmptyCodeAndMissingRecord(t *testing.T) {
	_, client, svc := newFixture()
	if _, err := svc.Poll(context.Background(), "  "); !errors.Is(err, ErrDeviceCodeEmpty) {
		t.Errorf("空设备码应返回 ErrDeviceCodeEmpty，实际 %v", err)
	}
	if _, err := svc.Poll(context.Background(), "nope"); !errors.Is(err, ErrDeviceAuthNotFound) {
		t.Errorf("不存在的设备码应返回 ErrDeviceAuthNotFound，实际 %v", err)
	}
	if client.pollCalls != 0 {
		t.Error("两种前置失败都不应发起网络请求")
	}
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

func TestCancelTransitionsPendingToDenied(t *testing.T) {
	store, _, svc := newFixture()
	mustStart(t, svc, "sess-1")
	if err := svc.Cancel(context.Background(), "dev-code-1"); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	st, _ := store.byCode("dev-code-1")
	if st.Status != StatusDenied {
		t.Errorf("取消应迁移为 denied，实际 %s", st.Status)
	}
}

func TestCancelIsIdempotentAndRejectsEmptyCode(t *testing.T) {
	store, _, svc := newFixture()
	mustStart(t, svc, "sess-1")
	if err := svc.Cancel(context.Background(), "dev-code-1"); err != nil {
		t.Fatal(err)
	}
	writes := store.writes
	if err := svc.Cancel(context.Background(), "dev-code-1"); err != nil {
		t.Fatalf("重复取消应幂等，实际 %v", err)
	}
	if store.writes != writes {
		t.Error("已是终态时重复取消不应再写库")
	}
	if err := svc.Cancel(context.Background(), ""); !errors.Is(err, ErrDeviceCodeEmpty) {
		t.Errorf("空设备码应返回 ErrDeviceCodeEmpty，实际 %v", err)
	}
}

func TestCancelMissingRecord(t *testing.T) {
	_, _, svc := newFixture()
	if err := svc.Cancel(context.Background(), "nope"); !errors.Is(err, ErrDeviceAuthNotFound) {
		t.Errorf("不存在的设备码应返回 ErrDeviceAuthNotFound，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 构造与守卫
// ---------------------------------------------------------------------------

func TestNewDeviceFlowServicePanicsOnNilDependency(t *testing.T) {
	store, client, _ := newFixture()
	for _, c := range []struct {
		name   string
		store  DeviceAuthStore
		client DeviceFlowClient
	}{
		{"nil store", nil, client},
		{"nil client", store, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("nil 依赖应在装配期 panic")
				}
			}()
			NewDeviceFlowService(c.store, c.client)
		})
	}
}
