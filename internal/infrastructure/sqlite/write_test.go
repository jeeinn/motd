package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// P0-06 验收：容量与背压、超时与取消、事务提交后才确认、
// 可重试/不可重试分类、并发写入、停机不泄漏等待者。

func setupWriter(t *testing.T, opts ...WriterOption) (*Pools, *Writer) {
	t.Helper()
	pools := openTestPools(t)
	if _, err := Migrate(context.Background(), pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	w := pools.NewWriter(opts...)
	w.Start()
	t.Cleanup(w.Stop) // 先停队列，再关连接池（openTestPools 的 Cleanup 更早注册）
	return pools, w
}

func countNodes(ctx context.Context, pools *Pools) int {
	var n int
	_ = pools.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&n)
	return n
}

func insertNodeTx(ctx context.Context, tx *sql.Tx, slug string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO nodes (slug, name) VALUES (?, ?)`, slug, slug)
	return err
}

func TestWriterCommitsBeforeAck(t *testing.T) {
	ctx := context.Background()
	pools, w := setupWriter(t)

	err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := insertNodeTx(ctx, tx, "a"); err != nil {
			return err
		}
		// 事务尚未提交，读连接（WAL）不应看到这条数据。
		if n := countNodes(ctx, pools); n != 0 {
			return fmt.Errorf("事务未提交但数据已对外可见，count=%d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if n := countNodes(ctx, pools); n != 1 {
		t.Fatalf("Submit 返回后数据应已提交，count=%d", n)
	}
}

func TestWriterRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	pools, w := setupWriter(t)
	sentinel := errors.New("用例主动失败")

	err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := insertNodeTx(ctx, tx, "a"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应透传用例错误，实际 %v", err)
	}
	if n := countNodes(ctx, pools); n != 0 {
		t.Fatalf("失败事务应回滚，count=%d", n)
	}
}

func TestWriterQueueFull(t *testing.T) {
	_, w := setupWriter(t,
		WithQueueSize(1),
		WithWriteTimeout(30*time.Second),
		WithMaxRetries(0),
	)

	started := make(chan struct{})
	release := make(chan struct{})
	blocking := func(ctx context.Context, tx *sql.Tx) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = w.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			close(started)
			return blocking(ctx, tx)
		})
	}()
	<-started

	go func() {
		defer wg.Done()
		_ = w.Submit(context.Background(), blocking)
	}()

	// 等第二个任务确实排进队列（worker 仍被第一个任务占住）。
	deadline := time.Now().Add(2 * time.Second)
	for w.Depth() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w.Depth() != 1 {
		t.Fatalf("期望队列深度 1，实际 %d", w.Depth())
	}

	err := w.Submit(context.Background(), func(context.Context, *sql.Tx) error { return nil })
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("队列满应快速返回 ErrQueueFull，实际 %v", err)
	}

	close(release)
	wg.Wait()
}

func TestWriterTimeoutRollsBack(t *testing.T) {
	ctx := context.Background()
	pools, w := setupWriter(t, WithWriteTimeout(60*time.Millisecond), WithMaxRetries(0))

	err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := insertNodeTx(ctx, tx, "a"); err != nil {
			return err
		}
		select {
		case <-time.After(500 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时应返回 context.DeadlineExceeded，实际 %v", err)
	}
	if n := countNodes(ctx, pools); n != 0 {
		t.Fatalf("超时事务应回滚，count=%d", n)
	}
}

func TestWriterRetriesTransientThenSucceeds(t *testing.T) {
	ctx := context.Background()
	pools, w := setupWriter(t, WithMaxRetries(3), WithRetryBaseDelay(time.Millisecond))
	var attempts atomic.Int32

	err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if attempts.Add(1) <= 2 {
			return errors.New("database is locked (5) (SQLITE_BUSY): simulated")
		}
		return insertNodeTx(ctx, tx, "a")
	})
	if err != nil {
		t.Fatalf("可重试错误应最终成功，实际 %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("期望尝试 3 次，实际 %d", got)
	}
	if n := countNodes(ctx, pools); n != 1 {
		t.Fatalf("重试成功后应只有 1 行，count=%d", n)
	}
}

func TestWriterRetryExhausted(t *testing.T) {
	_, w := setupWriter(t, WithMaxRetries(2), WithRetryBaseDelay(time.Millisecond))
	var attempts atomic.Int32

	err := w.Submit(context.Background(), func(context.Context, *sql.Tx) error {
		attempts.Add(1)
		return errors.New("SQLITE_BUSY: simulated")
	})
	if !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("重试耗尽应返回 ErrRetryExhausted，实际 %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("maxRetries=2 应尝试 3 次，实际 %d", got)
	}
}

func TestWriterNonRetryableStopsImmediately(t *testing.T) {
	_, w := setupWriter(t, WithMaxRetries(3), WithRetryBaseDelay(time.Millisecond))
	var attempts atomic.Int32

	err := w.Submit(context.Background(), func(context.Context, *sql.Tx) error {
		attempts.Add(1)
		return errors.New("UNIQUE constraint failed: nodes.slug")
	})
	if err == nil || errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("不可重试错误应立即返回，实际 %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("不可重试错误不应重试，实际尝试 %d 次", got)
	}
}

func TestWriterConcurrent200(t *testing.T) {
	ctx := context.Background()
	pools, w := setupWriter(t, WithRetryBaseDelay(time.Millisecond))

	const n = 200
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
				return insertNodeTx(ctx, tx, fmt.Sprintf("node-%03d", i))
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发写入失败: %v", err)
		}
	}
	if got := countNodes(ctx, pools); got != n {
		t.Fatalf("期望 %d 行，实际 %d", n, got)
	}
}

func TestWriterNotStarted(t *testing.T) {
	pools := openTestPools(t)
	w := pools.NewWriter()
	if err := w.Submit(context.Background(), func(context.Context, *sql.Tx) error { return nil }); !errors.Is(err, ErrWriterNotStarted) {
		t.Fatalf("未 Start 应返回 ErrWriterNotStarted，实际 %v", err)
	}
}

func TestWriterStopRejectsNewTasks(t *testing.T) {
	_, w := setupWriter(t)
	w.Stop()
	err := w.Submit(context.Background(), func(context.Context, *sql.Tx) error { return nil })
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Stop 后应拒绝新任务，实际 %v", err)
	}
}

// TestWriterStopDrainsPending 验证停机时排队任务都会拿到明确结果，不会永久阻塞。
func TestWriterStopDrainsPending(t *testing.T) {
	_, w := setupWriter(t, WithWriteTimeout(30*time.Second))

	started := make(chan struct{})
	release := make(chan struct{})
	errCh := make(chan error, 2)
	stopDone := make(chan struct{})

	go func() {
		errCh <- w.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-started

	go func() {
		errCh <- w.Submit(context.Background(), func(context.Context, *sql.Tx) error { return nil })
	}()
	deadline := time.Now().Add(2 * time.Second)
	for w.Depth() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w.Depth() != 1 {
		t.Fatalf("期望有 1 个排队任务，实际 %d", w.Depth())
	}

	// 先在途任务未结束时触发 Stop，再释放阻塞，确保 loop 退出时走 drain 分支。
	go func() { w.Stop(); close(stopDone) }()
	time.Sleep(10 * time.Millisecond)
	close(release)

	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 未能返回，worker 可能泄漏")
	}

	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, ErrShuttingDown) {
				t.Fatalf("停机期任务返回了非预期错误: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("排队任务永久阻塞，drain 未生效")
		}
	}
}

func TestIsRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"busy 字符串", errors.New("database is locked (5) (SQLITE_BUSY)"), true},
		{"包装后的 busy", fmt.Errorf("wrap: %w", errors.New("SQLITE_BUSY")), true},
		{"locked 字符串", errors.New("database table is locked"), true},
		{"唯一约束", errors.New("UNIQUE constraint failed: nodes.slug"), false},
		{"上下文取消", context.Canceled, false},
		{"外键错误", errors.New("FOREIGN KEY constraint failed"), false},
	}
	for _, c := range cases {
		if got := isRetryable(c.err); got != c.want {
			t.Errorf("%s: isRetryable = %v，期望 %v", c.name, got, c.want)
		}
	}
}
