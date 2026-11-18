package main

import (
	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// newDeviceFlowService 装配 P0-16 的 Device Flow 状态机。
//
// 与 ADR-020/021 同一口径：端口在 application/auth，持久化实现在
// infrastructure/sqlite，GitHub 协议客户端由 P0-15 注入；cmd 是唯一同时
// 能看到三者的地方。状态机内部不 sleep，节奏交给 P0-19 的会话循环。
func newDeviceFlowService(pools *sqlite.Pools, writer *sqlite.Writer, client auth.DeviceFlowClient) *auth.DeviceFlowService {
	return auth.NewDeviceFlowService(sqlite.NewDeviceAuthRepos(pools, writer), client)
}
