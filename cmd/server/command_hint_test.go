package main

// v0.3.1 体验项：命令输入提示每会话只打一次（实测一次会话刷 5 次）。
// pageController 由 Serve 每连接新建，故重连自然重新打印。

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestCommandEntryHintShownOncePerSession(t *testing.T) {
	pc := &pageController{}
	var out bytes.Buffer

	pc.maybeShowCommandEntryHint(&out) // 第一次进入命令输入
	first := out.String()
	if !strings.Contains(first, commandEntryHint) {
		t.Fatalf("首次进入命令输入必须打印提示: %q", first)
	}
	if !pc.entryHintShown {
		t.Fatal("首次打印后应标记本会话已提示")
	}

	for i := 2; i <= 3; i++ {
		out.Reset()
		pc.maybeShowCommandEntryHint(&out)
		if out.Len() != 0 {
			t.Fatalf("第 %d 次进入命令输入应静默，实际输出 %q", i, out.String())
		}
	}
}

func TestCommandEntryHintResetOnNewSession(t *testing.T) {
	var first bytes.Buffer
	pc1 := &pageController{}
	pc1.maybeShowCommandEntryHint(&first)
	if !strings.Contains(first.String(), commandEntryHint) {
		t.Fatal("会话 1 首次应有提示")
	}

	// 重连 = 新的 pageController（Serve 每连接 newPageController）。
	var second bytes.Buffer
	pc2 := &pageController{}
	pc2.maybeShowCommandEntryHint(&second)
	if !strings.Contains(second.String(), commandEntryHint) {
		t.Fatal("新会话首次应重新打印提示")
	}
}

// 机械守卫：提示只能经 maybeShowCommandEntryHint 输出，防止有人改回直接写 sess。
func TestCommandEntryHintOnlyEmittedThroughOnceHelper(t *testing.T) {
	b, err := os.ReadFile("ssh_handler.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "io.WriteString(sess, commandEntryHint)") {
		t.Fatal("命令提示必须经 maybeShowCommandEntryHint 每会话只打一次")
	}
	if !strings.Contains(string(b), "pc.maybeShowCommandEntryHint(sess)") {
		t.Fatal("handle 必须接入 once 助手，守卫可能漏扫")
	}
}
