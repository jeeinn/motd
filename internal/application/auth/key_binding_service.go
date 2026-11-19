package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// maxKeyBindingAttempts 是并发唯一约束冲突后的整体重试上限。
// 正常路径第一次就成功；只有两个绑定真的撞上 fingerprint UNIQUE 才重试。
const maxKeyBindingAttempts = 3

// KeyBindingService 编排 §4.6 的公钥绑定 / 解绑 / 列表。
//
// 依赖分两类且严格分离：
//   - KeyBindingStore：判定与 ssh_keys 写入全在**同一个写事务**内；
//   - SessionRevoker：**只在解绑事务提交之后**调用（内存动作，不进事务）。
//
// 用例刻意不持有 SSH Session：它不改任何会话状态。P0-19 的登录升级必须
// 等 Bind 成功才把 guest 提升为 user；绑定失败时 DB 事务整体回滚，
// 会话保持访客态、不落半成品（§4.6 验收）。
type KeyBindingService struct {
	store   KeyBindingStore
	revoker SessionRevoker
	gate    *SecurityGate // 可选；P0 既有装配不注入时行为完全不变
	now     func() time.Time
}

// KeyBindingOption 注入 P1-M2 的安全门。保持无参可用于既有测试与部署。
type KeyBindingOption func(*KeyBindingService)

func WithKeyBindingGate(gate *SecurityGate) KeyBindingOption {
	return func(s *KeyBindingService) { s.gate = gate }
}

