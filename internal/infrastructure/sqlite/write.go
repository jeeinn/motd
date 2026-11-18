// 写入队列与事务边界（评审稿 v0.2 §2.6 / P0-06）。
//
// 单进程里只有一个 SQLite 写连接，所有业务写入都排进这条 chan WriteTask，
// 由唯一 worker 串行执行。必须守住的边界：
//   - 队列有容量上限，满时快速失败，不无限阻塞；
//   - 每次写入受 context 超时与取消约束；
//   - 一个业务用例对应一个事务，事务提交后才向调用方确认成功；
//   - 区分可重试（SQLITE_BUSY / SQLITE_LOCKED）与不可重试错误，重试有上限。
//
// 高频状态（在线人数、心跳）不走这里，见 P0-13 内存连接注册表。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrQueueFull 表示队列已满，调用方应稍后重试或降级，不应等待。
	ErrQueueFull = errors.New("写入队列已满")
	// ErrShuttingDown 表示写入队列已关闭，新任务不再受理。
	ErrShuttingDown = errors.New("写入队列已关闭")
	// ErrWriterNotStarted 表示尚未调用 Start。
	ErrWriterNotStarted = errors.New("写入队列未启动")
	// ErrRetryExhausted 表示可重试错误用尽重试次数。
	ErrRetryExhausted = errors.New("写入重试次数耗尽")
)

// TxFunc 是一个业务用例的写入单元。它拿到的 *sql.Tx 由 Writer 管理，
// 用例不得自行 Commit / Rollback，也不得跨用例复用。
type TxFunc func(context.Context, *sql.Tx) error

const (
	defaultQueueSize    = 256
	defaultWriteTimeout = 3 * time.Second
	defaultMaxRetries   = 3
	defaultRetryBase    = 10 * time.Millisecond
)

type writerConfig struct {
	queueSize  int
	timeout    time.Duration
	maxRetries int
	retryBase  time.Duration
}

// WriterOption 用于调整写入队列参数（测试与压测时可缩小容量以复现背压）。
type WriterOption func(*writerConfig)

// WithQueueSize 设置队列容量，<=0 时回落到 256。
func WithQueueSize(n int) WriterOption {
	return func(c *writerConfig) { c.queueSize = n }
}

// WithWriteTimeout 设置单次写入的超时，<=0 时回落到 3s。
func WithWriteTimeout(d time.Duration) WriterOption {
	return func(c *writerConfig) { c.timeout = d }
}

// WithMaxRetries 设置可重试错误的最大重试次数（不含首次尝试），<0 视为 0。
func WithMaxRetries(n int) WriterOption {
	return func(c *writerConfig) { c.maxRetries = n }
}

// WithRetryBaseDelay 设置退避基数，实际间隔为 base * 2^attempt。
func WithRetryBaseDelay(d time.Duration) WriterOption {
	return func(c *writerConfig) { c.retryBase = d }
}

type writeTask struct {
	ctx  context.Context
	fn   TxFunc
	done chan error // 容量 1，worker 回写结果时永不阻塞
}

// Writer 是 application 层唯一的写入门面。
// 它不暴露 *sql.DB：所有写必须经 Submit 携带 TxFunc，事务边界由这里统一把守。
type Writer struct {
	db    *sql.DB
	tasks chan writeTask
	cfg   writerConfig

	mu      sync.Mutex
	started bool
	closed  bool
	stopCh  chan struct{}
	wg      sync.WaitGroup

	// commits 只统计成功提交的事务，用于运维观测与 P0-13 的「在线状态零写库」验收。
	// 放在 Writer 内部而不是外部计数器，是因为所有写入都必经这里。
	commits atomic.Int64
}

