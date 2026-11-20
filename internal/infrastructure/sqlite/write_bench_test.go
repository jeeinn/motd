package sqlite

// v0.5.0 / P3-M4（ADR-059）：最小压测集 —— 场景 3「写队列背压」。
// 小队列容量制造背压，验证 Writer.Submit 的快速失败/提交语义与无卡死。
// 通过标准：submit 成功数 == commits 增量；ErrQueueFull 视为预期背压；无卡死。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

func BenchmarkWriteQueueBackpressure(b *testing.B) {
	ctx := context.Background()
	pools, err := Open(ctx, b.TempDir()+"/motd.db")
	if err != nil {
		b.Fatal(err)
	}
	defer pools.Close()
	if _, err := Migrate(ctx, pools); err != nil {
		b.Fatal(err)
	}
	w := pools.NewWriter(WithQueueSize(8)) // 小队列制造背压
	w.Start()
	defer w.Stop()

	var seq int64 // 跨子基准累加，避免两轮 slug 撞 UNIQUE(nodes.slug)
	for _, parallel := range []int{8, 32} {
		b.Run(fmt.Sprintf("parallel=%d", parallel), func(b *testing.B) {
			before := w.Commits()
			b.SetParallelism(parallel)
			b.ResetTimer()
			var submitted, full int64
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					slug := fmt.Sprintf("/bench/node-%d", atomic.AddInt64(&seq, 1))
					err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
						_, err := tx.ExecContext(ctx, `INSERT INTO nodes (slug, name, description) VALUES (?, ?, ?)`,
							slug, "bench", "")
						return err
					})
					if err == nil {
						atomic.AddInt64(&submitted, 1)
					} else if errors.Is(err, ErrQueueFull) {
						atomic.AddInt64(&full, 1) // 预期背压，非失败
					} else {
						b.Errorf("非预期写入错误: %v", err)
						return
					}
				}
			})
			if got := w.Commits() - before; got != submitted {
				b.Errorf("提交数 %d != 成功提交数 %d", got, submitted)
			}
			b.ReportMetric(float64(full)/float64(b.N)*100, "backpressure%")
		})
	}
}
