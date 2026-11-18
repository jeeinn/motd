package safe

import (
	"strings"
	"testing"
)

func TestSanitizeMakesControlSequencesVisible(t *testing.T) {
	clean := SanitizeText("a\x1b[2Jb")
	if strings.ContainsRune(clean, 0x1b) {
		t.Fatal("净化后不得残留 ESC 字节")
	}
	if clean != "a␛[2Jb" {
		t.Fatalf("ESC 应变成可见控制图示，其余参数字节保留为普通文本，实际 %q", clean)
	}

	osc := SanitizeText("\x1b]52;c;YWJj\x07")
	if !strings.Contains(osc, "␛]52;c;YWJj") || !strings.Contains(osc, "␇") {
		t.Fatalf("OSC 52 与 BEL 都必须可见化，实际 %q", osc)
	}
	assertNoRawControl(t, osc)
}

func TestSanitizeStripsC1AndBidi(t *testing.T) {
	clean := SanitizeText("a\u009b31mb\u202eX\u202cc")
	assertNoRawControl(t, clean)
	// 剥离的是控制字符本身；其后的 "31m" 是可打印文本，必须保留为普通内容
	// （不再构成控制序列，因为起始的 C1 CSI 已消失）。
	if clean != "a31mbXc" {
		t.Fatalf("C1 CSI 与双向文本控制符应被剥离，实际 %q", clean)
	}
	if got := SanitizeText("x\uFEFFy\u200Bz"); got != "xyz" {
		t.Fatalf("BOM / 零宽字符应被剥离，实际 %q", got)
	}
}

func TestSanitizeTabExpansionAndC0(t *testing.T) {
	if got := SanitizeText("a\tb"); got != "a       b" {
		t.Fatalf("Tab 应按 8 列展开（a 后 7 空格），实际 %q", got)
	}
	clean := SanitizeText("a\x00b\x7fc")
	assertNoRawControl(t, clean)
	if !strings.Contains(clean, "␀") || !strings.Contains(clean, "␡") {
		t.Fatalf("NUL / DEL 应变成可见控制图示，实际 %q", clean)
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
	}{
		{'A', 1},
		{'中', 2},
		{'文', 2},
		{'😀', 2},
		{'あ', 2},
		{'́', 0}, // U+0301 组合音标
		{'\n', 0},
	}
	for _, c := range cases {
		if got := DisplayWidth(c.r); got != c.want {
			t.Errorf("DisplayWidth(%q) = %d，期望 %d", c.r, got, c.want)
		}
	}
}

func TestWrapTextZeroWidthIsNoop(t *testing.T) {
	raw := "a\nb"
	if got := WrapText(raw, 0); got != raw {
		t.Fatalf("Width<=0 应原样返回，实际 %q", got)
	}
	if got := WrapText("中文AB", 3); strings.Contains(got, "\n") == false {
		t.Fatalf("宽字符应按 2 列计，中文AB 在宽度 3 下应折行，实际 %q", got)
	}
}
