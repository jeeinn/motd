package forum

// R-15 / ADR-079 主题置顶用例：管理员把已发布主题设为置顶/取消置顶。
// 权限只走 requireAdmin 单点（ADR-049），审计复用 adminActionEvent → outbox。
// 每版块最多 1 条置顶：PinThread 在同一写事务内先清同版块旧置顶再设新置顶。

import (
	"context"
	"fmt"

	"github.com/jeeinn/motd/internal/domain/event"
)

// PinThread 把主题设为置顶。返回该主题所属版块 ID 供 cmd 刷新对应列表。
// 权限判定在本入口直接过 requireAdmin 单点（archtest 守卫用例入口必须直调此谓词）。
func (s *Service) PinThread(ctx context.Context, actor Actor, threadID int64) (int64, error) {
	if err := s.requireAdmin(actor); err != nil {
		return 0, err
	}
	return s.setThreadMark(ctx, actor, threadID, 1, "thread pin")
}

// UnpinThread 取消主题置顶。权限同样在本入口过单点。
func (s *Service) UnpinThread(ctx context.Context, actor Actor, threadID int64) (int64, error) {
	if err := s.requireAdmin(actor); err != nil {
		return 0, err
	}
	return s.setThreadMark(ctx, actor, threadID, 0, "thread unpin")
}

func (s *Service) setThreadMark(ctx context.Context, actor Actor, threadID int64, mark int8, action string) (int64, error) {
	if threadID <= 0 {
		return 0, fmt.Errorf("%w: 主题 ID 非法", ErrNotFound)
	}

	// 先读 nodeID（低频治理，主题版块不可变——节点移动已否决）。
	t, err := s.repos.GetThread(ctx, threadID)
	if err != nil {
		return 0, err
	}

	now := s.now().UTC()
	var ev event.DomainEvent
	err = s.repos.Write(ctx, func(tx WriteTx) error {
		// 写事务内复核主题仍存在（防 TOCTOU：排队期间被删除）；
		// 可见性由 SetThreadMark 的 status='active' 条件兜底。
		exists, err := tx.ThreadExists(ctx, threadID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: 主题不存在", ErrNotFound)
		}
		if mark == 1 {
			// 每版块 1 条：先清旧置顶，再设新置顶，同事务原子替换。
			if err := tx.ClearPinnedInNode(ctx, t.NodeID, threadID); err != nil {
				return err
			}
		}
		if err := tx.SetThreadMark(ctx, threadID, mark); err != nil {
			return err
		}
		ev = adminActionEvent(action, fmt.Sprintf("thread:%d", threadID), t.NodeID, actor, now)
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.afterCommit(ctx, ev)
	return t.NodeID, nil
}
