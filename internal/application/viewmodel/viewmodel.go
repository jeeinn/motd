// Package viewmodel 定义统一应用状态模型（评审稿 v0.2 §2.3）。
//
// 边界（P0-11 验收项）：本包只含语义字段，不含颜色、尺寸、控制序列、
// 光标位置、帧序号或任何渲染库类型。终端与浏览器两个 Renderer 各自把
// Screen 翻译成受控输出；共享的是状态与业务用例，不是渲染结果。
//
// 所有字段都是值类型（整数 / 字符串 / 布尔 / 枚举 / 结构体 / 切片），
// 不含函数、通道、接口、指针或 time.Time：时间统一存 Unix 秒，
// 不预先格式化，避免把某个 Renderer 的格式约定泄漏进共享状态。
package viewmodel

import "fmt"

// ScreenKind 是页面种类。用枚举而非字符串魔法值，非法值在校验时即拒绝。
type ScreenKind uint8

const (
	ScreenUnknown       ScreenKind = iota
	ScreenNodes                    // 版块列表
	ScreenThreads                  // 主题列表
	ScreenThreadDetail             // 主题详情 + 回复列表
	ScreenComposer                 // 编辑态（发帖 / 回复）
	ScreenLogin                    // Device Flow 登录态
	ScreenHelp                     // 命令帮助
	ScreenSearchResults            // 正文检索结果
)

// Valid 报告页面种类是否在允许取值域内；零值不合法，调用方必须显式指定。
func (k ScreenKind) Valid() bool { return k >= ScreenNodes && k <= ScreenSearchResults }

// NoticeKind 是状态栏提示的语义分类。具体配色由 Renderer 决定。
type NoticeKind uint8

const (
	NoticeNone NoticeKind = iota
	NoticeInfo
	NoticeSuccess
	NoticeWarning
	NoticeError
)

func (k NoticeKind) Valid() bool { return k <= NoticeError }

// Notice 是一条待展示的提示。Code 是稳定的机器可读码（如 rate_limited），
// Message 是可直接展示的语义文本；RetryAfterSec 供限流类提示使用。
type Notice struct {
	Kind          NoticeKind
	Code          string
	Message       string
	RetryAfterSec int
}

// OverlayKind 是浮层种类。
type OverlayKind uint8

const (
	OverlayNone       OverlayKind = iota
	OverlayAlert                  // 只展示信息，确认后关闭
	OverlayConfirm                // 二选一确认
	OverlayPrompt                 // 需要输入
	OverlayDeviceCode             // Device Flow 等待授权
)

func (k OverlayKind) Valid() bool { return k <= OverlayDeviceCode }

// DeviceFlowState 是登录浮层的阶段，语义状态而非轮询计时器。
type DeviceFlowState uint8

const (
	DeviceFlowIdle DeviceFlowState = iota
	DeviceFlowWaiting
	DeviceFlowPolling
	DeviceFlowAuthorized
	DeviceFlowDenied
	DeviceFlowExpired
)

func (s DeviceFlowState) Valid() bool { return s <= DeviceFlowExpired }

// Field 是浮层里的一个输入字段。Value 是纯文本，Secret / Multiline
// 描述语义约束，具体回显字符由 Renderer 决定。
type Field struct {
	Key       string
	Label     string
	Value     string
	Secret    bool
	Multiline bool
}

// Action 是浮层里的一个可触发动作。Command 是归一化后的命令串，
// 不携带按键绑定（按键映射属于入口适配层）。
type Action struct {
	Key     string
	Label   string
	Command string
	Primary bool
}

// Progress 是 Device Flow 浮层的语义状态。
type Progress struct {
	UserCode        string
	ExpiresAt       int64
	PollIntervalSec int
	State           DeviceFlowState
}

// Overlay 是浮层。Kind=OverlayNone 时其余字段应为空。
type Overlay struct {
	Kind     OverlayKind
	Title    string
	Body     string
	Fields   []Field
	Actions  []Action
	Progress Progress
}

