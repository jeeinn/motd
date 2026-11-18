package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

const (
	defaultDeviceCodeURL = "https://github.com/login/device/code"
	defaultTokenURL      = "https://github.com/login/oauth/access_token"
	defaultAPIBaseURL    = "https://api.github.com"

	// maxResponseBytes 限制单次响应读取上限，防止异常 / 恶意大响应打爆进程内存。
	maxResponseBytes = 1 << 20 // 1 MiB
	// requestTimeout 是单次请求的硬上限，不依赖调用方传入的 http.Client 是否设了 Timeout。
	requestTimeout = 10 * time.Second

	// Device Flow 两个端点默认返回 form-urlencoded，必须显式要 JSON；
	// REST API（/user、/user/emails）官方推荐 application/vnd.github+json。
	// 两者不能混用，见 ADR-021 / §7 R14。
	acceptJSON   = "application/json"
	acceptGitHub = "application/vnd.github+json"
	formType     = "application/x-www-form-urlencoded"
)

// Endpoints 把 4 个端点地址抽成可注入配置：生产指向 github.com / api.github.com，
// 测试指向 httptest.Server。Q3 约束下客户端只暴露 Go 接口，不绑定 transport，
// 也不开放任何 HTTP 端点。
type Endpoints struct {
	DeviceCodeURL string
	TokenURL      string
	APIBaseURL    string
}

func DefaultEndpoints() Endpoints {
	return Endpoints{
		DeviceCodeURL: defaultDeviceCodeURL,
		TokenURL:      defaultTokenURL,
		APIBaseURL:    defaultAPIBaseURL,
	}
}

func (e Endpoints) withDefaults() Endpoints {
	d := DefaultEndpoints()
	if v := strings.TrimSpace(e.DeviceCodeURL); v != "" {
		d.DeviceCodeURL = v
	}
	if v := strings.TrimSpace(e.TokenURL); v != "" {
		d.TokenURL = v
	}
	if v := strings.TrimSpace(e.APIBaseURL); v != "" {
		d.APIBaseURL = v
	}
	return d
}

// Client 实现 auth.DeviceFlowClient。只依赖标准库，零第三方依赖。
type Client struct {
	clientID string
	http     *http.Client
	eps      Endpoints
}

var _ auth.DeviceFlowClient = (*Client)(nil)

// New 构造客户端。
//
// clientID 允许为空（本机开发 / httptest 测试不需要真实凭据），但每次请求都会
// 返回 auth.ErrClientIDMissing，绝不静默发出 client_id 为空的请求，也不 panic。
// httpClient 为 nil 时使用带超时的默认客户端；端点为零值时回落到官方地址。
func New(clientID string, httpClient *http.Client, eps Endpoints) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{
		clientID: strings.TrimSpace(clientID),
		http:     httpClient,
		eps:      eps.withDefaults(),
	}
}

