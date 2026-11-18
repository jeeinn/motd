package main

import (
	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/config"
	githubclient "github.com/jeeinn/motd/internal/infrastructure/github"
)

// newDeviceFlowClient 装配 P0-15 的 GitHub Device Flow 客户端。
//
// 抽到独立文件是为了让 cmd 的 import 面保持清晰：main.go 只依赖
// auth.DeviceFlowClient 端口，HTTP 实现细节由 infrastructure/github 承担。
// 换 token 不需要 client_secret（§7 R1），凭据只从 MOTD_GITHUB_CLIENT_ID 读取。
func newDeviceFlowClient(cfg config.Config) auth.DeviceFlowClient {
	return githubclient.New(cfg.GitHubClientID, nil, githubclient.DefaultEndpoints())
}
