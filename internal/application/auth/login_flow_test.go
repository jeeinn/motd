package auth

// P0-19 领域层回归：呈现投影白名单、间隔钳制、轮询指令分类。
// 全部是纯函数测试，不碰 SQLite / 网络 / 时间循环。

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var loginFlowNow = time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)

func loginFlowStart(uc, uri string) StartResult {
	return StartResult{
		DeviceCode:      "dc-private-only",
		UserCode:        uc,
		VerificationURI: uri,
		ExpiresAt:       loginFlowNow.Add(900 * time.Second),
		Interval:        5,
	}
}

func TestProjectLoginViewAcceptsOfficialShape(t *testing.T) {
	v, err := ProjectLoginView(loginFlowStart("ABCD-EFGH", "https://github.com/login/device"), loginFlowNow)
	if err != nil {
		t.Fatalf("官方形态应被接受: %v", err)
	}
	if v.UserCode != "ABCD-EFGH" || v.VerificationURI != "https://github.com/login/device" {
		t.Fatalf("投影字段不符: %+v", v)
	}
	if v.IntervalSeconds != LoginMinIntervalSeconds {
		t.Fatalf("间隔应被钳制为 %d，实际 %d", LoginMinIntervalSeconds, v.IntervalSeconds)
	}
	if v.RemainingSeconds <= 0 {
		t.Fatalf("剩余时间应为正数，实际 %d", v.RemainingSeconds)
	}
}

func TestProjectLoginViewRejectsUnsafeUserCode(t *testing.T) {
	bad := []string{
		"", "ABC", "abcd-efgh", // 小写不在 GitHub user_code 字符集
		"ABCD-EFGH\x1b[2J", "ABCD EFGH", "ABCD\nEFGH", "ABCD\tEFGH",
		"----", strings.Repeat("A", maxUserCodeRunes+1),
		"ABCD-EFGH\u202e", "ABCD<script>",
	}
	for _, uc := range bad {
		if _, err := ProjectLoginView(loginFlowStart(uc, "https://github.com/login/device"), loginFlowNow); !errors.Is(err, ErrUnsafeLoginView) {
			t.Errorf("非法 user_code %q 必须被拒绝，实际 %v", uc, err)
		}
	}
}

func TestProjectLoginViewRejectsUnsafeVerificationURI(t *testing.T) {
	bad := []string{
		"", "http://github.com/login/device", "javascript:alert(1)",
		"https://github.com/login/device\x1b]0;pwned\x07",
		"https://github.com/login/device<script>",
		"https://github.com/login device", "https://github.com/login/\ndevice",
		"https://" + strings.Repeat("a", maxVerificationURIRunes),
		"https://github.com/login/device\\evil",
	}
	for _, uri := range bad {
		if _, err := ProjectLoginView(loginFlowStart("ABCD-EFGH", uri), loginFlowNow); !errors.Is(err, ErrUnsafeLoginView) {
			t.Errorf("非法 verification_uri %q 必须被拒绝，实际 %v", uri, err)
		}
	}
}

func TestProjectLoginViewRejectsExpiredCode(t *testing.T) {
	s := loginFlowStart("ABCD-EFGH", "https://github.com/login/device")
	s.ExpiresAt = loginFlowNow.Add(-time.Second)
	if _, err := ProjectLoginView(s, loginFlowNow); !errors.Is(err, ErrLoginViewExpired) {
		t.Fatalf("过期设备码必须被拒绝，实际 %v", err)
	}
	s.ExpiresAt = loginFlowNow // 恰好到点也视为过期（同 EvaluatePoll 口径）
	if _, err := ProjectLoginView(s, loginFlowNow); !errors.Is(err, ErrLoginViewExpired) {
		t.Fatalf("恰好到点也应视为过期，实际 %v", err)
	}
}

