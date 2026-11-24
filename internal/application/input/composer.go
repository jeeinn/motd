package input

// 多行正文编辑器内核（P1-M4）。
//
// 边界：本文件是**纯应用逻辑**——只吃归一化的 KeyMsg、只吐内存里的文本行，
// 不认识 SSH / 终端 / 渲染，也不 import transport / infrastructure（archtest
// 静态守卫会检查）。行编辑回显由调用方（cmd/server 的 lineEcho）负责。
//
// 提交语义（二轮讨论拍板，ADR-034）：
//   - KeyEnter 在正文里**只插入换行**，绝不提交；
//   - 单独一行恰好为 SubmitSentinel（"."）后按 Enter → 提交，该哨兵行不进正文；
//   - KeyCtrlD → 立即提交当前缓冲；
//   - KeyPaste 整块进正文，**内部换行不拆成 KeyEnter**，因此粘贴进来的 "." 行
//     不会被误判为哨兵（decoder 也保证一次 paste 只产一个 KeyPaste）。
//
// 为什么不把「Ctrl 与 Enter 的组合键」当提交键：通用 SSH 终端在字节流层面
// 无法可靠区分该组合与普通 Enter，P1 不做主提交键（写进提示文案的负向断言）。
//
// 跨行退格合并（v0.3.0）：当前行非空时退格删本行最后一个 rune；当前行为空且
// 不是第一行时，删除当前空行并把它并入上一行末尾——ComposerOp.MergedLines=true，
// MergedText 携带合并后的上一行文本供 cmd 层整行重绘。合并只删行、不增字节，
// 故不触发字节/行数上限校验；第一行空行退格无上一行可并，报 Deleted=false。
// 光标定位不做在这层（纯应用逻辑），由回显层用 ADR-027 白名单序列实现。

import (
	"errors"
	"strings"
)

const (
	// DefaultMaxLines 是正文行数的保守默认值；调用方可按业务上限覆盖。
	DefaultMaxLines = 200
	// DefaultMaxBytes 与 forum.MaxBodyBytes（8 KiB）一致，按字节而非字符计，
	// 防止一次超长提交白跑事务。
	DefaultMaxBytes = 8 << 10
	// SubmitSentinel 是「单独一行提交」的哨兵；只在整行内容恰好等于它时生效。
	SubmitSentinel = "."
)

var (
	// ErrComposerInactive 表示编辑器未 Start 或已提交后继续 Feed。
	ErrComposerInactive = errors.New("多行编辑器未激活")
	// ErrTooManyLines 表示再收下这次输入会超过行数上限；状态保持不变。
	ErrTooManyLines = errors.New("正文行数超过上限")
	// ErrTooManyBytes 表示再收下这次输入会超过字节上限；状态保持不变。
	ErrTooManyBytes = errors.New("正文字节数超过上限")
)

// OpKind 是 Feed 对外暴露的语义动作，调用方据此决定回显方式。
type OpKind uint8

const (
	OpNone      OpKind = iota
	OpRune             // 追加了一个可打印字符
	OpNewline          // 插入了一个换行（正文新增一行）
	OpBackspace        // 退格：Deleted=删到本行字符；MergedLines=空行并入上一行
	OpPaste            // 整块粘贴已收下（Text 为净化换行后的实际入正文文本）
	OpSubmit           // 提交：缓冲区已是最终正文
)

// ComposerOp 是一次 Feed 的结果。Rune/Text/Deleted 只在对应 Kind 下有意义。
// MergedLines/MergedText 只在 OpBackspace 且发生跨行合并时有意义：
// MergedLines 表示当前空行已并入上一行，MergedText 是合并后的上一行文本（供回显重绘）。
type ComposerOp struct {
	Kind        OpKind
	Rune        rune
	Text        string
	Deleted     bool
	MergedLines bool
	MergedText  string
}

// Composer 是单会话一份的多行正文缓冲。零值不可用，必须经 NewComposer + Start。
type Composer struct {
	lines    [][]rune
	active   bool
	maxLines int
	maxBytes int
}

// ComposerOption 覆盖默认上限。非正值回落到默认值，避免误配成「零行正文」。
type ComposerOption func(*Composer)

func WithMaxLines(n int) ComposerOption {
	return func(c *Composer) { c.maxLines = n }
}

func WithMaxBytes(n int) ComposerOption {
	return func(c *Composer) { c.maxBytes = n }
}

func NewComposer(opts ...ComposerOption) *Composer {
	c := &Composer{maxLines: DefaultMaxLines, maxBytes: DefaultMaxBytes}
	for _, opt := range opts {
		opt(c)
	}
	if c.maxLines <= 0 {
		c.maxLines = DefaultMaxLines
	}
	if c.maxBytes <= 0 {
		c.maxBytes = DefaultMaxBytes
	}
	return c
}

// Start 开启一次编辑，重置为空缓冲。重复调用等于重新开始。
func (c *Composer) Start() {
	c.lines = [][]rune{{}}
	c.active = true
}

// Active 报告当前是否处于可输入的编辑态。
func (c *Composer) Active() bool { return c.active }

// Lines 返回当前正文行快照（防御性副本，调用方改动不影响内部状态）。
func (c *Composer) Lines() []string {
	out := make([]string, len(c.lines))
	for i, line := range c.lines {
		out[i] = string(line)
	}
	return out
}

