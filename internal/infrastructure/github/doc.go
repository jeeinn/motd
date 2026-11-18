// Package github 封装 GitHub OAuth Device Flow 与用户资料查询（P0-15）。
//
// 边界：只做外部协议实现，不参与领域规则与账号判定；只暴露 Go 接口，
// 不绑定 transport，也不开放任何 HTTP 端点（Q3：P0 全程进程内调用）。
// 值对象与端口定义在 application/auth，依赖方向为 infrastructure → application。
//
// 全部用标准库实现（net/http / encoding/json），零第三方依赖。
// 端点地址可注入，开发 / 测试用 httptest 假端点，不依赖真实凭据与外网。
package github
