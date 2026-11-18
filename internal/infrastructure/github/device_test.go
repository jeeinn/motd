package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
)

const testClientID = "Iv1.testclient"

// endpointsFor 把 4 个端点全部指向 httptest 假服务，保证测试零外网依赖（Q3 / OPS §3）。
func endpointsFor(base string) Endpoints {
	return Endpoints{
		DeviceCodeURL: base + "/login/device/code",
		TokenURL:      base + "/login/oauth/access_token",
		APIBaseURL:    base,
	}
}

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(testClientID, srv.Client(), endpointsFor(srv.URL))
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestRequestDeviceCodeRequestShape(t *testing.T) {
	var path, method, accept, contentType, body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		accept = r.Header.Get("Accept")
		contentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		writeJSON(w, 200, `{"device_code":"dc123","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
	})

	dc, err := c.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("RequestDeviceCode: %v", err)
	}
	if path != "/login/device/code" || method != http.MethodPost {
		t.Errorf("端点 = %s %s，期望 POST /login/device/code", method, path)
	}
	if accept != acceptJSON {
		t.Errorf("Accept 必须为 %s 才能拿到 JSON，实际 %q", acceptJSON, accept)
	}
	if contentType != formType {
		t.Errorf("Content-Type = %q，期望 %q", contentType, formType)
	}
	form, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("解析请求体: %v", err)
	}
	if form.Get("client_id") != testClientID {
		t.Errorf("client_id = %q", form.Get("client_id"))
	}
	if form.Get("scope") != auth.DeviceScope {
		t.Errorf("scope = %q，期望最小集 %q", form.Get("scope"), auth.DeviceScope)
	}
	if strings.Contains(body, "client_secret") {
		t.Error("Device Flow 换 token 不需要 client_secret（§7 R1），请求体不得出现该参数")
	}
	if len(form) != 2 {
		t.Errorf("请求体只应有 client_id 与 scope 两个参数，实际 %v", form)
	}
	if dc.DeviceCode != "dc123" || dc.UserCode != "WDJB-MJHT" || dc.ExpiresIn != 900 || dc.PollInterval() != 5 {
		t.Errorf("响应字段映射错误: %+v", dc)
	}
}

func TestPollAccessTokenRequestShape(t *testing.T) {
	var path, accept, body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, accept = r.URL.Path, r.Header.Get("Accept")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		writeJSON(w, 200, `{"access_token":"gho_abc","token_type":"bearer","scope":"read:user,user:email"}`)
	})

	tok, err := c.PollAccessToken(context.Background(), "dc-from-step1")
	if err != nil {
		t.Fatalf("PollAccessToken: %v", err)
	}
	if path != "/login/oauth/access_token" {
		t.Errorf("端点 = %q", path)
	}
	if accept != acceptJSON {
		t.Errorf("Accept = %q，期望 %q", accept, acceptJSON)
	}
	form, _ := url.ParseQuery(body)
	if form.Get("grant_type") != auth.DeviceGrantType {
		t.Errorf("grant_type = %q，期望 %q", form.Get("grant_type"), auth.DeviceGrantType)
	}
	if form.Get("device_code") != "dc-from-step1" || form.Get("client_id") != testClientID {
		t.Errorf("device_code / client_id 错误: %v", form)
	}
	if strings.Contains(body, "client_secret") || len(form) != 3 {
		t.Errorf("请求体只能有 3 个参数且不得含 client_secret，实际 %v", form)
	}
	if !tok.Valid() || tok.TokenType != "bearer" || tok.Scope != "read:user,user:email" {
		t.Errorf("令牌映射错误: %+v", tok)
	}
}

// 8 个官方错误码 + token_expired 别名，逐一验证分类与可读消息。
func TestPollAccessTokenErrorCodes(t *testing.T) {
	cases := []struct {
		code   string
		status auth.PollStatus
	}{
		{"authorization_pending", auth.PollPending},
		{"slow_down", auth.PollSlowDown},
		{"expired_token", auth.PollExpired},
		{"token_expired", auth.PollExpired},
		{"access_denied", auth.PollTerminal},
		{"device_flow_disabled", auth.PollTerminal},
		{"incorrect_client_credentials", auth.PollTerminal},
		{"incorrect_device_code", auth.PollTerminal},
		{"unsupported_grant_type", auth.PollTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 200, fmt.Sprintf(`{"error":%q,"error_description":"ignored","interval":10}`, tc.code))
			})
			_, err := c.PollAccessToken(context.Background(), "dc")
			if err == nil {
				t.Fatal("错误码响应必须返回 error")
			}
			var pe *auth.PollError
			if !errors.As(err, &pe) {
				t.Fatalf("错误必须能被 errors.As 取回，实际 %T: %v", err, err)
			}
			if pe.Status != tc.status {
				t.Errorf("分类 = %s，期望 %s", pe.Status, tc.status)
			}
			if pe.Code != tc.code || pe.Endpoint != "access_token" {
				t.Errorf("错误码 / 端点应原样保留，实际 %q / %q", pe.Code, pe.Endpoint)
			}
			if pe.Message == "" {
				t.Error("必须有可读中文提示")
			}
			if pe.Interval != 10 {
				t.Errorf("slow_down 的新 interval 应被带出，实际 %d", pe.Interval)
			}
		})
	}
}

func TestDeviceCodeEndpointErrors(t *testing.T) {
	// device_flow_disabled 通常出现在第一步；即使 HTTP 200 也必须分类。
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"error":"device_flow_disabled"}`)
	})
	_, err := c.RequestDeviceCode(context.Background())
	var pe *auth.PollError
	if !errors.As(err, &pe) || pe.Status != auth.PollTerminal {
		t.Fatalf("第一步的 device_flow_disabled 应终止，实际 %v", err)
	}
	if !strings.Contains(pe.Message, "启用") {
		t.Errorf("提示应指导用户去启用 Device Flow，实际 %q", pe.Message)
	}

	// 4xx + error 字段同样先按错误码分类，不能被当成网络错误吞掉。
	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, `{"error":"incorrect_client_credentials"}`)
	})
	_, err = c2.RequestDeviceCode(context.Background())
	if !errors.As(err, &pe) || pe.Status != auth.PollTerminal {
		t.Fatalf("4xx 带错误码也应分类，实际 %v", err)
	}
}

