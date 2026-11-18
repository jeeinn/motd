package user

// Q5 拍板的等级分档：karma 达到阈值即升档，P0 只用于状态栏显示，不参与权限判定。
// 阈值是纯领域常量，放在 domain/user；Renderer 负责把档位翻译成具体文案。
const (
	KarmaL2 int64 = 10
	KarmaL3 int64 = 50
	KarmaL4 int64 = 200
)

// Level 由 karma 返回 1–4 档。负 karma 与零均视为 L1，不做惩罚性降级。
// 权限判定请用 Status.CanPost，不要用 Level —— 两者是不同关注点。
func Level(karma int64) uint8 {
	switch {
	case karma >= KarmaL4:
		return 4
	case karma >= KarmaL3:
		return 3
	case karma >= KarmaL2:
		return 2
	default:
		return 1
	}
}
