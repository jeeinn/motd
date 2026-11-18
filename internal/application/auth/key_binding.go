package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 公钥绑定领域口径（评审稿 v0.2 §4.6，P0-18）。
//
// 核心约束：绑定的是「认证阶段服务端已持有的 ssh.PublicKey 的指纹」，
// **不接受客户端提交的公钥文本**。指纹与公钥文本都只能来自 transport/ssh
// 的 PublicKeyCallback —— 客户端必须先持有私钥通过公钥挑战，服务端才拿到
// ssh.PublicKey，再由它算出指纹（FingerprintSHA256）并序列化出公钥文本
// （MarshalAuthorizedKey）。application 层既不解析公钥，也不从命令参数
// 反推指纹；这条边界由 archtest 静态守卫固化。
//
// 另一条边界：/key rm 的参数是「指纹」这种标识符，不是公钥文本。它必须先过
// ValidFingerprint（与认证阶段同一把尺子），因此「别人的公钥字符串」根本
// 进不了用例 —— 这正是 P0-18 验收要挡住的伪造路径。

// maxPublicKeyTextRunes 限制服务端序列化的公钥文本长度，避免脏数据撑大库行。
const maxPublicKeyTextRunes = 8192

var (
	// ErrBindingRequiresLogin 表示访客试图绑定 / 解绑公钥（UserID 必须 > 0）。
	ErrBindingRequiresLogin = errors.New("绑定公钥需要登录态")
	// ErrInvalidPublicKey 表示服务端持有的公钥文本缺失或含控制字符。
	ErrInvalidPublicKey = errors.New("服务端持有的公钥文本不合法")
	// ErrKeyBoundToOtherUser 表示指纹已被另一账号占用（§4.6 一钥只属一号）。
	ErrKeyBoundToOtherUser = errors.New("该公钥已属于其他账号")
	// ErrKeyRevokedBinding 表示指纹命中但已吊销；吊销是安全动作，P0 不提供重新激活。
	ErrKeyRevokedBinding = errors.New("该公钥已吊销，不允许重新绑定")
	// ErrKeyNotFound 表示解绑的指纹在 ssh_keys 里不存在。
	ErrKeyNotFound = errors.New("未找到该公钥绑定")
	// ErrKeyConflict 是 fingerprint UNIQUE 被并发绑定撞上，触发整体重试。
	ErrKeyConflict = errors.New("公钥绑定并发冲突，重试中")
	// ErrKeyRetriesExhausted 表示并发冲突重试次数用尽。
	ErrKeyRetriesExhausted = errors.New("公钥绑定并发冲突重试次数耗尽")
	// ErrKeySubcommand 表示 /key 的子命令或参数形态不合法。
	ErrKeySubcommand = errors.New("公钥子命令参数不合法")
)

// KeySubcommand 是 /key 的受限子命令枚举。
type KeySubcommand uint8

const (
	KeySubUnknown KeySubcommand = iota
	KeySubAdd
	KeySubRemove
)

// KeyCommand 是 /key 参数解析后的结果。
//
// 刻意**不携带公钥文本**：Add 的指纹 / 公钥文本只能由调用方从服务端
// Session 读取；Remove 只带一个已经过格式校验的指纹标识符。
type KeyCommand struct {
	Kind        KeySubcommand
	Fingerprint string // 仅 KeySubRemove 有效
}

