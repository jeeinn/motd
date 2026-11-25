package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
)

func runeMsg(r rune) input.KeyMsg { return input.KeyMsg{Kind: input.KeyRune, Rune: r} }

func TestApplyKeyCommandLine(t *testing.T) {
	h := newInputHandler()
	if h.reg != command.Default() {
		t.Fatal("inputHandler 必须使用唯一的默认注册表")
	}

	cl := &commandLine{}
	if a, _, _ := applyKey(cl, runeMsg('?')); a != actionHelp {
		t.Fatalf("? 应触发帮助，实际 %v", a)
	}
	if a, _, _ := applyKey(cl, runeMsg('/')); a != actionCommandStart || !cl.active {
		t.Fatalf("/ 应进入命令编辑态，实际 action=%v active=%v", a, cl.active)
	}
	for _, r := range "open 3" {
		if a, _, _ := applyKey(cl, runeMsg(r)); a != actionNone {
			t.Fatalf("编辑态下可打印字符不应触发动作，实际 %v", a)
		}
	}
	// Backspace 不应删掉开头那个 '/'
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyBackspace}); a != actionNone {
		t.Fatal("Backspace 不应触发动作")
	}
	_, _, _ = applyKey(cl, runeMsg('3'))
	a, line, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyEnter})
	if a != actionSubmit || line != "/open 3" {
		t.Fatalf("Enter 应提交 /open 3，实际 action=%v line=%q", a, line)
	}
	if cl.active {
		t.Fatal("提交后必须退出编辑态")
	}
	inv, err := h.reg.Parse(line)
	if err != nil || inv.Name != "open" {
		t.Fatalf("提交的命令行应能解析为 /open，实际 inv=%+v err=%v", inv, err)
	}
}

func TestApplyKeyCancelQuitAndPaste(t *testing.T) {
	cl := &commandLine{}
	_, _, _ = applyKey(cl, runeMsg('/'))
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyEsc}); a != actionCancel || cl.active {
		t.Fatal("编辑态 Esc 应取消并退出编辑态")
	}
	_, _, _ = applyKey(cl, runeMsg('/'))
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyCtrlC}); a != actionCancel {
		t.Fatal("编辑态 Ctrl-C 应取消命令输入")
	}
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyCtrlD}); a != actionQuit {
		t.Fatal("非编辑态 Ctrl-D 应退出会话")
	}
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyCtrlC}); a != actionQuit {
		t.Fatal("非编辑态 Ctrl-C 应退出会话")
	}
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyEsc}); a != actionNone {
		t.Fatal("非编辑态 Esc 应静默（浏览态没有可取消对象）")
	}
	if a, _, n := applyKey(cl, input.KeyMsg{Kind: input.KeyPaste, Text: "abc"}); a != actionPasteNotice || n != 3 {
		t.Fatalf("非编辑态粘贴应只提示字节数，实际 action=%v n=%d", a, n)
	}
}

// 浏览态连按 Esc：动作必须是 actionNone（在 ADR-027 的「屏幕连续」集合里），
// 且固定提示函数返回空串——两级合起来即「输出零新增文本、屏幕不追加行」。
func TestBrowseEscIsSilentOnRepeats(t *testing.T) {
	cl := &commandLine{}
	for i := 0; i < 3; i++ {
		action, _, pasteLen := applyKey(cl, input.KeyMsg{Kind: input.KeyEsc})
		if action != actionNone {
			t.Fatalf("第 %d 次浏览态 Esc 应为 actionNone，实际 %v", i+1, action)
		}
		if got := actionNotice(action, pasteLen); got != "" {
			t.Fatalf("第 %d 次浏览态 Esc 不得产生输出，实际 %q", i+1, got)
		}
		if cl.active || len(cl.runes) != 0 {
			t.Fatalf("浏览态 Esc 不得改变命令行状态: active=%v runes=%q", cl.active, string(cl.runes))
		}
	}
	// 屏幕连续集合必须含 actionNone，否则下一次渲染会退回追加模式（ADR-027）。
	continuous := map[cmdAction]bool{
		actionMoveDown: true, actionMoveUp: true, actionOpen: true, actionBack: true,
		actionNextPage: true, actionPrevPage: true, actionNone: true,
	}
	if !continuous[actionNone] {
		t.Fatal("actionNone 必须保持屏幕连续")
	}
}

// 命令输入态 Esc 仍输出取消文案；粘贴提示仍按纯函数产生固定文本（回归）。
func TestActionNoticeKeepsRealCancelTexts(t *testing.T) {
	if got := actionNotice(actionCancel, 0); got != "（已取消命令输入）\r\n" {
		t.Fatalf("命令输入态取消文案被破坏: %q", got)
	}
	if got := actionNotice(actionPasteNotice, 3); got != "（已收到粘贴块 3 字节；请先在编辑态内粘贴）\r\n" {
		t.Fatalf("粘贴提示文案被破坏: %q", got)
	}
	// 浏览态 Esc 改造后，旧的「（取消）」文案彻底消失。
	if strings.Contains(actionNotice(actionNone, 0), "取消") {
		t.Fatal("静默动作不得输出取消文案")
	}
}