// RequestDeviceCode 对应 POST /login/device/code（官方文档 Step 1）。
func (c *Client) RequestDeviceCode(ctx context.Context) (auth.DeviceCode, error) {
	form := url.Values{
		"client_id": {c.clientID},
		"scope":     {auth.DeviceScope},
	}
	body, err := c.postForm(ctx, "device/code", c.eps.DeviceCodeURL, form)
	if err != nil {
		return auth.DeviceCode{}, err
	}
	var raw struct {
		DeviceCode      string  `json:"device_code"`
		UserCode        string  `json:"user_code"`
		VerificationURI string  `json:"verification_uri"`
		ExpiresIn       flexInt `json:"expires_in"`
		Interval        flexInt `json:"interval"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return auth.DeviceCode{}, fmt.Errorf("解析 device/code 响应失败（需 Accept: %s 返回 JSON）: %w", acceptJSON, err)
	}
	if raw.DeviceCode == "" || raw.UserCode == "" || raw.VerificationURI == "" {
		return auth.DeviceCode{}, errors.New("device/code 响应缺少 device_code / user_code / verification_uri")
	}
	return auth.DeviceCode{
		DeviceCode:      raw.DeviceCode,
		UserCode:        raw.UserCode,
		VerificationURI: raw.VerificationURI,
		ExpiresIn:       int(raw.ExpiresIn),
		Interval:        int(raw.Interval),
	}, nil
}

// PollAccessToken 对应 POST /login/oauth/access_token（官方文档 Step 3）。
// 按 §7 R1：Device Flow 换 token 不需要 client_secret，请求体只有三个参数。
func (c *Client) PollAccessToken(ctx context.Context, deviceCode string) (auth.AccessToken, error) {
	dc := strings.TrimSpace(deviceCode)
	if dc == "" {
		return auth.AccessToken{}, errors.New("设备码为空，无法轮询")
	}
	form := url.Values{
		"client_id":   {c.clientID},
		"device_code": {dc},
		"grant_type":  {auth.DeviceGrantType},
	}
	body, err := c.postForm(ctx, "access_token", c.eps.TokenURL, form)
	if err != nil {
		return auth.AccessToken{}, err
	}
	var raw struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return auth.AccessToken{}, fmt.Errorf("解析 access_token 响应失败: %w", err)
	}
	if raw.AccessToken == "" {
		return auth.AccessToken{}, errors.New("access_token 响应缺少令牌")
	}
	return auth.AccessToken{Token: raw.AccessToken, TokenType: raw.TokenType, Scope: raw.Scope}, nil
}

// GetUser 对应 GET /user。id 必须是合法数字，作 §4.3 的身份主键。
func (c *Client) GetUser(ctx context.Context, token string) (auth.GitHubUser, error) {
	body, err := c.getJSON(ctx, "/user", "/user", token)
	if err != nil {
		return auth.GitHubUser{}, err
	}
	var raw struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return auth.GitHubUser{}, fmt.Errorf("解析 /user 响应失败: %w", err)
	}
	if raw.ID <= 0 {
		return auth.GitHubUser{}, errors.New("/user 响应缺少合法数字 id（身份主键）")
	}
	return auth.GitHubUser{ID: raw.ID, Login: raw.Login, Email: raw.Email}, nil
}

// GetEmails 对应 GET /user/emails。per_page=100 取满单页上限；
// P0 不实现分页（P1 前置，见 ADR-021 边界）。
func (c *Client) GetEmails(ctx context.Context, token string) ([]auth.GitHubEmail, error) {
	body, err := c.getJSON(ctx, "/user/emails", "/user/emails?per_page=100", token)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 /user/emails 响应失败: %w", err)
	}
	out := make([]auth.GitHubEmail, 0, len(raw))
	for _, e := range raw {
		if e.Email == "" {
			continue
		}
		out = append(out, auth.GitHubEmail{Email: e.Email, Primary: e.Primary, Verified: e.Verified})
	}
	return out, nil
}

// postForm 发送 Device Flow 的两个表单 POST，并统一处理错误码。
//
// 注意顺序：先解析 error 字段，再判 HTTP 状态。GitHub 的 token 端点常以
// HTTP 200 + {"error": "authorization_pending"} 返回，device_flow_disabled
// 也可能出现在第一步；若先按状态码分支就会把这些错误误判成网络错误。
func (c *Client) postForm(ctx context.Context, endpoint, target string, form url.Values) ([]byte, error) {
	if c.clientID == "" {
		return nil, auth.ErrClientIDMissing
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("构造 %s 请求失败: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", formType)
	req.Header.Set("Accept", acceptJSON)

	body, status, err := c.execute(req, endpoint)
	if err != nil {
		return nil, err
	}
	if pe := parseDeviceError(body, endpoint); pe != nil {
		return nil, pe
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("GitHub %s 返回 HTTP %d", endpoint, status)
	}
	return body, nil
}

// getJSON 发送带 Bearer token 的 REST GET。错误消息不携带响应体原文，
// 避免 GitHub 在错误响应里回显的请求细节 / 令牌片段进入日志或终端。
func (c *Client) getJSON(ctx context.Context, endpoint, path, token string) ([]byte, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("access_token 为空，无法查询 GitHub 用户资料")
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	target := strings.TrimRight(c.eps.APIBaseURL, "/") + path
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 %s 请求失败: %w", endpoint, err)
	}
	req.Header.Set("Accept", acceptGitHub)
	req.Header.Set("Authorization", "Bearer "+token)

	body, status, err := c.execute(req, endpoint)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("GitHub %s 返回 HTTP %d（token 可能无效或 scope 不足）", endpoint, status)
	}
	return body, nil
}

func (c *Client) execute(req *http.Request, endpoint string) ([]byte, int, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("请求 GitHub %s 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()
	// 多读 1 字节用于判定是否超限，避免把截断内容当成合法 JSON 解析。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取 GitHub %s 响应失败: %w", endpoint, err)
	}
	if len(data) > maxResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("GitHub %s 响应体超过 %d 字节上限，已丢弃", endpoint, maxResponseBytes)
	}
	return data, resp.StatusCode, nil
}

type deviceErrorPayload struct {
	Error            string  `json:"error"`
	ErrorDescription string  `json:"error_description"`
	Interval         flexInt `json:"interval"`
}

// parseDeviceError 只对「JSON 且含非空 error 字段」的响应生效；
// 成功响应或非 JSON 响应原样交给调用方，不误判。
func parseDeviceError(body []byte, endpoint string) *auth.PollError {
	var p deviceErrorPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Error == "" {
		return nil
	}
	return auth.ClassifyDeviceError(p.Error, endpoint, int(p.Interval))
}

// flexInt 兼容 GitHub 把数值字段写成 JSON number 或 string 两种情况；
// 非法值按 0 处理（slow_down 无 interval 时由 auth.NextInterval 兜底 +5 秒）。
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt(v)
	return nil
}
