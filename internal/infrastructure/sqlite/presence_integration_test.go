package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/jeeinn/motd/internal/application/presence"
)

// P0-13 验收项：100 个模拟连接上下线，写库次数为 0。
//
// 防假绿：先做一次真实写入让 Writer.Commits() 变成 1，再驱动 presence；
// 若计数器本身不工作，这条测试会立刻失败，而不是恒真地报「0 次写库」。
func TestPresenceDoesNotWriteSQLite(t *testing.T) {
	ctx := context.Background()
	_, writer := setupWriter(t)

	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (slug, name) VALUES (?, ?)`, "/tmp/presence-probe", "probe")
		return err
	}); err != nil {
		t.Fatalf("基线写入失败: %v", err)
	}
	baseline := writer.Commits()
	if baseline != 1 {
		t.Fatalf("提交计数器应记 1 次，实际 %d（计数器失效会让本测试假绿）", baseline)
	}

	reg := presence.New()
	const n = 100
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("s%03d", i)
		if err := reg.Connect(id, presence.KindSSH, int64(i+1)); err != nil {
			t.Fatalf("连接 %s: %v", id, err)
		}
		// 每个会话都做心跳 + 状态切换 + 一次 Reap 检查，覆盖全部高频状态操作。
		if err := reg.Touch(id); err != nil {
			t.Fatal(err)
		}
		if err := reg.SetState(id, presence.StateBrowsing); err != nil {
			t.Fatal(err)
		}
		_ = reg.Reap()
	}
	if reg.Online() != n || reg.Active() != n {
		t.Fatalf("连接状态错误: online=%d active=%d", reg.Online(), reg.Active())
	}
	for i := 0; i < n; i++ {
		reg.Disconnect(fmt.Sprintf("s%03d", i))
	}
	if reg.Online() != 0 || reg.Peak() != n {
		t.Fatalf("下线后状态错误: online=%d peak=%d", reg.Online(), reg.Peak())
	}

	if got := writer.Commits(); got != baseline {
		t.Fatalf("在线状态维护产生了 %d 次写库（基线 %d）", got-baseline, baseline)
	}
}

// 提交计数器自身的回归：只有 Commit 成功才 +1，失败与约束冲突都不计。
func TestWriterCommitsOnlySuccessfulTransactions(t *testing.T) {
	ctx := context.Background()
	_, writer := setupWriter(t)
	if writer.Commits() != 0 {
		t.Fatal("新 Writer 的提交数应为 0")
	}
	insert := func(slug string) error {
		return writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO nodes (slug, name) VALUES (?, ?)`, slug, slug)
			return err
		})
	}
	if err := insert("/tmp/commits-ok"); err != nil {
		t.Fatal(err)
	}
	if writer.Commits() != 1 {
		t.Fatalf("成功提交应计 1 次，实际 %d", writer.Commits())
	}

	sentinel := errors.New("主动失败")
	if err := writer.Submit(ctx, func(context.Context, *sql.Tx) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("失败事务应返回错误: %v", err)
	}
	if err := insert("/tmp/commits-ok"); err == nil {
		t.Fatal("唯一约束冲突应失败")
	}
	if writer.Commits() != 1 {
		t.Fatalf("失败/冲突事务不应增加提交数，实际 %d", writer.Commits())
	}
}