func TestProjectLoginViewNeverProjectsPrivateFields(t *testing.T) {
	// 结构层面：可呈现投影不得携带 device_code / token 字段。
	for _, typ := range []reflect.Type{reflect.TypeOf(LoginView{}), reflect.TypeOf(LoginDirective{})} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			if strings.Contains(name, "token") || strings.Contains(name, "devicecode") {
				t.Errorf("%s 不得含敏感字段 %s", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

func TestClampIntervalBounds(t *testing.T) {
	cases := map[int]int{
		-1:   LoginMinIntervalSeconds,
		0:    LoginMinIntervalSeconds,
		1:    LoginMinIntervalSeconds,
		4:    LoginMinIntervalSeconds,
		5:    5,
		10:   10,
		60:   60,
		61:   LoginMaxIntervalSeconds,
		9999: LoginMaxIntervalSeconds,
	}
	for in, want := range cases {
		if got := ClampInterval(in); got != want {
			t.Errorf("ClampInterval(%d) = %d，期望 %d", in, got, want)
		}
	}
}

func TestClassifyPollContinueUsesNextInterval(t *testing.T) {
	st := DeviceAuthState{Status: StatusPending, PollingInterval: 5}
	d := ClassifyPoll(PollResult{Outcome: OutcomeContinue, State: st, NextInterval: 12, Message: "响应原文不得透传"}, nil)
	if d.Kind != DirectiveContinue || d.DelaySeconds != 12 {
		t.Fatalf("continue 指令错误: %+v", d)
	}
	if d.Message != LoginTextWaiting {
		t.Fatalf("不得透传响应原文，实际 %q", d.Message)
	}
	// NextInterval 为 0 时回落到 state 的 interval。
	d = ClassifyPoll(PollResult{Outcome: OutcomeContinue, State: st}, nil)
	if d.Kind != DirectiveContinue || d.DelaySeconds != LoginMinIntervalSeconds {
		t.Fatalf("回落间隔错误: %+v", d)
	}
}

func TestClassifyPollNetworkErrorIsRetryNotDenied(t *testing.T) {
	st := DeviceAuthState{Status: StatusPending, PollingInterval: 5}
	d := ClassifyPoll(PollResult{State: st}, errors.New("dial tcp: timeout"))
	if d.Kind != DirectiveRetry {
		t.Fatalf("网络抖动不能误判为用户拒绝，实际 %s", d.Kind)
	}
	if d.DelaySeconds != LoginMinIntervalSeconds || d.Message != LoginTextRetry {
		t.Fatalf("重试指令错误: %+v", d)
	}
}

func TestClassifyPollAuthorizedCarriesNoToken(t *testing.T) {
	d := ClassifyPoll(PollResult{
		Outcome: OutcomeAuthorized,
		Token:   AccessToken{Token: "gho_secret"},
		Message: "授权成功",
	}, nil)
	if d.Kind != DirectiveAuthorized || d.Message != LoginTextAuthorizing {
		t.Fatalf("授权指令错误: %+v", d)
	}
	if d.DelaySeconds != 0 || strings.Contains(d.Message, "gho_secret") || d.Kind.String() != "authorized" {
		t.Fatalf("授权指令不得携带令牌或节奏: %+v", d)
	}
}

func TestClassifyPollTerminalAndUnknownFailClosed(t *testing.T) {
	for _, st := range []DeviceAuthStatus{StatusExpired, StatusDenied} {
		d := ClassifyPoll(PollResult{Outcome: OutcomeTerminal, State: DeviceAuthState{Status: st}, Message: "响应原文"}, nil)
		if d.Kind != DirectiveTerminal {
			t.Errorf("%s 应为终态，实际 %s", st, d.Kind)
		}
		if d.Message != TerminalLoginText(st) {
			t.Errorf("终态文案应取固定常量，实际 %q", d.Message)
		}
	}
	d := ClassifyPoll(PollResult{Outcome: PollOutcome(99)}, nil)
	if d.Kind != DirectiveTerminal || d.Message != LoginTextDenied {
		t.Fatalf("未知枚举必须失败关闭，实际 %+v", d)
	}
}

func TestTerminalLoginTextIsFixed(t *testing.T) {
	if TerminalLoginText(StatusExpired) != LoginTextExpired {
		t.Fatal("过期文案不符")
	}
	if TerminalLoginText(StatusDenied) != LoginTextDenied {
		t.Fatal("拒绝文案不符")
	}
	if TerminalLoginText(StatusAuthorized) != LoginTextDenied {
		t.Fatal("authorized 不属轮询终态路径，应回落固定拒绝文案")
	}
}
