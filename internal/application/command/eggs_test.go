package command

// v0.5.1 / P3-M6：彩蛋命令合并与语料回归。

import (
	"strings"
	"testing"
)

func TestWithCommandsMergesEggs(t *testing.T) {
	r, err := WithCommands(EggCommands())
	if err != nil {
		t.Fatalf("合并彩蛋命令: %v", err)
	}
	for _, name := range []string{"cowsay", "sudo", "matrix"} {
		inv, err := r.Parse("/" + name)
		if err != nil {
			t.Errorf("彩蛋命令 %s 应可解析: %v", name, err)
			continue
		}
		if inv.Name != name {
			t.Errorf("解析 /%s 得到 Name=%q", name, inv.Name)
		}
	}
	// ADR-075：已删除的三个彩蛋命令不得再被解析。
	for _, name := range []string{"fortune", "xkcd", "blame"} {
		if _, err := r.Parse("/" + name); err == nil {
			t.Errorf("/%s 已删除，不应再可解析", name)
		}
	}
	// 内置命令仍在。
	if _, err := r.Parse("/help"); err != nil {
		t.Errorf("合并后 /help 应仍可用: %v", err)
	}
	// Help 含彩蛋说明，且管理员分组仍在。
	help := r.Help()
	for _, name := range []string{"/cowsay", "/matrix"} {
		if !strings.Contains(help, name) {
			t.Errorf("Help 应含 %s", name)
		}
	}
}

func TestWithCommandsRejectsDuplicate(t *testing.T) {
	// 重名命令必须被拒，避免覆盖内置命令。
	dup := []Command{{Name: "help", Usage: "/help", Summary: "假帮助"}}
	if _, err := WithCommands(dup); err == nil {
		t.Error("与内置命令重名的彩蛋应被拒绝")
	}
}

func TestEggCorpusIsNonEmptyAndSafe(t *testing.T) {
	for name, texts := range map[string][]string{
		"cowsay": {Cowsay()},
		"sudo":   {Sudo()},
	} {
		for _, text := range texts {
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s 语料为空", name)
			}
			// 彩蛋文案不得含 ESC 控制字符（安全渲染硬约束）。
			if strings.ContainsRune(text, 0x1b) {
				t.Errorf("%s 语料含 ESC 控制字符", name)
			}
		}
	}
}
