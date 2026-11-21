//go:build !race

package main

import "time"

// e2eWaitTimeout 是真实 SSH e2e 等待输出的上限（正常构建）。
// -race 构建见 e2e_wait_race.go：race 检测下首屏渲染显著变慢，单独放宽到 30s。
const e2eWaitTimeout = 8 * time.Second
