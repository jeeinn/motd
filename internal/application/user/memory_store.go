package user

import (
	"context"
	"fmt"
	"sync"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// Grant 是一条已解锁成就的内存记录，供测试与运维观测。
type Grant struct {
	UserID     int64
	Code       domainuser.AchievementCode
	UnlockedAt time.Time
}

// MemoryAchievementStore 是 P0 的内存占位实现：不写 SQLite。
//
// 成就系统属 P1 范围（P0-PLAN §1.1），但插件链路必须在 P0 就能端到端验证，
// 因此这里提供一个满足 AchievementStore 的可观测实现。
// P1 只需换成读写 achievements 表的实现，application/user 的校验逻辑不变。
type MemoryAchievementStore struct {
	mu      sync.Mutex
	grants  map[string]Grant
	failPut error // 注入写入失败，用于验证不产生半状态
}

func NewMemoryAchievementStore() *MemoryAchievementStore {
	return &MemoryAchievementStore{grants: make(map[string]Grant)}
}

// SetGrantFailure 仅用于测试注入持久化失败。
func (s *MemoryAchievementStore) SetGrantFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPut = err
}

func grantKey(userID int64, code domainuser.AchievementCode) string {
	return fmt.Sprintf("%d:%s", userID, code)
}

func (s *MemoryAchievementStore) Has(ctx context.Context, userID int64, code domainuser.AchievementCode) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.grants[grantKey(userID, code)]
	return ok, nil
}

func (s *MemoryAchievementStore) Grant(ctx context.Context, userID int64, code domainuser.AchievementCode, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut != nil {
		return s.failPut
	}
	key := grantKey(userID, code)
	if _, exists := s.grants[key]; exists {
		return nil // 幂等：重复授予不产生第二条记录
	}
	s.grants[key] = Grant{UserID: userID, Code: code, UnlockedAt: at}
	return nil
}

func (s *MemoryAchievementStore) GrantedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.grants)
}

// Entries 返回快照副本，外部修改不影响内部。
func (s *MemoryAchievementStore) Entries() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Grant, 0, len(s.grants))
	for _, g := range s.grants {
		out = append(out, g)
	}
	return out
}
