package user

import "testing"

// 等级只是显示分档，边界值必须锁死：达到阈值才升档，阈值前一档不变。
func TestLevelThresholds(t *testing.T) {
	cases := []struct {
		karma int64
		want  uint8
	}{
		{-100, 1}, {0, 1}, {KarmaL2 - 1, 1},
		{KarmaL2, 2}, {KarmaL3 - 1, 2},
		{KarmaL3, 3}, {KarmaL4 - 1, 3},
		{KarmaL4, 4}, {KarmaL4 * 10, 4},
	}
	for _, c := range cases {
		if got := Level(c.karma); got != c.want {
			t.Errorf("Level(%d) = %d，期望 %d", c.karma, got, c.want)
		}
	}
}

func TestLevelIsIndependentFromWritePermission(t *testing.T) {
	// 等级只用于展示；冻结账号即使达到最高档，写权限仍由 Status 决定。
	frozen := User{Karma: KarmaL4 * 10, Status: StatusDeleting}
	if Level(frozen.Karma) != 4 {
		t.Fatal("高 karma 应为 L4")
	}
	if frozen.Status.CanPost() {
		t.Fatal("冻结状态不应因高等级而获得写权限")
	}
}
