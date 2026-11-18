package auth

import (
	"errors"
	"testing"
	"time"
)

// P0-16 领域层回归：四态枚举、迁移表穷举、轮询门禁、间隔兜底、会话标识校验。

func TestDeviceAuthStatusValidity(t *testing.T) {
	all := AllDeviceAuthStatuses()
	if len(all) != 4 {
		t.Fatalf("状态全集应为 4 个，实际 %d", len(all))
	}
	seen := map[DeviceAuthStatus]bool{}
	for _, s := range all {
		if !s.Valid() {
			t.Errorf("枚举值 %q 应合法", s)
		}
		if s.String() != string(s) {
			t.Errorf("String() 应返回字面值，实际 %q", s.String())
		}
		if seen[s] {
			t.Errorf("枚举值 %q 重复", s)
		}
		seen[s] = true
	}
	for _, bad := range []DeviceAuthStatus{"", "PENDING", "unknown", "active"} {
		if bad.Valid() {
			t.Errorf("非法状态 %q 不应通过 Valid()", bad)
		}
	}
	if !StatusPending.Valid() || StatusPending.Terminal() {
		t.Error("pending 合法且非终态")
	}
	for _, s := range []DeviceAuthStatus{StatusAuthorized, StatusDenied, StatusExpired} {
		if !s.Terminal() {
			t.Errorf("%s 应为终态", s)
		}
	}
	if DeviceAuthStatus("bogus").Terminal() {
		t.Error("非法状态不应被当作终态")
	}
}

// TestCanTransitionExhaustive 穷举 4×4 矩阵：只有 3 条路径合法（验收「非法路径被拒绝」）。
func TestCanTransitionExhaustive(t *testing.T) {
	legal := map[DeviceAuthStatus]map[DeviceAuthStatus]bool{
		StatusPending: {StatusAuthorized: true, StatusDenied: true, StatusExpired: true},
	}
	for _, from := range AllDeviceAuthStatuses() {
		for _, to := range AllDeviceAuthStatuses() {
			err := CanTransition(from, to)
			wantOK := legal[from][to]
			if wantOK && err != nil {
				t.Errorf("%s → %s 应合法，实际 %v", from, to, err)
			}
			if !wantOK && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s → %s 应被 ErrInvalidTransition 拒绝，实际 %v", from, to, err)
			}
		}
	}
	// 未知 / 零值两端都必须拒绝。
	for _, c := range [][2]DeviceAuthStatus{
		{"", StatusPending}, {StatusPending, ""}, {"bogus", StatusDenied}, {StatusPending, "bogus"},
	} {
		if err := CanTransition(c[0], c[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("CanTransition(%q,%q) 应拒绝，实际 %v", c[0], c[1], err)
		}
	}
}

func TestEvaluatePoll(t *testing.T) {
	base := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	st := DeviceAuthState{
		Status:    StatusPending,
		ExpiresAt: base.Add(900 * time.Second),
	}
	cases := []struct {
		name string
		mut  func(*DeviceAuthState)
		now  time.Time
		want PollEligibility
	}{
		{"未过期 pending 可轮询", func(*DeviceAuthState) {}, base.Add(1 * time.Second), GateReady},
		{"恰好到点视为过期", func(*DeviceAuthState) {}, st.ExpiresAt, GateExpired},
		{"超过 expires_at", func(*DeviceAuthState) {}, st.ExpiresAt.Add(time.Second), GateExpired},
		{"authorized 终态", func(s *DeviceAuthState) { s.Status = StatusAuthorized }, base, GateTerminal},
		{"denied 终态", func(s *DeviceAuthState) { s.Status = StatusDenied }, base, GateTerminal},
		{"expired 终态", func(s *DeviceAuthState) { s.Status = StatusExpired }, base, GateTerminal},
		{"非法状态按终态拒绝", func(s *DeviceAuthState) { s.Status = "bogus" }, base, GateTerminal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := st
			c.mut(&s)
			if got := EvaluatePoll(s, c.now); got != c.want {
				t.Fatalf("门禁 = %s，期望 %s", got, c.want)
			}
		})
	}
	if PollEligibility(0).String() != "unknown" {
		t.Error("未知门禁值 String() 应回落")
	}
}

// TestPollEligibilitySetIsClosed 把「门禁全集恰好 3 个」钉死：新增第 4 个值时
// 必须同步改 state_machine.go 的 switch（那里有失败关闭的 default 分支），
// 否则新值会被当成未知门禁拒绝而不是静默发起网络请求。
func TestPollEligibilitySetIsClosed(t *testing.T) {
	set := []PollEligibility{GateReady, GateTerminal, GateExpired}
	seen := map[PollEligibility]bool{}
	for _, e := range set {
		if e.String() == "unknown" {
			t.Errorf("有效门禁值 %d 必须有可读名称", e)
		}
		if seen[e] {
			t.Errorf("门禁值 %d 重复", e)
		}
		seen[e] = true
	}
	if len(set) != 3 {
		t.Fatalf("门禁全集应为 3 个，实际 %d", len(set))
	}
}

func TestDeviceAuthStatePollIntervalFallback(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{0, DefaultPollInterval}, {-1, DefaultPollInterval}, {5, 5}, {12, 12},
	} {
		if got := (DeviceAuthState{PollingInterval: c.in}).PollInterval(); got != c.want {
			t.Errorf("PollInterval(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

func TestNormalizeSessionRef(t *testing.T) {
	got, err := normalizeSessionRef("  sess-1  ")
	if err != nil || got != "sess-1" {
		t.Fatalf("应去空白后返回 sess-1，实际 (%q, %v)", got, err)
	}
	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, err := normalizeSessionRef(bad); !errors.Is(err, ErrSessionRefEmpty) {
			t.Errorf("空白会话标识 %q 应被拒绝，实际 %v", bad, err)
		}
	}
}

func TestTerminalMessageCoversAllStatuses(t *testing.T) {
	for _, s := range AllDeviceAuthStatuses() {
		if terminalMessage(s) == "" {
			t.Errorf("终态提示 %s 不应为空", s)
		}
	}
}

func TestPollOutcomeString(t *testing.T) {
	for _, c := range []struct {
		in   PollOutcome
		want string
	}{
		{OutcomeContinue, "continue"}, {OutcomeAuthorized, "authorized"},
		{OutcomeTerminal, "terminal"}, {PollOutcome(99), "unknown"},
	} {
		if got := c.in.String(); got != c.want {
			t.Errorf("PollOutcome(%d).String() = %q，期望 %q", c.in, got, c.want)
		}
	}
}
