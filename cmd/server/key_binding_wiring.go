package main

import (
	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// newKeyBindingService 装配 P0-18 的公钥绑定用例。
//
// 与 ADR-020/021/022/023 同一口径：端口在 application/auth，持久化实现在
// infrastructure/sqlite；会话撤销端口的实现是 transport/ssh.Server —— cmd
// 是唯一同时能看到三者的地方。ssh_keys 表已在 0001_init.sql 建好，无新迁移。
func newKeyBindingService(pools *sqlite.Pools, writer *sqlite.Writer, revoker auth.SessionRevoker) *auth.KeyBindingService {
	return auth.NewKeyBindingService(sqlite.NewKeyBindingRepos(pools, writer), revoker)
}
