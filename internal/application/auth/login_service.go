package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// maxLoginAttempts 是并发唯一约束冲突后的整体重试上限。
// 正常路径第一次就成功；只有两个首登真的撞上 UNIQUE 时才走重试，
// 重试会重新执行 FindIdentityByUID，命中即转为「登录」而不是重复建号。
const maxLoginAttempts = 3

// LoginService 编排 §4.3 的账号创建与登录事务。
//
// 依赖分两类且严格分离：
//   - DeviceFlowClient：只在**写事务之外**调用 GetUser / GetEmails（网络）；
//   - LoginStore：三步判定 + users/identities 原子创建全在**同一个写事务**内。
//
// token 只在 Login 的参数里进程内流转，绝不进入 LoginResult、日志或任何持久化结构。
type LoginService struct {
	client DeviceFlowClient
	store  LoginStore
	now    func() time.Time
}

var _ = (*LoginService)(nil)

// NewLoginService 构造登录用例。依赖为空属于装配期错误，立即 panic，
// 不能留到运行期退化成「查不到就放行」或「建个空账号」。
func NewLoginService(client DeviceFlowClient, store LoginStore) *LoginService {
	if client == nil {
		panic("auth.NewLoginService: DeviceFlowClient 不能为空")
	}
	if store == nil {
		panic("auth.NewLoginService: LoginStore 不能为空")
	}
	return &LoginService{client: client, store: store, now: time.Now}
}

// Login 用 access_token 换取 GitHub 身份，再按 §4.3 完成登录或注册。
//
// 顺序固定：
//  1. token 空 → 直接拒绝（不触网、不开事务）；
//  2. GetUser + GetEmails（网络，事务外）；任一失败不改任何状态；
//  3. 归一化为 IdentityProfile（数字 id / 只收 primary+verified 邮箱 / 派生用户名）；
//  4. 单个写事务内执行三步判定，唯一约束冲突则整体重试。
//
// §4.3 三步（结合 §7 R16 的口径消解）：
//  1. (provider, provider_uid) 命中 → 登录（账号须 active，失败关闭）；
//  2. 未命中但已验证邮箱命中其他账号 → **不自动合并、不阻断**，仅返回 Notice；
//  3. 都为新 UID 原子创建 users + identities（R7：primary_email 无唯一约束，
//     因此「邮箱相同但 UID 不同」必然建成两个账号，满足验收）。
func (s *LoginService) Login(ctx context.Context, token string) (LoginResult, error) {
	tok := strings.TrimSpace(token)
	if tok == "" {
		return LoginResult{}, ErrEmptyToken
	}

	// 网络调用一律在写事务之外（ADR-022）。两次 GET 都不占唯一写连接。
	g, err := s.client.GetUser(ctx, tok)
	if err != nil {
		return LoginResult{}, fmt.Errorf("获取 GitHub 用户资料失败: %w", err)
	}
	emails, err := s.client.GetEmails(ctx, tok)
	if err != nil {
		return LoginResult{}, fmt.Errorf("获取 GitHub 邮箱列表失败: %w", err)
	}

	profile, err := NewIdentityProfile(g, emails)
	if err != nil {
		return LoginResult{}, err
	}

	return s.loginWithProfile(ctx, profile)
}

// loginWithProfile 把三步判定放进一个可重试的写事务循环。
// 抽成独立方法是为了让「同 UID 并发首登」与「邮箱命中提示」可以脱离网络测试。
func (s *LoginService) loginWithProfile(ctx context.Context, profile IdentityProfile) (LoginResult, error) {
	var out LoginResult
	for attempt := 0; attempt < maxLoginAttempts; attempt++ {
		err := s.store.Write(ctx, func(tx LoginWriteTx) error {
			res, err := s.decideAndCreate(ctx, tx, profile)
			if err != nil {
				return err
			}
			out = res
			return nil
		})
		if err == nil {
			return out, nil
		}
		if isRetryableLoginConflict(err) {
			// 事务已整体回滚，没有半成品；下一轮重新查身份，命中即登录。
			continue
		}
		return LoginResult{}, err
	}
	return LoginResult{}, fmt.Errorf("%w（%d 次）", ErrLoginRetriesExhausted, maxLoginAttempts)
}

// decideAndCreate 是单个写事务内的 §4.3 判定，不涉及任何网络调用。
func (s *LoginService) decideAndCreate(ctx context.Context, tx LoginWriteTx, p IdentityProfile) (LoginResult, error) {
	// 步骤 1：(provider, provider_uid) 命中即登录。
	existing, err := tx.FindIdentityByUID(ctx, p.Provider, p.ProviderUID)
	if err == nil {
		if existing.Status != domainuser.StatusActive {
			return LoginResult{}, fmt.Errorf("%w: 账号 %d 状态 %q", ErrAccountInactive, existing.UserID, existing.Status)
		}
		return LoginResult{
			UserID:   existing.UserID,
			Username: existing.Username,
			Created:  false,
		}, nil
	}
	if !errors.Is(err, ErrLoginNotFound) {
		return LoginResult{}, fmt.Errorf("查询身份绑定: %w", err)
	}

	// 步骤 2：邮箱命中已有账号 → 不自动合并、不复用、不阻断，只生成 Notice。
	// 只在有 primary+verified 邮箱时才查；Notice 是固定文案，不含邮箱原文。
	var notice string
	if p.EmailVerified {
		_, err := tx.FindAccountByEmail(ctx, p.Email)
		if err == nil {
			notice = EmailOccupiedNotice
		} else if !errors.Is(err, ErrLoginNotFound) {
			return LoginResult{}, fmt.Errorf("查询邮箱归属: %w", err)
		}
	}

	// 步骤 3：原子创建 users + identities。用户名冲突由 NOCASE UNIQUE 兜底，
	// 先在同一事务内探测并追加数字后缀，再把两张表一起写。
	username, err := AllocateUsername(p.PreferredUsername, func(name string) (bool, error) {
		return tx.UsernameExists(ctx, name)
	}, DefaultUsernameAttempts)
	if err != nil {
		return LoginResult{}, err
	}

	now := s.now().UTC()
	userID, err := tx.InsertUser(ctx, NewAccount{
		Username:     username,
		DisplayName:  p.Login,
		PrimaryEmail: p.Email,
		Status:       domainuser.StatusActive,
		CreatedAt:    now,
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("创建账号: %w", err)
	}
	if err := tx.InsertIdentity(ctx, NewIdentity{
		UserID:        userID,
		Provider:      p.Provider,
		ProviderUID:   p.ProviderUID,
		Email:         p.Email,
		EmailVerified: p.EmailVerified,
		CreatedAt:     now,
	}); err != nil {
		return LoginResult{}, fmt.Errorf("绑定身份: %w", err)
	}

	return LoginResult{
		UserID:   userID,
		Username: username,
		Created:  true,
		Notice:   notice,
	}, nil
}

// isRetryableLoginConflict 判定是否属于「并发撞唯一约束」这类可整体重试的错误。
// 其余错误（脏数据、账号停用、网络、写队列故障）一律不重试，避免掩盖真实缺陷。
func isRetryableLoginConflict(err error) bool {
	return errors.Is(err, ErrIdentityConflict) || errors.Is(err, ErrUsernameConflict)
}
