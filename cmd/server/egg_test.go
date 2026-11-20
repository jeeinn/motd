package main

// v0.5.1 / P3-M6：彩蛋分发与渲染回归——固定文案、无用户数据回显。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
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
	for _, name := range []string{"fortune", "cowsay", "xkcd", "blame", "sudo", "karma"} {
		line := "/" + name
		if name == "blame" {
			line += " @someone"
		}
		if _, err := h.reg.Parse(line); err != nil {
			t.Errorf("注入后 %s 应可解析: %v", line, err)
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
