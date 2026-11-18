package main

// 本机 PowerShell + OpenSSH 实测反馈回归：
//  1. 按 / 后缓冲区已预置 '/'，提示语再说「以 / 开头」会让用户补打成 //login；
//  2. /login 不带参数原本只回笼统「参数个数不符」，用户不知道要加 github；
//  3. 本机未配 client_id 时原本只回「登录失败，请稍后重试」，不可操作。

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
)

func TestCommandEntryHintDoesNotAskForSecondSlash(t *testing.T) {
	if strings.Contains(commandEntryHint, "以 / 开头") {
		t.Fatal("提示语不得再要求用户输入 /：缓冲区已经预置了一个")
	}
	if !strings.Contains(commandEntryHint, "无需再打一次") {
		t.Fatal("提示语必须明确说明 / 已自动填入")
	}
	// 零回显是 P0-22 安全约束，不打算改；但必须提前告知，否则用户会以为没反应。
	if !strings.Contains(commandEntryHint, "不回显") {
		t.Fatal("提示语必须说明输入过程不回显")
	}
}

func TestLoginWithoutProviderIsActionable(t *testing.T) {
	inv, err := command.Default().Parse("/login")
	if err != nil {
		t.Fatalf("/login 应能进入 provider 白名单分支，实际解析失败: %v", err)
	}
	if inv.Name != "login" || len(inv.Args) != 0 {
		t.Fatalf("解析结果不对: %+v", inv)
	}
	if isGitHubLogin(inv) {
		t.Fatal("无参数不得启动 Device Flow")
	}
	if !strings.Contains(auth.LoginTextUnsupportedProvider, "/login github") {
		t.Fatal("提示必须给出可操作的完整命令")
	}
}

func TestLoginStartFailureTextIsActionableAndNeverEchoesError(t *testing.T) {
	// state_machine.go 用 %w 包装，errors.Is 必须能穿透。
	wrapped := fmt.Errorf("申请设备码: %w", auth.ErrClientIDMissing)
	if got := loginStartFailureText(wrapped); got != auth.LoginTextClientIDMissing {
		t.Fatalf("缺 client_id 应给专用提示，实际 %q", got)
	}
	if !strings.Contains(auth.LoginTextClientIDMissing, "MOTD_GITHUB_CLIENT_ID") {
		t.Fatal("专用提示必须点名缺失的环境变量")
	}

	leaky := fmt.Errorf("上游返回: token=gho_SECRET_SHOULD_NOT_APPEAR")
	got := loginStartFailureText(leaky)
	if got != auth.LoginTextLoginFailed {
		t.Fatalf("其它错误应回落通用文案，实际 %q", got)
	}
	if strings.Contains(got, "gho_SECRET") || strings.Contains(got, leaky.Error()) {
		t.Fatal("绝不回显外部错误原文")
	}
}