// Identity 是当前身份的语义值，不含凭证。
type Identity struct {
	Guest    bool
	Username string
}

// StatusBar 是状态栏的语义值（设计稿 §3.1：online / karma / 等级）。
// Active 与 Online 独立统计：连接数不等于活跃数。
type StatusBar struct {
	Online   int
	Active   int
	Identity Identity
	Karma    int64
	Level    uint8
	Notice   Notice
	Context  string
}

// RowMeta 是一行的元信息。全部是语义值：作者用 ID 而非拼好的字符串，
// 时间存 Unix 秒而非格式化文本 —— 格式由 Renderer 决定。
type RowMeta struct {
	AuthorID    int64
	Author      string
	Node        string
	Replies     int
	CreatedAt   int64
	LastReplyAt int64
	Unread      bool
	Extra       string
}

// Row 是列表中的一行。Index 是 1-based 语义序号（键盘选择用），
// ID 是领域实体标识，供 /open、翻页等命令使用。
// Body 供详情屏承载正文；列表屏留空。
type Row struct {
	Index int
	ID    int64
	Score int64
	Title string
	Body  string
	Meta  RowMeta
}

// Selection 是焦点与选中项的语义位置，不含行列坐标或滚动像素。
type Selection struct {
	FocusedIndex int
	SelectedID   int64
	TopRowIndex  int
}

// Pagination 描述当前页在序列中的位置。FirstIndex 是本页首行的全局序号，
// 保证翻页后序号连续；Loaded 是已加载行数，不是终端可见行数。
type Pagination struct {
	FirstIndex int
	Loaded     int
	HasMore    bool
	NextCursor string
}

// Detail 是详情屏的主体内容（主题本身）。回复列表仍放在 Rows。
type Detail struct {
	ID        int64
	Title     string
	Body      string
	AuthorID  int64
	Node      string
	CreatedAt int64
}

// Screen 是 application 层输出的一屏完整语义状态。
// 两个 Renderer 订阅同一份 Screen，各自渲染，不互相解析对方的输出。
type Screen struct {
	Kind       ScreenKind
	Rows       []Row
	StatusBar  StatusBar
	Overlay    Overlay
	Selection  Selection
	Pagination Pagination
	Detail     Detail
}

// Validate 检查 Screen 的自洽性，避免 Renderer 各自猜测畸形状态。
// 这是纯语义校验：不检查长度、宽度或任何终端能力。
func (s Screen) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("非法页面种类: %d", s.Kind)
	}
	if !s.Overlay.Kind.Valid() {
		return fmt.Errorf("非法浮层种类: %d", s.Overlay.Kind)
	}
	if !s.StatusBar.Notice.Kind.Valid() {
		return fmt.Errorf("非法提示种类: %d", s.StatusBar.Notice.Kind)
	}
	if s.StatusBar.Level < 1 || s.StatusBar.Level > 4 {
		return fmt.Errorf("等级档位越界: %d", s.StatusBar.Level)
	}
	if s.Overlay.Kind == OverlayDeviceCode && !s.Overlay.Progress.State.Valid() {
		return fmt.Errorf("登录浮层缺少合法阶段: %d", s.Overlay.Progress.State)
	}

	prev := 0
	for i, r := range s.Rows {
		if r.Index <= prev {
			return fmt.Errorf("第 %d 行序号非严格递增: %d（前一行 %d）", i, r.Index, prev)
		}
		prev = r.Index
	}
	if s.Selection.FocusedIndex < 0 {
		return fmt.Errorf("焦点序号为负: %d", s.Selection.FocusedIndex)
	}
	if len(s.Rows) > 0 && s.Selection.FocusedIndex > s.Rows[len(s.Rows)-1].Index {
		return fmt.Errorf("焦点 %d 超出行范围（末行 %d）", s.Selection.FocusedIndex, s.Rows[len(s.Rows)-1].Index)
	}
	if s.Pagination.FirstIndex < 0 || s.Pagination.Loaded < 0 {
		return fmt.Errorf("分页序号为负")
	}
	return nil
}