// NewKeyBindingService 构造用例。依赖为空属于装配期错误，立即 panic，
// 不能留到运行期退化成「查不到就放行」或「悄悄跳过会话撤销」。
func NewKeyBindingService(store KeyBindingStore, revoker SessionRevoker, opts ...KeyBindingOption) *KeyBindingService {
	if store == nil {
		panic("auth.NewKeyBindingService: KeyBindingStore 不能为空")
	}
	if revoker == nil {
		panic("auth.NewKeyBindingService: SessionRevoker 不能为空")
	}
	s := &KeyBindingService{store: store, revoker: revoker, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// List 返回当前账号所有未吊销的公钥绑定（读池，零写入）。
func (s *KeyBindingService) List(ctx context.Context, userID int64) ([]KeyBinding, error) {
	if userID <= 0 {
		return nil, ErrBindingRequiresLogin
	}
	return s.store.ListActive(ctx, userID)
}

// Get 查询当前账号一把公钥的详情（/keys <fingerprint>，P1-M4）。
// 指纹先过与认证同一把尺子的格式校验；归属过滤在仓储的 WHERE 里完成，
// 用例层不做「查出来再判断」，避免跨用户指纹探测。
func (s *KeyBindingService) Get(ctx context.Context, userID int64, fingerprint string) (KeyBinding, error) {
	if userID <= 0 {
		return KeyBinding{}, ErrBindingRequiresLogin
	}
	fp, err := NormalizeFingerprint(fingerprint)
	if err != nil {
		return KeyBinding{}, err
	}
	return s.store.GetByFingerprint(ctx, userID, fp)
}

// Bind 把「当前连接认证阶段服务端持有的公钥指纹」绑定到指定账号。
//
// 顺序固定：
//  1. 登录态 / 指纹格式 / 公钥文本合法性都在开事务之前校验（失败零写入）；
//  2. 单个写事务内做四态判定：未命中→插入；已吊销→拒绝；他人占用→拒绝；
//     本人占用→幂等零写入；
//  3. fingerprint UNIQUE 被并发撞上 → ErrKeyConflict → 整体重试，
//     下一轮重新查命中后转为「幂等」或「拒绝」，绝不产生第二条绑定。
//
// 事务提交成功后才返回；用例不改会话、不发事件。
func (s *KeyBindingService) Bind(ctx context.Context, req BindRequest) (BindResult, error) {
	if req.UserID <= 0 {
		return BindResult{}, ErrBindingRequiresLogin
	}
	// 安全门先判定：审计环满或按用户限流命中时，连事务都不开（零写入）。
	if err := s.gate.NoteKeyAttempt(AuditKeyBound, req.UserID, ""); err != nil {
		return BindResult{}, err
	}
	fp, err := NormalizeFingerprint(req.Fingerprint)
	if err != nil {
		return BindResult{}, err
	}
	if err := ValidatePublicKeyText(req.PublicKey); err != nil {
		return BindResult{}, err
	}

	var out BindResult
	for attempt := 0; attempt < maxKeyBindingAttempts; attempt++ {
		out = BindResult{}
		err := s.store.Write(ctx, func(tx KeyBindingWriteTx) error {
			view, err := tx.FindByFingerprint(ctx, fp)
			if err != nil {
				return fmt.Errorf("查询公钥绑定: %w", err)
			}
			if view.Found {
				if view.Revoked {
					return ErrKeyRevokedBinding
				}
				if view.UserID != req.UserID {
					return ErrKeyBoundToOtherUser
				}
				out = BindResult{Fingerprint: fp, UserID: req.UserID, Created: false}
				return nil // 幂等：零写入
			}
			if err := tx.Insert(ctx, NewKeyBinding{
				UserID:      req.UserID,
				Fingerprint: fp,
				PublicKey:   req.PublicKey,
				CreatedAt:   s.now().UTC(),
			}); err != nil {
				return err
			}
			out = BindResult{Fingerprint: fp, UserID: req.UserID, Created: true}
			return nil
		})
		if err == nil {
			return out, nil
		}
		if errors.Is(err, ErrKeyConflict) {
			continue // 事务已整体回滚，重新查命中
		}
		return BindResult{}, err
	}
	return BindResult{}, fmt.Errorf("%w（%d 次）", ErrKeyRetriesExhausted, maxKeyBindingAttempts)
}

// Unbind 软吊销当前账号的一把公钥，并在事务提交后撤销使用它的在线会话。
//
// 四条规则同时落地：
//   - 解绑只影响该钥：WHERE fingerprint = ?（精确一行）；
//   - 不能解绑别人的钥匙：WHERE user_id = 当前账号；
//   - 解绑后撤销相关会话：事务提交后调 SessionRevoker；
//   - 失败不落半成品：判定与 UPDATE 同一事务，任何错误整体回滚。
func (s *KeyBindingService) Unbind(ctx context.Context, req UnbindRequest) (UnbindResult, error) {
	if req.UserID <= 0 {
		return UnbindResult{}, ErrBindingRequiresLogin
	}
	if err := s.gate.NoteKeyAttempt(AuditKeyUnbound, req.UserID, ""); err != nil {
		return UnbindResult{}, err
	}
	fp, err := NormalizeFingerprint(req.Fingerprint)
	if err != nil {
		return UnbindResult{}, err
	}

	var revoked bool
	err = s.store.Write(ctx, func(tx KeyBindingWriteTx) error {
		n, err := tx.Revoke(ctx, req.UserID, fp, s.now().UTC())
		if err != nil {
			return fmt.Errorf("吊销公钥: %w", err)
		}
		if n == 1 {
			revoked = true
			return nil
		}
		// 0 行：区分「不存在」「属于别人」「此前已吊销（幂等）」。
		view, err := tx.FindByFingerprint(ctx, fp)
		if err != nil {
			return fmt.Errorf("复查公钥绑定: %w", err)
		}
		switch {
		case !view.Found:
			return ErrKeyNotFound
		case view.UserID != req.UserID:
			return ErrKeyBoundToOtherUser
		case view.Revoked:
			revoked = false
			return nil
		default:
			return ErrKeyConflict
		}
	})
	if err != nil {
		return UnbindResult{}, err
	}

	// 事务已提交，才撤销在线会话（内存动作不可回滚，故绝不能放进 Write 闭包）。
	closed := s.revoker.RevokeSessions(ctx, fp)
	// 审计在撤销之后：撤销已生效，审计入队失败只上报，不回滚已关闭的会话。
	if err := s.gate.NoteSessionRevoked(req.UserID, "", closed); err != nil {
		return UnbindResult{Fingerprint: fp, Revoked: revoked, SessionsClosed: closed}, err
	}
	return UnbindResult{Fingerprint: fp, Revoked: revoked, SessionsClosed: closed}, nil
}
