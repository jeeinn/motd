package forum

// v0.5.0 / P3-M4（ADR-059）：最小压测集 —— 场景 2「浏览列表（读多）」。
// 用内存 fake 测用例编排与渲染无关的读路径开销；真实 SQLite 读延迟不在本最小集范围。
// 通过标准（首测后转基线）：0 错误、延迟分布稳定（相对劣化 ≤20% 视为回归）。

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkListThreads(b *testing.B) {
	e := newTestEnv()
	for _, parallel := range []int{16, 64} {
		b.Run(fmt.Sprintf("parallel=%d", parallel), func(b *testing.B) {
			b.SetParallelism(parallel)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := e.svc.ListNodes(context.Background()); err != nil {
						b.Errorf("ListNodes 失败: %v", err)
						return
					}
				}
			})
		})
	}
}
