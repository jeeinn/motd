package main

// v0.5.1 / P3-M6：彩蛋分发与渲染回归——固定文案、无用户数据回显。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

func TestEggTextIsFixedCorpus(t *testing.T) {
	h := newInputHandler()
	for _, name := range []string{"fortune", "cowsay", "xkcd", "blame", "sudo"} {
		text := h.eggText(name)
		if strings.TrimSpace(text) == "" {
			t.Errorf("/%s 文案为空", name)
		}
	}
	// /blame 文案不含 @ 参数回显痕迹（固定模板）。
	if strings.Contains(h.eggText("blame"), "@") {
		t.Error("/blame 不应回显用户名")
	}
}

func TestWriteEggUsesSafeRender(t *testing.T) {
	var buf bytes.Buffer
	writeEgg(&buf, command.Fortune()+"\r\n")
	out := buf.String()
	if out == "" {
		t.Fatal("writeEgg 未输出")
	}
	// safe.RenderSystem(StylePlain) 不引入额外转义序列，文案保持可读。
	if strings.Contains(out, "箴言：") == false {
		t.Errorf("输出不含彩蛋文案: %q", out)
	}
}

func TestEggRegistryInjectsCommands(t *testing.T) {
	reg, err := eggRegistry(nil) // 无插件时 EggCommands 兜底仍注入
	if err != nil {
		t.Fatal(err)
	}
	h := newInputHandler().withCommands(reg)
	for _, name := range []string{"fortune", "cowsay", "xkcd", "blame", "sudo", "matrix"} {
		line := "/" + name
		if name == "blame" {
			line += " @someone"
		}
		if _, err := h.reg.Parse(line); err != nil {
			t.Errorf("注入后 %s 应可解析: %v", line, err)
		}
	}
}

// v0.7.2：彩蛋命令必须显示在独立的「彩蛋命令（无需权限）」分组里，
// 不能落在「管理员命令（需白名单指纹授权）」标题之下——那会误导权限。
func TestEggHelpGroupIsSeparateFromAdmin(t *testing.T) {
	reg, err := eggRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	help := reg.Help()

	adminAt := strings.Index(help, "管理员命令（需白名单指纹授权）")
	eggAt := strings.Index(help, "彩蛋命令（无需权限，纯娱乐）")
	keysAt := strings.Index(help, "\r\n键位：")
	if adminAt < 0 || eggAt < 0 {
		t.Fatalf("两个分组说明都应出现: admin=%d egg=%d", adminAt, eggAt)
	}
	if !(adminAt < eggAt && eggAt < keysAt) {
		t.Fatalf("分组顺序应为 管理员 → 彩蛋 → 键位，实际 admin=%d egg=%d keys=%d", adminAt, eggAt, keysAt)
	}
	if n := strings.Count(help, "彩蛋命令（无需权限，纯娱乐）"); n != 1 {
		t.Fatalf("彩蛋分组说明必须恰好一次，实际 %d 次", n)
	}

	for _, c := range reg.All() {
		at := strings.Index(help, c.Usage)
		if at < 0 {
			t.Errorf("帮助缺少命令 %q", c.Name)
			continue
		}
		if c.Group == command.GroupEgg && at < eggAt {
			t.Errorf("彩蛋命令 %q 不得出现在彩蛋说明之前（位置 %d）", c.Usage, at)
		}
		if c.Group == command.GroupAdmin && at > eggAt {
			t.Errorf("管理员命令 %q 不得落到彩蛋分组之后（位置 %d）", c.Usage, at)
		}
		if c.Group == command.GroupAdmin && c.Name == "fortune" {
			t.Errorf("彩蛋命令不得归入管理员组")
		}
	}
}

// ADR-065：/karma 入口删除后，cmd 的 karmaLevel 与 domain/user.Level 是两套阈值
// 实现，必须逐点对拍，防口径漂移误伤状态栏等级渲染。
func TestKarmaLevelMatchesDomainLevel(t *testing.T) {
	for karma := int64(0); karma < 300; karma++ {
		level, _ := karmaLevel(karma)
		if want := int(domainuser.Level(karma)); level != want {
			t.Errorf("karma=%d: karmaLevel=L%d，domain Level=L%d，不一致", karma, level, want)
		}
	}
}

func TestKarmaLevelThresholds(t *testing.T) {
	cases := []struct {
		karma int64
		level int
		next  int64
	}{
		{0, 1, 10}, {9, 1, 10}, {10, 2, 50}, {49, 2, 50},
		{50, 3, 200}, {199, 3, 200}, {200, 4, 0}, {999, 4, 0},
	}
	for _, c := range cases {
		level, next := karmaLevel(c.karma)
		if level != c.level || next != c.next {
			t.Errorf("karma=%d 得到 L%d/next=%d，期望 L%d/%d",
				c.karma, level, next, c.level, c.next)
		}
	}
}