// ParseKeyCommand 把 /key 的参数解析成受限指令。
//
// 关键规则：`add` 必须**恰好 1 个参数**。多出来的任何 token（包括伪造的
// "ssh-rsa AAAA..." 公钥字符串）一律拒绝，从结构上保证「客户端提交公钥
// 文本」没有进入绑定路径的入口。命令表本身的 MaxArgs 只是粗筛，真正的
// 语义收口在这里（P0-18 验收：伪造提交别人的公钥字符串无法完成绑定）。
func ParseKeyCommand(args []string) (KeyCommand, error) {
	if len(args) == 0 {
		return KeyCommand{}, fmt.Errorf("%w: 缺少子命令", ErrKeySubcommand)
	}
	switch args[0] {
	case "add":
		if len(args) != 1 {
			return KeyCommand{}, fmt.Errorf("%w: /key add 不接受任何公钥文本参数", ErrKeySubcommand)
		}
		return KeyCommand{Kind: KeySubAdd}, nil
	case "rm":
		if len(args) != 2 {
			return KeyCommand{}, fmt.Errorf("%w: 用法 /key rm <fingerprint>", ErrKeySubcommand)
		}
		fp, err := NormalizeFingerprint(args[1])
		if err != nil {
			return KeyCommand{}, fmt.Errorf("%w: %v", ErrKeySubcommand, err)
		}
		return KeyCommand{Kind: KeySubRemove, Fingerprint: fp}, nil
	default:
		return KeyCommand{}, fmt.Errorf("%w: 未知子命令", ErrKeySubcommand)
	}
}

// NormalizeFingerprint 校验并归一化指纹。复用 P0-14 的 ValidFingerprint，
// 保证「认证阶段算出的格式」与「/key rm 参数格式」用同一把尺子；
// 任何公钥文本（ssh-rsa AAAA... 等）都不会通过。
func NormalizeFingerprint(raw string) (string, error) {
	fp := strings.TrimSpace(raw)
	if fp == "" {
		return "", ErrEmptyFingerprint
	}
	if !ValidFingerprint(fp) {
		return "", ErrInvalidFingerprint
	}
	return fp, nil
}

// ValidatePublicKeyText 对服务端序列化的公钥文本做最小落库校验：
// 非空、单行、无控制字符 / NUL、长度有上限。P0 **不解析**公钥内容，
// 也不把它写回终端；真实来源由 archtest 守卫限定为 Session.PublicKey()。
func ValidatePublicKeyText(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("%w: 为空", ErrInvalidPublicKey)
	}
	if utf8.RuneCountInString(s) > maxPublicKeyTextRunes {
		return fmt.Errorf("%w: 超过 %d 字符", ErrInvalidPublicKey, maxPublicKeyTextRunes)
	}
	for _, r := range s {
		if r == 0 || unicode.IsControl(r) {
			return fmt.Errorf("%w: 含控制字符", ErrInvalidPublicKey)
		}
	}
	return nil
}

// BindRequest 是一次绑定输入。
//
// Fingerprint / PublicKey 必须来自服务端认证阶段（Session.Fingerprint() /
// Session.PublicKey()），绝不是命令参数或客户端提交的公钥文本。
// UserID 来自会话登录态（/key add）或 LoginService.Login 的结果（P0-19 自动绑定）。
type BindRequest struct {
	UserID      int64
	Fingerprint string
	PublicKey   string
}

// BindResult 是绑定结果。Created=false 表示该指纹此前已绑定给同一账号（幂等）。
type BindResult struct {
	Fingerprint string
	UserID      int64
	Created     bool
}

// UnbindRequest 是一次解绑输入。Fingerprint 来自 /key rm 的参数，
// 但已在 ParseKeyCommand / NormalizeFingerprint 处过了严格格式校验。
type UnbindRequest struct {
	UserID      int64
	Fingerprint string
}

// UnbindResult 是解绑结果。
// Revoked=false 表示此前已吊销（幂等）；SessionsClosed 是事务提交后
// 被撤销的在线会话数（纯内存动作，不计入数据库事务）。
type UnbindResult struct {
	Fingerprint    string
	Revoked        bool
	SessionsClosed int
}

// KeyBinding 是 /keys 列表的一行，数据源是 ssh_keys 表而非用户输入。
// LastUsedAt 为零值时 HasLastUsed=false；P0 只展示 Fingerprint。
type KeyBinding struct {
	Fingerprint   string
	Label         string
	CreatedAtUnix int64
	LastUsedUnix  int64
	HasLastUsed   bool
}
