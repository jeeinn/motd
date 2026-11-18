// Package ssh 承载 SSH 入口：连接建立、公钥认证、PTY 分配、窗口尺寸变化与输入输出。
// 边界：只做协议适配，把输入归一化为统一的 KeyMsg，把 ViewModel 交给 presentation/terminal 渲染；
// 不包含业务规则，不直接访问数据库。
package ssh
