package terminal

// 展示时区是进程级呈现策略，不是会话状态。
//
// 设计取舍（贝洛奇，patch「展示时区可配置」）：
//   - 时区在 main 读完 config 后一次性注入，之后只读；
//   - atomic.Pointer 保证多 SSH 会话并发渲染时无锁安全读取；
//   - 论坛列表/详情（formatTime）与 /keys 详情（DisplayLocation getter）
//     共用这一处来源，不会两处各读各的。
//
// 对 RenderScreen 纯函数语义的影响：它仍是给定 (Screen, width) 与这份不可变
// 进程配置的确定性函数——不读时钟、不读 DB、不碰 SSH、不持有会话状态。
// 唯一的外部依赖是「进程展示时区」这一配置常量，等价于 i18n/l10n 的全局 locale。
// 未注入时的回退值是 UTC（保持 patch 前行为）；生产启动一定会用
// cfg.DisplayLocation 覆盖（默认 Asia/Shanghai）。

import (
	"sync/atomic"
	"time"
)

var displayLocation atomic.Pointer[time.Location]

func init() {
	displayLocation.Store(time.UTC)
}

// SetDisplayLocation 设置进程展示时区。生产只在启动期调用一次；
// 测试可反复调用做隔离（先用 DisplayLocation 读回旧值，配合 t.Cleanup 复位）。
// nil 一律视为 UTC，避免 nil 穿透到 time.Time.In 引发 panic。
func SetDisplayLocation(loc *time.Location) {
	if loc == nil {
		loc = time.UTC
	}
	displayLocation.Store(loc)
}

// DisplayLocation 返回当前进程展示时区，永不返回 nil。
// /keys 详情用它格式化公钥时间，与论坛时间共享同一份配置。
func DisplayLocation() *time.Location {
	if loc := displayLocation.Load(); loc != nil {
		return loc
	}
	return time.UTC
}
