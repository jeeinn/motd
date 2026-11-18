// Package email 封装事务邮件投递（验证码、恢复链接，P1 启用）。
// 边界：只做投递，不保存验证码状态（状态在 email_tokens）。
package email