func TestFormatResultNeverEchoesUserInput(t *testing.T) {
	reg := command.Default()
	malicious := []string{
		"/x\x1b[2J",
		"/open 3\x1b]52;c;YWJj\x07 4",
		"\x1b[31mhello",
		"/help\x1b]0;pwned\x07",
	}
	for _, line := range malicious {
		inv, err := reg.Parse(line)
		out := formatResult(reg, inv, err)
		if strings.Contains(out, "\x1b") || strings.Contains(out, "pwned") || strings.Contains(out, "hello") {
			t.Errorf("回显泄露了用户提交的原始文本: line=%q out=%q", line, out)
		}
	}

	// /help 必须由注册表生成，包含全部命令用法。
	inv, err := reg.Parse("/help")
	out := formatResult(reg, inv, err)
	for _, c := range reg.All() {
		if !strings.Contains(out, c.Usage) {
			t.Errorf("/help 输出缺少 %q", c.Usage)
		}
	}

	// 参数属于用户输入，不回显；命令名来自注册表白名单，可以出现。
	inv, err = reg.Parse("/open 3")
	out = formatResult(reg, inv, err)
	if !strings.Contains(out, "/open") || strings.Contains(out, " 3") {
		t.Fatalf("只应回显白名单命令名，不应回显参数: %q", out)
	}
}

// ---------------------------------------------------------------------------
// /matrix 全屏动画（ADR-072 §5）：进入/退出清屏、三条中断路径、合规红线
// ---------------------------------------------------------------------------

func TestMatrixEntersAndExitsWithCleanFrame(t *testing.T) {
	var b bytes.Buffer
	h := newInputHandler()
	// Frames=3 + 1ms 帧间隔：固定 3 帧后自然结束，测试不依赖墙钟。
	h.runMatrix(context.Background(), &b, make(chan input.KeyMsg), nil, matrixRun{
		Width: 40, Height: 10, Frames: 3, FrameGap: time.Millisecond,
	})
	out := b.String()
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Fatalf("动画应以进入清屏 H+2J 开头，实际 %q", out[:min(24, len(out))])
	}
	if !strings.HasSuffix(out, "\x1b[H\x1b[2J\x1b[0m") {
		t.Fatalf("动画应以退出清屏+复位结尾（供调用方重绘原页），实际 %q", out[max(0, len(out)-24):])
	}
	if !strings.Contains(out, "[Esc 返回]") {
		t.Fatal("首行必须提示退出方式")
	}
	// 雨区 9 行（Height-1）：每帧用 CUU(8) 回到雨区首行，屏内覆盖而非追加。
	if n := strings.Count(out, "\x1b[8A"); n != 3 {
		t.Fatalf("应写出 3 个覆盖帧（各含一次 CUU(8)），实际 %d", n)
	}
}

func TestMatrixEscInterrupts(t *testing.T) {
	var b bytes.Buffer
	h := newInputHandler()
	msgs := make(chan input.KeyMsg, 1)
	msgs <- input.KeyMsg{Kind: input.KeyEsc}
	h.runMatrix(context.Background(), &b, msgs, nil, matrixRun{
		Width: 40, Height: 10, Frames: 5, FrameGap: 50 * time.Millisecond,
	})
	out := b.String()
	// Esc 立即中断：一个覆盖帧都不该写出。
	if n := strings.Count(out, "\x1b[8A"); n != 0 {
		t.Fatalf("Esc 应立即中断（不写覆盖帧），实际写出 %d 帧", n)
	}
	if !strings.HasSuffix(out, "\x1b[H\x1b[2J\x1b[0m") {
		t.Fatalf("Esc 退出也必须清屏+复位收尾，实际 %q", out[max(0, len(out)-24):])
	}
	if !strings.Contains(out, "[Esc 返回]") {
		t.Fatal("Esc 路径仍应输出提示行")
	}
}

// 任意键都应中断动画：动画期吞掉的按键会让用户以为界面卡死
// （v0.9.2 / ADR-076；此前只有 Esc 生效，Ctrl-C 被静默丢弃）。
func TestMatrixAnyKeyExits(t *testing.T) {
	var b bytes.Buffer
	h := newInputHandler()
	msgs := make(chan input.KeyMsg, 1)
	msgs <- input.KeyMsg{Kind: input.KeyRune, Rune: 'j'}
	h.runMatrix(context.Background(), &b, msgs, nil, matrixRun{
		Width: 40, Height: 10, Frames: 5, FrameGap: 50 * time.Millisecond,
	})
	out := b.String()
	if n := strings.Count(out, "\x1b[8A"); n != 0 {
		t.Fatalf("任意键应立即中断（不写覆盖帧），实际写出 %d 帧", n)
	}
	if !strings.HasSuffix(out, "\x1b[H\x1b[2J\x1b[0m") {
		t.Fatalf("任意键退出也必须清屏+复位收尾，实际 %q", out[max(0, len(out)-24):])
	}
}

