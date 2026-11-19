package sqlite

// ssh_keys.last_used_at 的异步节流记录器（P1-M4，ADR-036）。
//
// 直接写库有两个问题：每次 SSH 连接都写、且写在认证热路径上。这里做两件事：
//  1. 节流：每把指纹 ≥ DefaultKeyUsageInterval 才排一次写，内存里记上次时间；
//  2. 异步：NoteUse 只判定 + 标记，真正的 UPDATE 丢进独立 goroutine 走 Writer，
//     失败被吞掉（best-effort），认证路径零等待。
//
// 节流的「先标记、后写」是有意取舍：写失败时下一把连接仍要等一个窗口，
// 用它换「绝不因运维痕迹放大写库」。这在 OPS 中是明确记录的偏差。

import (
	"context"
	"sync"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

const (
	// DefaultKeyUsageInterval 是同一把钥匙两次使用记录的最小间隔。
	DefaultKeyUsageInterval = 5 * time.Minute
	keyUsageWriteTimeout    = 2 * time.Second
)

var _ auth.KeyUsageRecorder = (*KeyUsageRecorder)(nil)

// lastUsedWriter 是记录器实际依赖的窄端口，由 KeyBindingRepos.TouchLastUsed 实现；
// 测试用假实现即可回归节流逻辑，不必起真实数据库。
type lastUsedWriter interface {
	TouchLastUsed(ctx context.Context, fingerprint string, at time.Time) error
}

type KeyUsageRecorderOption func(*KeyUsageRecorder)

// WithKeyUsageClock 注入时钟，使 5 分钟节流可在毫秒级回归。
func WithKeyUsageClock(now func() time.Time) KeyUsageRecorderOption {
	return func(r *KeyUsageRecorder) {
		if now != nil {
			r.now = now
		}
	}
}

// KeyUsageRecorder 实现 auth.KeyUsageRecorder。零值不可用，须经 NewKeyUsageRecorder。
type KeyUsageRecorder struct {
	touch    lastUsedWriter
	now      func() time.Time
	interval time.Duration

	mu   sync.Mutex
	last map[string]time.Time
	wg   sync.WaitGroup
}

func NewKeyUsageRecorder(touch lastUsedWriter, interval time.Duration, opts ...KeyUsageRecorderOption) *KeyUsageRecorder {
	if touch == nil {
		panic("sqlite.NewKeyUsageRecorder: lastUsedWriter 不能为空")
	}
	if interval <= 0 {
		interval = DefaultKeyUsageInterval
	}
	r := &KeyUsageRecorder{
		touch: touch, now: time.Now, interval: interval, last: make(map[string]time.Time),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// NoteUse 判定节流窗口；命中窗口就排一次异步写。非阻塞、不返回错误。
func (r *KeyUsageRecorder) NoteUse(fingerprint string) {
	if fingerprint == "" {
		return
	}
	r.mu.Lock()
	now := r.now()
	if prev, ok := r.last[fingerprint]; ok && now.Sub(prev) < r.interval {
		r.mu.Unlock()
		return
	}
	r.last[fingerprint] = now
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		writeCtx, cancel := context.WithTimeout(context.Background(), keyUsageWriteTimeout)
		defer cancel()
		// 失败只影响运维展示，绝不影响已经成功的认证。
		_ = r.touch.TouchLastUsed(writeCtx, fingerprint, now)
	}()
}

// Wait 等待所有在途使用记录写完成。供停机（writer.Stop 之前）与测试使用。
func (r *KeyUsageRecorder) Wait() { r.wg.Wait() }
