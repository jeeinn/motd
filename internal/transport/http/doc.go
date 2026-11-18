// Package http 承载 HTTP 入口：OAuth 回调、Device Flow 端点、运维探针。
// 边界：只做请求解析、鉴权中间件与响应编码；业务一律调用 application 层。
package http