// 并发上限：整站上行 1 Mbps 是共享资源，超过上限必须拒绝进入。
func TestMatrixConcurrentSlotLimit(t *testing.T) {
	h := newInputHandler()
	if !h.acquireMatrixSlot() || !h.acquireMatrixSlot() {
		t.Fatal("前两个名额应能占用")
	}
	if h.acquireMatrixSlot() {
		t.Fatal("超过并发上限（2）必须拒绝")
	}
	h.releaseMatrixSlot()
	if !h.acquireMatrixSlot() {
		t.Fatal("归还名额后应能再次占用")
	}
	h.releaseMatrixSlot()
	h.releaseMatrixSlot()
}

func TestMatrixContextCancel(t *testing.T) {
	var b bytes.Buffer
	h := newInputHandler()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.runMatrix(ctx, &b, make(chan input.KeyMsg), nil, matrixRun{
		Width: 40, Height: 10, Frames: 5, FrameGap: 50 * time.Millisecond,
	})
	out := b.String()
	if n := strings.Count(out, "\x1b[8A"); n != 0 {
		t.Fatalf("ctx 取消后应立即停止（不写覆盖帧），实际写出 %d 帧", n)
	}
	if !strings.HasSuffix(out, "\x1b[H\x1b[2J\x1b[0m") {
		t.Fatalf("断连退出必须清屏+复位收尾，避免留脏屏: %q", out[max(0, len(out)-24):])
	}
}

// 帧数上限即自然结束：写满 N 帧后停。
func TestMatrixFramesLimit(t *testing.T) {
	var b bytes.Buffer
	h := newInputHandler()
	h.runMatrix(context.Background(), &b, make(chan input.KeyMsg), nil, matrixRun{
		Width: 40, Height: 12, Frames: 3, FrameGap: time.Millisecond,
	})
	out := b.String()
	// 雨区 11 行 → CUU(10)。
	if n := strings.Count(out, "\x1b[10A"); n != 3 {
		t.Fatalf("帧数上限应写出 3 帧，实际 %d", n)
	}
	if !strings.HasSuffix(out, "\x1b[H\x1b[2J\x1b[0m") {
		t.Fatalf("自然结束也必须清屏+复位收尾: %q", out[max(0, len(out)-24):])
	}
}

func TestMatrixNoForbiddenSequences(t *testing.T) {
	var b bytes.Buffer
	h := newInputHandler()
	h.runMatrix(context.Background(), &b, make(chan input.KeyMsg), nil, matrixRun{
		Width: 40, Height: 10, Frames: 3, FrameGap: time.Millisecond,
	})
	out := b.String()
	cmdOnlyWhitelistedANSI(t, out)
	for _, bad := range []string{
		"\x1b[?1049h", "\x1b[?1049l", // 备用屏
		"\x1b[3J", "\x1b[1J", "\x1b[J", // 清历史 / 其他 ED
		"\x1b[2;3H", "\x1b[1;1H", "\x1b[H\x1b[2;", // 带参 CUP
		"\x1b]52", "\x1b]0;", "\x1b]8", // OSC
		"\x1b[38;5;", "\x1b[38;2;", // 256 色 / 真彩（ROADMAP §4 冻结决策）
	} {
		if strings.Contains(out, bad) {
			t.Fatalf("动画泄露禁止序列 %q: %q", bad, out)
		}
	}
}

// TestMatrixEscViaPollInterrupts 覆盖真实链路的 Esc：单独按下的 ESC 需要
// KeyDecoder.Poll 判定（25ms 窗口），动画期间主循环阻塞、无人 Poll，
// 因此 runMatrix 必须自己每帧补判一次——msgs 永远为空，只有 poll 能给出 Esc。
func TestMatrixEscViaPollInterrupts(t *testing.T) {
	var b bytes.Buffer
	h := newInputHandler()
	poll := func() []input.KeyMsg { return []input.KeyMsg{{Kind: input.KeyEsc}} }
	h.runMatrix(context.Background(), &b, make(chan input.KeyMsg), poll, matrixRun{
		Width: 40, Height: 10, Frames: 5, FrameGap: time.Millisecond,
	})
	out := b.String()
	// poll 补判在写帧之前进行：Esc 生效时一个覆盖帧都不该写出。
	if n := strings.Count(out, "\x1b[8A"); n != 0 {
		t.Fatalf("Poll 判定 Esc 后应立即中断（不写覆盖帧），实际 %d 帧", n)
	}
	if !strings.HasSuffix(out, "\x1b[H\x1b[2J\x1b[0m") {
		t.Fatalf("Esc（Poll 路径）退出也必须清屏+复位收尾: %q", out[max(0, len(out)-24):])
	}
}
