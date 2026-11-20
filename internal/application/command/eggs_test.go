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
	for _, name := range []string{"fortune", "cowsay", "xkcd", "blame", "sudo", "karma"} {
		line := "/" + name
		if name == "blame" {
			line += " @someone" // /blame 要求一个参数
		}
		inv, err := r.Parse(line)
		if err != nil {
			t.Errorf("彩蛋命令 %s 应可解析: %v", line, err)
			continue
		}
		if inv.Name != name {
			t.Errorf("解析 %s 得到 Name=%q", line, inv.Name)
		}
	}
	// /blame 要求 1 个参数，无参应被注册表挡掉。
	if _, err := r.Parse("/blame"); err == nil {
		t.Error("/blame 无参应报参数个数不符")
	}
	// 内置命令仍在。
	if _, err := r.Parse("/help"); err != nil {
		t.Errorf("合并后 /help 应仍可用: %v", err)
	}
	// Help 含彩蛋说明，且管理员分组仍在。
	help := r.Help()
	for _, name := range []string{"/fortune", "/karma"} {
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
		"fortune": {Fortune()},
		"cowsay":  {Cowsay()},
		"xkcd":    {XKCD()},
		"blame":   {Blame()},
		"sudo":    {Sudo()},
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