func TestNon2xxWithoutErrorCode(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 502, "upstream-secret-should-not-leak")
	})
	_, err := c.RequestDeviceCode(context.Background())
	if err == nil {
		t.Fatal("HTTP 502 必须报错")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("错误应带状态码，实际 %q", err.Error())
	}
	if strings.Contains(err.Error(), "upstream-secret") {
		t.Error("错误消息不得携带响应体原文")
	}
}

func TestGetUser(t *testing.T) {
	var path, accept, authorization string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, accept, authorization = r.URL.Path, r.Header.Get("Accept"), r.Header.Get("Authorization")
		writeJSON(w, 200, `{"id":1234567,"login":"octocat","email":"octocat@github.com"}`)
	})
	u, err := c.GetUser(context.Background(), "gho_abc")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if path != "/user" || accept != acceptGitHub || authorization != "Bearer gho_abc" {
		t.Errorf("请求头错误: path=%q accept=%q auth=%q", path, accept, authorization)
	}
	if u.ID != 1234567 || u.Login != "octocat" || u.Email != "octocat@github.com" {
		t.Errorf("用户投影错误: %+v", u)
	}
}

func TestGetUserRejectsMissingID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"login":"octocat"}`)
	})
	if _, err := c.GetUser(context.Background(), "gho_abc"); err == nil {
		t.Fatal("缺少合法数字 id 必须报错（§4.3 身份主键）")
	}
}

func TestGetEmails(t *testing.T) {
	var path, query, authorization string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, query, authorization = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		writeJSON(w, 200, `[
			{"email":"primary@example.com","primary":true,"verified":true,"visibility":"public"},
			{"email":"second@example.com","primary":false,"verified":true},
			{"email":"","primary":false,"verified":false}
		]`)
	})
	list, err := c.GetEmails(context.Background(), "gho_abc")
	if err != nil {
		t.Fatalf("GetEmails: %v", err)
	}
	if path != "/user/emails" || !strings.Contains(query, "per_page=100") || authorization != "Bearer gho_abc" {
		t.Errorf("请求错误: path=%q query=%q auth=%q", path, query, authorization)
	}
	if len(list) != 2 {
		t.Fatalf("空 email 应被过滤，实际 %d 条: %+v", len(list), list)
	}
	if !list[0].Primary || !list[0].Verified || list[0].Email != "primary@example.com" {
		t.Errorf("首项字段错误: %+v", list[0])
	}
	if list[1].Primary {
		t.Errorf("第二项不应是 primary: %+v", list[1])
	}
}

func TestGetJSONRequiresToken(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(w, 200, `{}`)
	})
	if _, err := c.GetUser(context.Background(), "  "); err == nil {
		t.Error("空 token 必须报错")
	}
	if _, err := c.GetEmails(context.Background(), ""); err == nil {
		t.Error("空 token 必须报错")
	}
	if requests != 0 {
		t.Errorf("缺 token 时不应发出任何请求，实际 %d 次", requests)
	}
}

func TestMissingClientID(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(w, 200, `{}`)
	}))
	defer srv.Close()
	c := New("", srv.Client(), endpointsFor(srv.URL))

	if _, err := c.RequestDeviceCode(context.Background()); !errors.Is(err, auth.ErrClientIDMissing) {
		t.Errorf("RequestDeviceCode 应返回 ErrClientIDMissing，实际 %v", err)
	}
	if _, err := c.PollAccessToken(context.Background(), "dc"); !errors.Is(err, auth.ErrClientIDMissing) {
		t.Errorf("PollAccessToken 应返回 ErrClientIDMissing，实际 %v", err)
	}
	if requests != 0 {
		t.Errorf("缺 client_id 时绝不发出请求，实际 %d 次", requests)
	}
	// GET /user 与 /user/emails 只需要 token，不受 client_id 缺失影响：
	// 假端点返回空对象会触发各自的解析错误，但绝不能是 ErrClientIDMissing。
	if _, err := c.GetUser(context.Background(), "gho_abc"); errors.Is(err, auth.ErrClientIDMissing) {
		t.Error("GetUser 不应依赖 client_id")
	}
	if _, err := c.GetEmails(context.Background(), "gho_abc"); errors.Is(err, auth.ErrClientIDMissing) {
		t.Error("GetEmails 不应依赖 client_id")
	}
}

func TestResponseBodyLimit(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxResponseBytes+100))
	})
	_, err := c.RequestDeviceCode(context.Background())
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超大响应必须被拒绝，实际 %v", err)
	}
}

// GitHub 两个 Device Flow 端点默认返回 form-urlencoded；若没设 Accept: application/json
// 就必须显式报错，而不是把表单文本当成成功响应。
func TestNonJSONResponseIsRejected(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "device_code=dc123&expires_in=900&interval=5&user_code=WDJB-MJHT")
	})
	_, err := c.RequestDeviceCode(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Accept") {
		t.Fatalf("非 JSON 响应必须提示 Accept 头问题，实际 %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.RequestDeviceCode(ctx); err == nil {
		t.Fatal("已取消的 context 必须立即返回错误，不能挂死")
	}
}

func TestEndpointsDefaultWhenEmpty(t *testing.T) {
	c := New(testClientID, nil, Endpoints{})
	d := DefaultEndpoints()
	if c.eps.DeviceCodeURL != d.DeviceCodeURL || c.eps.TokenURL != d.TokenURL || c.eps.APIBaseURL != d.APIBaseURL {
		t.Errorf("零值端点应回落官方地址，实际 %+v", c.eps)
	}
	if c.http.Timeout != requestTimeout {
		t.Errorf("nil http.Client 应带默认超时，实际 %v", c.http.Timeout)
	}
}

func TestFlexInt(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`5`, 5},
		{`"12"`, 12},
		{`null`, 0},
		{`"oops"`, 0},
		{`0`, 0},
	}
	for _, tc := range cases {
		var f flexInt
		if err := json.Unmarshal([]byte(tc.raw), &f); err != nil {
			t.Errorf("flexInt(%s) 解析失败: %v", tc.raw, err)
		} else if int(f) != tc.want {
			t.Errorf("flexInt(%s) = %d，期望 %d", tc.raw, int(f), tc.want)
		}
	}
}

// 验收项「轮询间隔不得小于 interval」的端到端串联：真实走
// pending → slow_down(新 interval=20) → 成功，用 auth.NextInterval 驱动节奏。
func TestPollingSequenceRespectsInterval(t *testing.T) {
	step := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		step++
		switch step {
		case 1:
			writeJSON(w, 200, `{"error":"authorization_pending"}`)
		case 2:
			writeJSON(w, 200, `{"error":"slow_down","interval":20}`)
		default:
			writeJSON(w, 200, `{"access_token":"gho_ok","token_type":"bearer"}`)
		}
	})

	interval := (auth.DeviceCode{Interval: 5}).PollInterval()
	for i := 0; i < 3; i++ {
		prev := interval
		_, err := c.PollAccessToken(context.Background(), "dc")
		interval = auth.NextInterval(interval, err)
		if interval < prev {
			t.Fatalf("第 %d 轮间隔缩小：%d → %d", i+1, prev, interval)
		}
		if interval < auth.DefaultPollInterval {
			t.Fatalf("第 %d 轮间隔低于官方下限", i+1)
		}
	}
	if interval != 20 {
		t.Fatalf("slow_down 后应保持 20 秒间隔，实际 %d", interval)
	}
	tok, err := c.PollAccessToken(context.Background(), "dc")
	if err != nil || !tok.Valid() {
		t.Fatalf("第三次轮询应成功拿到 token，实际 %+v / %v", tok, err)
	}
}