// Text 返回以 LF 连接的正文。提交后仍可读，供用例层取最终文本。
func (c *Composer) Text() string { return strings.Join(c.Lines(), "\n") }

// Load 以全文重建缓冲并激活，供草稿恢复；替代 Start 的清空语义。
//
// 契约（与 Feed 的「超限不改状态」同款）：
//   - 整块构造 lines：按 \n 切分后逐行 []rune，**绝不逐字符 Feed**——否则正文里
//     独立的 "." 行会命中 Enter 的哨兵判定被误判为提交（见 Feed 的 KeyEnter 分支）；
//   - CRLF/CR 归一化为 LF；空文本 = 单行空缓冲；
//   - 复用 maxLines / maxBytes 与 byteLen 校验，超限返回错误且状态不变
//     （初始 active=false 时保持 false）；
//   - 成功后置 active=true，调用方不再 Start()（Start 会清空）。
func (c *Composer) Load(text string) error {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	parts := strings.Split(normalized, "\n")
	next := make([][]rune, len(parts))
	for i, p := range parts {
		next[i] = []rune(p)
	}
	if len(next) > c.maxLines {
		return ErrTooManyLines
	}
	if c.byteLen(next) > c.maxBytes {
		return ErrTooManyBytes
	}
	c.lines = next
	c.active = true
	return nil
}

// byteLen 按 Text() 的实际形态计数：行间 LF 也算 1 字节。
func (c *Composer) byteLen(lines [][]rune) int {
	n := 0
	for i, line := range lines {
		if i > 0 {
			n++ // 行间换行
		}
		n += len(string(line))
	}
	return n
}

// Feed 消费一个归一化按键，返回语义动作。任何超限错误都保证状态未被修改，
// 调用方可安全地只输出一句固定提示、不回显被拒的字符。
func (c *Composer) Feed(m KeyMsg) (ComposerOp, error) {
	if !c.active {
		return ComposerOp{}, ErrComposerInactive
	}

	switch m.Kind {
	case KeyCtrlD:
		return c.submit()

	case KeyEnter:
		cur := c.lines[len(c.lines)-1]
		if len(cur) == 1 && cur[0] == rune(SubmitSentinel[0]) {
			// 哨兵行不进正文：删掉它，再把剩余行交给提交。
			if len(c.lines) == 1 {
				c.lines[0] = nil
			} else {
				c.lines = c.lines[:len(c.lines)-1]
			}
			return c.submit()
		}
		if len(c.lines)+1 > c.maxLines {
			return ComposerOp{}, ErrTooManyLines
		}
		c.lines = append(c.lines, nil)
		return ComposerOp{Kind: OpNewline}, nil

	case KeyRune:
		next := cloneLines(c.lines)
		next[len(next)-1] = append(next[len(next)-1], m.Rune)
		if c.byteLen(next) > c.maxBytes {
			return ComposerOp{}, ErrTooManyBytes
		}
		c.lines = next
		return ComposerOp{Kind: OpRune, Rune: m.Rune}, nil

	case KeyBackspace:
		cur := &c.lines[len(c.lines)-1]
		if len(*cur) == 0 {
			// 当前行为空：第一行没有上一行可并，保持原状并报未删除。
			if len(c.lines) == 1 {
				return ComposerOp{Kind: OpBackspace, Deleted: false}, nil
			}
			// 非首行：删掉当前空行并入上一行末尾。当前行为空，故合并后的
			// 上一行内容不变；只删行不增字节，无需重做上限校验。
			merged := string(c.lines[len(c.lines)-2])
			c.lines = c.lines[:len(c.lines)-1]
			return ComposerOp{Kind: OpBackspace, MergedLines: true, MergedText: merged}, nil
		}
		*cur = (*cur)[:len(*cur)-1]
		return ComposerOp{Kind: OpBackspace, Deleted: true}, nil

	case KeyPaste:
		// 终端粘贴多行可能带 CRLF 或裸 CR，统一成 LF 后按行追加。
		// 关键：整块文本直接进缓冲，**不逐字节重新构造 KeyMsg**，
		// 因此内部的换行 / "." 行都不会触发 Enter 的哨兵判定。
		text := strings.ReplaceAll(m.Text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\r", "\n")
		parts := strings.Split(text, "\n")
		next := cloneLines(c.lines)
		next[len(next)-1] = append(next[len(next)-1], []rune(parts[0])...)
		for _, p := range parts[1:] {
			next = append(next, []rune(p))
		}
		if len(next) > c.maxLines {
			return ComposerOp{}, ErrTooManyLines
		}
		if c.byteLen(next) > c.maxBytes {
			return ComposerOp{}, ErrTooManyBytes
		}
		c.lines = next
		return ComposerOp{Kind: OpPaste, Text: text}, nil

	default:
		// 方向键 / Tab / Unknown 等：不进正文、不产生回显动作。
		return ComposerOp{Kind: OpNone}, nil
	}
}

func (c *Composer) submit() (ComposerOp, error) {
	c.active = false
	return ComposerOp{Kind: OpSubmit}, nil
}

// cloneLines 复制外层切片与每个行缓冲，保证「超限不改状态」与 Lines() 快照语义。
func cloneLines(in [][]rune) [][]rune {
	out := make([][]rune, len(in))
	for i, line := range in {
		out[i] = append([]rune(nil), line...)
	}
	return out
}
