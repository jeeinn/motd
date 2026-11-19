// Command repro_auth_limit 是 P1-M2 的本地复现驱动：
// 在调用方给出的隔离 SQLite 库上反复触发失败认证，断言
//  1. 超过阈值后返回带 RetryAfter 的明确拒绝；
//  2. auth_events 行数确实增长（审计异步批量落库生效）；
//  3. 审计环溢出时认证动作 fail-closed。
//
// 它只接受显式 -db 路径，绝不读取生产 MOTD_DATA_DIR / ./data。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	db := flag.String("db", "", "隔离数据库路径（必填，禁止指向生产 data/）")
	flag.Parse()
	if strings.TrimSpace(*db) == "" {
		fail("必须显式传 -db <隔离临时路径>，拒绝使用默认 ./data")
	}
	if strings.Contains(*db, "data"+string(os.PathSeparator)) || *db == "./data/motd.db" {
		fail("拒绝在 data/ 目录下运行，避免碰到生产库")
	}

	ctx := context.Background()
	pools, err := sqlite.Open(ctx, *db)
	if err != nil {
		fail("打开隔离库: %v", err)
	}
	defer pools.Close()
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		fail("迁移隔离库: %v", err)
	}
	writer := pools.NewWriter()
	writer.Start()
	defer writer.Stop()

	const maxAttempts = 3
	limiter := auth.NewRateLimiter(auth.WithRateLimitConfig(auth.RateLimitConfig{
		MaxAttempts: maxAttempts,
		Window:      time.Minute,
		Lockout:     2 * time.Minute,
	}))
	audit := auth.NewAuditLogger(sqlite.NewAuthEventStore(writer), auth.WithAuditCapacity(16))
	gate := auth.NewSecurityGate(limiter, audit, auth.ProviderGitHub)

	// 连续 5 次失败认证：前 3 次只留审计，第 4 次起被限流拒绝。
	accepted, rejected := 0, 0
	var lastRetry time.Duration
	for i := 1; i <= 5; i++ {
		err := gate.NoteLoginFailure("repro-ip-hash", "")
		var rl *auth.RateLimitError
		switch {
		case err == nil:
			accepted++
		case errors.As(err, &rl):
			rejected++
			lastRetry = rl.RetryAfter
		default:
			fail("第 %d 次失败认证返回了非限流错误: %v", i, err)
		}
		fmt.Printf("attempt %d: %s\n", i, describe(err))
	}
	if accepted != maxAttempts || rejected != 2 {
		fail("限流阈值不符: accepted=%d rejected=%d（期望 %d/2）", accepted, rejected, maxAttempts)
	}
	if lastRetry <= 0 {
		fail("超限拒绝必须携带 RetryAfter")
	}

	// 审计异步落库：Tick 一次后核对 auth_events 行数。
	audit.Tick(ctx)
	n, err := pools.CountAuthEvents(ctx, auth.AuditLoginFailure)
	if err != nil {
		fail("统计 auth_events: %v", err)
	}
	if n != 5 {
		fail("auth_events 应增长到 5 行，实际 %d", n)
	}
	depth, dropped, flushed, _ := audit.Stats()
	if depth != 0 || dropped != 0 || flushed != 5 {
		fail("审计器状态不符: depth=%d dropped=%d flushed=%d", depth, dropped, flushed)
	}

	// 溢出 fail-closed 单独验证（不影响上面的落库断言）。
	small := auth.NewAuditLogger(sqlite.NewAuthEventStore(writer), auth.WithAuditCapacity(1))
	if err := small.Record(auth.AuditLogout, 1, "github", "repro-ip-hash", `{}`); err != nil {
		fail("审计环第 1 条应成功: %v", err)
	}
	if err := small.Record(auth.AuditLogout, 1, "github", "repro-ip-hash", `{}`); !errors.Is(err, auth.ErrAuditQueueFull) {
		fail("审计环第 2 条应 fail-closed，实际 %v", err)
	}
	_, dropped, _, _ = small.Stats()
	if dropped != 1 {
		fail("溢出 dropped 计数应为 1，实际 %d", dropped)
	}

	fmt.Printf("PASS: accepted=%d rejected=%d retry_after=%s auth_events=%d dropped=%d\n",
		accepted, rejected, lastRetry.Round(time.Second), n, dropped)
}

func describe(err error) string {
	if err == nil {
		return "accepted"
	}
	var rl *auth.RateLimitError
	if errors.As(err, &rl) {
		return fmt.Sprintf("rejected: %s", err)
	}
	return "error: " + err.Error()
}
