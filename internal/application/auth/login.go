package auth

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// 登录事务的领域口径（评审稿 v0.2 §4.3 / §4.5 / §5.1，P0-17）。
//
// 身份主键是 (provider, provider_uid)，其中 provider_uid 对 GitHub 取数字 id
// 的字符串，绝不用 login / email（两者都可被用户改写）。邮箱只作兜底与恢复通道，
// 不参与账号归并（§8 风险表「账号接管：邮箱自动合并」/ §7 R7）。

const (
	// ProviderGitHub 是 P0 唯一启用的身份源；DDL 的 CHECK 还留了 email 供 P1。
	ProviderGitHub = "github"
	// maxUsernameRunes 与 domain/user.ValidateUsername 的 {3,20} 上界一致。
	// 追加数字后缀时会先按这个上界截断基准名，避免候选名越界。
	maxUsernameRunes = 20
	// DefaultUsernameAttempts 是用户名冲突时的最大候选数（base, base2, ...）。
	DefaultUsernameAttempts = 20
)

var (
	// ErrInvalidIdentityProfile 表示 GitHub 资料无法构成合法登录输入。
	ErrInvalidIdentityProfile = errors.New("GitHub 身份档案不完整")
	// ErrEmptyToken 表示调用方没有把 access_token 传进来。
	ErrEmptyToken = errors.New("access_token 为空，无法登录")
	// ErrLoginNotFound 是端口的「未命中」哨兵；用例据此推进 §4.3 的下一步。
	ErrLoginNotFound = errors.New("登录记录未命中")
	// ErrAccountInactive 表示身份命中但账号状态不允许登录（失败关闭）。
	ErrAccountInactive = errors.New("账号状态不允许登录")
	// ErrIdentityConflict 是 identities 唯一约束被并发首登撞上，触发整体重试。
	ErrIdentityConflict = errors.New("身份已被并发登录占用，重试中")
	// ErrUsernameConflict 是 users.username 唯一约束被并发撞上，触发整体重试。
	ErrUsernameConflict = errors.New("用户名已被并发占用，重试中")
	// ErrUsernameExhausted 表示用户名候选全部被占用。
	ErrUsernameExhausted = errors.New("用户名候选全部被占用")
	// ErrLoginRetriesExhausted 表示并发冲突重试次数用尽。
	ErrLoginRetriesExhausted = errors.New("登录并发冲突重试次数耗尽")
)

// EmailOccupiedNotice 是 §4.3 步骤 2 的「不自动合并」提示。
//
// 刻意做成**不带任何外部数据的固定文案**：邮箱、login 都来自 GitHub 响应，
// 一旦拼进提示就可能把控制字符带进终端（P0-23 的分层渲染是展示侧防线，
// 这里是错误构造侧防线）。调用方拿到 Notice 后自行经白名单渲染展示。
const EmailOccupiedNotice = "检测到该邮箱已属于其他账号，本次已为你创建独立账号（未自动合并）；如需关联，请先登录原账号再使用 /link"

// IdentityProfile 是从 GitHub 资料归一化出的登录输入，**不含 token**。
// token 只在 LoginService 里进程内用于两个 GET，绝不进入任何持久化结构。
type IdentityProfile struct {
	Provider          string
	ProviderUID       string // GitHub 数字 id 的字符串
	PreferredUsername string // 从 login 派生、已过 ValidateUsername
	Login             string // 原始 login，仅作 display_name 落库
	Email             string // 仅 primary && verified，已 normalize；无则空
	EmailVerified     bool
}

// LoginResult 是登录事务的结果。Created=false 表示命中已有身份直接登录。
// Notice 非空时表示「邮箱已属于其他账号」，但不影响本次账号创建（§7 R16）。
type LoginResult struct {
	UserID   int64
	Username string
	Created  bool
	Notice   string
}

// NewIdentityProfile 把 GitHub 的 /user 与 /user/emails 归一化成登录输入。
// 任何不完整的资料都在开写事务之前拒绝，不给数据库留半成品。
func NewIdentityProfile(g GitHubUser, emails []GitHubEmail) (IdentityProfile, error) {
	if g.ID <= 0 {
		return IdentityProfile{}, fmt.Errorf("%w: 数字 id 非法（%d）", ErrInvalidIdentityProfile, g.ID)
	}
	uid := strconv.FormatInt(g.ID, 10)
	base, err := DeriveUsername(g.Login, uid)
	if err != nil {
		return IdentityProfile{}, err
	}
	email, verified := SelectPrimaryVerifiedEmail(emails)
	return IdentityProfile{
		Provider:          ProviderGitHub,
		ProviderUID:       uid,
		PreferredUsername: base,
		Login:             g.Login,
		Email:             email,
		EmailVerified:     verified,
	}, nil
}

