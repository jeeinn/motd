package terminal

// v0.5.0 / P3-M4（ADR-059）：最小压测集 —— 场景 4「并发读 + 渲染 + presence」。
// 100 会话量级验证渲染纯函数与显示时区并发读的无锁安全；配合 CI 的 -race job
// 检查数据竞争。通过标准：0 错误、Online/Peak 统计一致。

import (
	"fmt"
	"testing"

	"github.com/jeeinn/motd/internal/application/presence"
	"github.com/jeeinn/motd/internal/application/viewmodel"
)

func BenchmarkConcurrentRenderWithPresence(b *testing.B) {
	reg := presence.New()
	const sessions = 100
	for i := 0; i < sessions; i++ {
		if err := reg.Connect(fmt.Sprintf("bench-%d", i), presence.KindSSH, 0); err != nil {
			b.Fatal(err)
		}
	}
	screen := viewmodel.Screen{
		Kind:      viewmodel.ScreenNodes,
		StatusBar: viewmodel.StatusBar{Identity: viewmodel.Identity{Guest: true}, Level: 1},
	}

	b.SetParallelism(sessions)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := RenderScreen(screen, 80); err != nil {
				b.Errorf("渲染失败: %v", err)
				return
			}
		}
	})
	if got := reg.Online(); got != sessions {
		b.Errorf("Online 应为 %d，实际 %d", sessions, got)
	}
	if got := reg.Peak(); got != sessions {
		b.Errorf("Peak 应为 %d，实际 %d", sessions, got)
	}
}
