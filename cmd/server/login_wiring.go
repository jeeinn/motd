package main

import (
	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// newLoginService 装配 P0-17 的账号创建与登录事务。
//
// 与 ADR-020/021/022 同一口径：端口在 application/auth，持久化实现在
// infrastructure/sqlite，GitHub 协议客户端复用 P0-15 的 DeviceFlowClient；
// cmd 是唯一同时能看到三者的地方。§4.3 三步判定与 users+identities 插入
// 都在同一个写事务内，GetUser / GetEmails 在事务外（ADR-022 边界）。
func newLoginService(writer *sqlite.Writer, client auth.DeviceFlowClient) *auth.LoginService {
	return auth.NewLoginService(client, sqlite.NewLoginRepos(writer))
}