// NewWriter 基于写池构造队列。创建后必须显式 Start 才会消费任务。
func (p *Pools) NewWriter(opts ...WriterOption) *Writer {
	cfg := writerConfig{
		queueSize:  defaultQueueSize,
		timeout:    defaultWriteTimeout,
		maxRetries: defaultMaxRetries,
		retryBase:  defaultRetryBase,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.queueSize <= 0 {
		cfg.queueSize = defaultQueueSize
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultWriteTimeout
	}
	if cfg.maxRetries < 0 {
		cfg.maxRetries = 0
	}
	if cfg.retryBase <= 0 {
		cfg.retryBase = defaultRetryBase
	}
	return &Writer{
		db:     p.writer,
		tasks:  make(chan writeTask, cfg.queueSize),
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
}

// Start 启动唯一 worker。重复调用无副作用；Stop 之后不可重启。
func (w *Writer) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.closed {
		return
	}
	w.started = true
	w.wg.Add(1)
	go w.loop()
}

// Stop 通知 worker 退出并等待在途任务收尾。
// 队列中未开始的任务会被逐一回写 ErrShuttingDown，不会让 Submit 永久等待。
func (w *Writer) Stop() {
	w.mu.Lock()
	if !w.started || w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.stopCh)
	w.mu.Unlock()
	w.wg.Wait()
}

// Depth / Cap 供运维探针与压测观察背压，不参与写入决策。
func (w *Writer) Depth() int { return len(w.tasks) }
func (w *Writer) Cap() int   { return cap(w.tasks) }

// Commits 返回成功提交的事务数（只读，不影响写入路径）。
func (w *Writer) Commits() int64 { return w.commits.Load() }

func (w *Writer) loop() {
	defer w.wg.Done()
	for {
		// 停机优先：在途事务跑完后，先检查退出信号，不再继续消费排队任务。
		// 否则 Stop 与队列同时就绪时 select 随机选，排队任务的结果会不确定。
		select {
		case <-w.stopCh:
			w.drain()
			return
		default:
		}
		select {
		case <-w.stopCh:
			w.drain()
			return
		case task := <-w.tasks:
			// 同步执行：在途事务会跑完才回到循环，退出时不会留下半提交状态。
			task.done <- w.execute(task)
		}
	}
}

// drain 给所有尚未开始的任务一个明确结果，避免调用方永久阻塞。
func (w *Writer) drain() {
	for {
		select {
		case task := <-w.tasks:
			task.done <- ErrShuttingDown
		default:
			return
		}
	}
}

// Submit 提交一个写入用例并等待结果。
// 队列满立即返回 ErrQueueFull；事务提交成功后才返回 nil。
func (w *Writer) Submit(ctx context.Context, fn TxFunc) error {
	if fn == nil {
		return errors.New("写入事务函数为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	task := writeTask{ctx: ctx, fn: fn, done: make(chan error, 1)}

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrShuttingDown
	}
	if !w.started {
		w.mu.Unlock()
		return ErrWriterNotStarted
	}
	select {
	case w.tasks <- task:
		w.mu.Unlock()
	default:
		w.mu.Unlock()
		return fmt.Errorf("%w: 容量 %d", ErrQueueFull, w.Cap())
	}

	select {
	case err := <-task.done:
		return err
	case <-ctx.Done():
		// 调用方放弃等待；worker 侧 task.ctx 同步取消，事务不会提交。
		return ctx.Err()
	}
}

func (w *Writer) execute(task writeTask) error {
	ctx, cancel := context.WithTimeout(task.ctx, w.cfg.timeout)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt <= w.cfg.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = runOnce(ctx, w.db, task.fn)
		if lastErr == nil {
			w.commits.Add(1) // 只有 Commit 成功才计数
			return nil
		}
		if !isRetryable(lastErr) {
			return fmt.Errorf("写入事务失败（不可重试）: %w", lastErr)
		}
		if attempt == w.cfg.maxRetries {
			break
		}
		delay := w.cfg.retryBase * time.Duration(1<<attempt)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("%w（尝试 %d 次）: %v", ErrRetryExhausted, w.cfg.maxRetries+1, lastErr)
}

// runOnce 执行单事务：fn 返回错误即回滚，只有 Commit 成功才返回 nil。
func runOnce(ctx context.Context, db *sql.DB, fn TxFunc) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // Commit 之后返回 sql.ErrTxDone，忽略

	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// isRetryable 只把锁竞争类错误判为可重试。
// 优先用驱动的错误码（避免字符串随版本漂移），包装错误再退回字符串匹配。
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		default:
			return false
		}
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "DATABASE IS LOCKED") ||
		strings.Contains(msg, "DATABASE TABLE IS LOCKED")
}