// SelectPrimaryVerifiedEmail 只挑 primary && verified 的邮箱并 normalize。
//
// GitHub 保证至多一个 primary；未验证 / 非 primary / 格式非法的邮箱一律忽略，
// 返回 ("", false)。没有验证邮箱**不阻断登录**：邮箱是恢复通道而不是身份主键，
// 但步骤 2 的「邮箱命中已有账号」判定只在本函数返回 true 时才进行。
func SelectPrimaryVerifiedEmail(emails []GitHubEmail) (string, bool) {
	for _, e := range emails {
		if !e.Primary || !e.Verified {
			continue
		}
		n := strings.ToLower(strings.TrimSpace(e.Email))
		if !validEmail(n) {
			continue
		}
		return n, true
	}
	return "", false
}

// validEmail 做最小语法校验（只用于降低脏数据入库概率，不追求完整 RFC）。
// 明确排除空白与控制字符，避免邮箱值被拼进终端提示时形成注入面。
func validEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') || at == len(s)-1 {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// DeriveUsername 把 GitHub login 派生成本站合法用户名。
//
// GitHub login 的字符集比本站窄，但仍可能超长、命中保留名或被恶意构造成空串。
// 流程：清洗非法字符 → 截断到 20 → 不足 3 字符时优先补下划线（保留中文可读性，
// 例如「张三」→「张三_」），清洗后为空才回退 gh+uid → 保留名追加 _0 →
// 再截断 → **必须过 domain/user.ValidateUsername**（ADR-005 的唯一事实来源）。
func DeriveUsername(login, uid string) (string, error) {
	clean := truncateRunes(sanitizeUsernameRunes(login), maxUsernameRunes)
	if n := utf8.RuneCountInString(clean); n < 3 {
		if clean == "" {
			// 清洗后什么都不剩：只能用数字 id 兜底，保证一定合法。
			clean = truncateRunes(sanitizeUsernameRunes("gh"+uid), maxUsernameRunes)
		} else {
			// 1–2 个合法字符（常见于两字中文 login）：补下划线而不是丢弃原名。
			clean += strings.Repeat("_", 3-n)
		}
	}
	clean = truncateRunes(domainuser.ResolveUsername(clean), maxUsernameRunes)
	if err := domainuser.ValidateUsername(clean); err != nil {
		return "", fmt.Errorf("%w: 派生用户名 %q 不合法: %v", ErrInvalidIdentityProfile, clean, err)
	}
	return clean, nil
}

// sanitizeUsernameRunes 只保留与 domain 正则 ^[\p{Han}a-zA-Z0-9_-]{3,20}$ 一致的字符。
func sanitizeUsernameRunes(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case unicode.Is(unicode.Han, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// truncateRunes 按 Unicode 码点截断（中文一字算一个），与 SQLite length(TEXT)
// 及 domain 正则的计数口径一致，避免按字节截断切出半个 UTF-8 字符。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// AllocateUsername 用 occupied 逐个探测候选名，返回第一个未被占用的合法用户名。
// 候选序列是 base, base2, base3, ...；加后缀时按 20 码点上界先截断基准名，
// 保证「超长 login + 后缀」仍然合法。occupied 由调用方绑定到当前写事务内。
func AllocateUsername(base string, occupied func(string) (bool, error), attempts int) (string, error) {
	if occupied == nil {
		return "", errors.New("用户名占用探测函数为空")
	}
	if attempts <= 0 {
		attempts = DefaultUsernameAttempts
	}
	for i := 0; i < attempts; i++ {
		candidate := usernameCandidate(base, i)
		taken, err := occupied(candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			if err := domainuser.ValidateUsername(candidate); err != nil {
				return "", fmt.Errorf("%w: 候选 %q 不合法: %v", ErrInvalidIdentityProfile, candidate, err)
			}
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w（基准 %q，尝试 %d 次）", ErrUsernameExhausted, base, attempts)
}

// usernameCandidate 生成第 i 个候选（i=0 即基准名）。
func usernameCandidate(base string, i int) string {
	if i == 0 {
		return base
	}
	suffix := strconv.Itoa(i + 1) // base2, base3, ...
	room := maxUsernameRunes - len(suffix)
	return truncateRunes(base, room) + suffix
}
