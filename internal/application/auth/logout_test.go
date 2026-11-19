package auth

import (
	"context"
	"errors"
	"testing"
)

type fakeUserRevoker struct {
	called []int64
	closed int
}

func (f *fakeUserRevoker) RevokeUserSessions(_ context.Context, userID int64) int {
	f.called = append(f.called, userID)
	return f.closed
}

func TestLogoutRevokesSessionsThenAudits(t *testing.T) {
	rev := &fakeUserRevoker{closed: 3}
	sink := &memoryAuditSink{}
	audit := NewAuditLogger(sink)
	svc := NewLogoutService(rev, NewSecurityGate(nil, audit, "github"))

	res, err := svc.Logout(context.Background(), 42, "ip-hash")
	if err != nil {
		t.Fatal(err)
	}
	if res.UserID != 42 || res.SessionsClosed != 3 {
		t.Fatalf("结果不符: %+v", res)
	}
	if len(rev.called) != 1 || rev.called[0] != 42 {
		t.Fatalf("撤销调用不符: %v", rev.called)
	}
	// 审计必须先落队：验证事件名、userID 与 detail 里的会话数。
	audit.Flush(context.Background())
	all := sink.all()
	if len(all) != 1 || all[0].Event != AuditLogout || all[0].UserID != 42 || all[0].IPHash != "ip-hash" {
		t.Fatalf("审计事件不符: %+v", all)
	}
	if all[0].DetailJSON != `{"sessions_closed":3}` {
		t.Fatalf("detail 不符: %q", all[0].DetailJSON)
	}
}

func TestLogoutRejectsGuestWithoutTouchingRevoker(t *testing.T) {
	rev := &fakeUserRevoker{closed: 99}
	svc := NewLogoutService(rev, nil)
	if _, err := svc.Logout(context.Background(), 0, ""); !errors.Is(err, ErrLogoutRequiresLogin) {
		t.Fatalf("访客注销应被拒: %v", err)
	}
	if len(rev.called) != 0 {
		t.Fatalf("访客注销不得调用撤销端口: %v", rev.called)
	}
}

func TestLogoutAuditFailureDoesNotUndoRevocation(t *testing.T) {
	rev := &fakeUserRevoker{closed: 2}
	// 容量 1 且先塞满，让注销审计入队失败。
	audit := NewAuditLogger(&memoryAuditSink{}, WithAuditCapacity(1))
	gate := NewSecurityGate(nil, audit, "github")
	if err := gate.NoteLoginFailure("other-ip", ""); err != nil {
		t.Fatal(err)
	}
	svc := NewLogoutService(rev, gate)
	res, err := svc.Logout(context.Background(), 1, "ip")
	if !errors.Is(err, ErrAuditQueueFull) {
		t.Fatalf("审计溢出应上报错误: %v", err)
	}
	if res.SessionsClosed != 2 || len(rev.called) != 1 {
		t.Fatalf("审计失败不得回滚已生效的撤销: res=%+v calls=%v", res, rev.called)
	}
}

func TestLogoutNilRevokerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("空 revoker 属于装配错误，必须立即 panic")
		}
	}()
	_ = NewLogoutService(nil, nil)
}

func TestKeyBindingGateRejectsBeforeTransaction(t *testing.T) {
	// 审计环满时，Bind / Unbind 必须在开写事务之前就失败。
	// 这里用空 store 的 KeyBindingService；若安全门失效就会panic/走到nil store。
	audit := NewAuditLogger(&memoryAuditSink{}, WithAuditCapacity(1))
	gate := NewSecurityGate(nil, audit, "github")
	svc := NewKeyBindingService(panicKeyBindingStore{}, loginNoopRevokerForAuthTest{}, WithKeyBindingGate(gate))
	// 用非法指纹先占住审计位：Bind 会先过安全门再校验指纹。
	if err := gate.NoteLoginFailure("ip", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Bind(context.Background(), BindRequest{UserID: 1, Fingerprint: "x", PublicKey: "y"}); !errors.Is(err, ErrAuditQueueFull) {
		t.Fatalf("审计满时 Bind 应被拒: %v", err)
	}
	if _, err := svc.Unbind(context.Background(), UnbindRequest{UserID: 1, Fingerprint: "x"}); !errors.Is(err, ErrAuditQueueFull) {
		t.Fatalf("审计满时 Unbind 应被拒: %v", err)
	}
}

type loginNoopRevokerForAuthTest struct{}

func (loginNoopRevokerForAuthTest) RevokeSessions(context.Context, string) int { return 0 }

// panicKeyBindingStore 一旦被进入写事务立即失败：安全门拒绝时绝不允许走到 DB。
type panicKeyBindingStore struct{}

func (panicKeyBindingStore) ListActive(context.Context, int64) ([]KeyBinding, error) {
	return nil, nil
}

func (panicKeyBindingStore) Write(context.Context, func(KeyBindingWriteTx) error) error {
	panic("安全门拒绝后不应进入写事务")
}
