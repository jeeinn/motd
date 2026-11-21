//go:build race

package main

import "time"

// e2eWaitTimeout 是真实 SSH e2e 等待输出的上限。
// race 检测下 SSH 握手 + 首屏渲染显著变慢（CI 实测初始列表偶发超过 8s），
// 因此单独放宽；正常构建仍用 8s 保持挂起检测的灵敏度（见 e2e_wait_default.go）。
const e2eWaitTimeout = 30 * time.Second
