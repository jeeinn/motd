package forum

// P2-M1b 内容治理：管理员隐藏他人主题 / 回复。
// ADR-053 追加 Unhide：误隐藏可逆，恢复走与 Hide 完全对称的口径。
//
// 三条不变量写在这里，命令层不得复制：
//  1. 准入只有 requireAdmin 一个入口（ADR-049）：白名单指纹即授权，
//     与作者身份、账号状态无关；管理员隐藏自己的内容同样允许。
//  2. hidden 是治理动作，deleted 是作者自删：两者都是软删不物理删，
//     但语义、审计命令名不同（ADR-050）。公开可见性由存储层 status 过滤天然保证。
//  3. 目标不存在 / 已被作者删除一律收敛为 ErrNotFound（固定文案）；
//     已 hidden 的目标重复隐藏是合法 no-op，便于重试。绝不回显目标原文 / ID 文本。
//
// 这里刻意不调 actor.checkWrite()：它会把访客（UserID=0）挡在门外，
// 而白名单内的访客正是 bootstrap 管理员（与节点治理同一口径）。

import (
	"context"
	"strconv"

	"github.com/jeeinn/motd/internal/domain/event"
)

// HidePost 隐藏一条回复。threadID 不由用例解析：命令层用当前打开的主题导航，
// 与 Delete(DeletePost) 的口径一致；治理权本身就覆盖全部内容，不需要所有权判定。
func (s *Service) HidePost(ctx context.Context, actor Actor, postID int64) error {
	if err := s.requireAdmin(actor); err != nil {
		return err
	}
	if postID <= 0 {
		return ErrNotFound // 固定文案：不回显参数，不区分「非法」与「不存在」
	}

	now := s.now().UTC()
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		// 条件更新即存在性判定：不存在或作者已删 → affected=0 → ErrNotFound；
		// 已 hidden → 幂等成功。检查与写入同一事务快照，无 TOCTOU 窗口。
		if err := tx.HidePost(ctx, postID); err != nil {
			return err
		}
		ev = adminActionEvent("post hide", "post:"+strconv.FormatInt(postID, 10), 0, actor, now)
		ev.PostID = postID
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.afterCommit(ctx, ev)
	return nil
}

// HideThread 隐藏整个主题。只改 threads.status，不级联改回复行
// （与 SoftDeleteThread 同构），回复随主题从公开视图消失但留在库里。
func (s *Service) HideThread(ctx context.Context, actor Actor, threadID int64) error {
	if err := s.requireAdmin(actor); err != nil {
		return err
	}
	if threadID <= 0 {
		return ErrNotFound
	}

	now := s.now().UTC()
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		if err := tx.HideThread(ctx, threadID); err != nil {
			return err
		}
		ev = adminActionEvent("thread hide", "thread:"+strconv.FormatInt(threadID, 10), 0, actor, now)
		ev.ThreadID = threadID
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.afterCommit(ctx, ev)
	return nil
}

// UnhidePost 恢复一条被隐藏的回复。与 HidePost 同构：入口 requireAdmin、
// 单写事务、审计同事务 AppendOutbox；固定文案不回显目标原文 / ID。
// 未隐藏的目标（status=normal）是合法 no-op，便于重试；作者已删的目标仍不可恢复。
func (s *Service) UnhidePost(ctx context.Context, actor Actor, postID int64) error {
	if err := s.requireAdmin(actor); err != nil {
		return err
	}
	if postID <= 0 {
		return ErrNotFound
	}

	now := s.now().UTC()
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		// 条件更新即存在性判定：deleted / 不存在 → ErrNotFound；
		// hidden → 恢复；已 normal → 幂等成功。检查与写入同一快照。
		if err := tx.UnhidePost(ctx, postID); err != nil {
			return err
		}
		ev = adminActionEvent("post unhide", "post:"+strconv.FormatInt(postID, 10), 0, actor, now)
		ev.PostID = postID
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.afterCommit(ctx, ev)
	return nil
}

// UnhideThread 恢复整个主题。只改 threads.status 回 active，不级联改回复行
// （隐藏时也没改；与 HideThread / SoftDeleteThread 同构）。
func (s *Service) UnhideThread(ctx context.Context, actor Actor, threadID int64) error {
	if err := s.requireAdmin(actor); err != nil {
		return err
	}
	if threadID <= 0 {
		return ErrNotFound
	}

	now := s.now().UTC()
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		if err := tx.UnhideThread(ctx, threadID); err != nil {
			return err
		}
		ev = adminActionEvent("thread unhide", "thread:"+strconv.FormatInt(threadID, 10), 0, actor, now)
		ev.ThreadID = threadID
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.afterCommit(ctx, ev)
	return nil
}

// Unthread 是 ADR-053 补丁：从任意屏幕按 ID 恢复被隐藏的**主题**。
// 与 UnhideThread 的差别只有两个：① 不要求当前处于该主题详情页——隐藏主题
// 已被 GetThread 过滤，根本打不开详情；② 审计命令名固定为 "thread unthread"，
// 与无参 /unhide 的 "thread unhide" 分开，审计能区分入口，event ID 的
// 幂等键也不会互相干扰。写端口仍复用 UnhideThread（幂等、只 UPDATE status）。
func (s *Service) Unthread(ctx context.Context, actor Actor, threadID int64) error {
	if err := s.requireAdmin(actor); err != nil {
		return err
	}
	if threadID <= 0 {
		return ErrNotFound
	}

	now := s.now().UTC()
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		if err := tx.UnhideThread(ctx, threadID); err != nil {
			return err
		}
		ev = adminActionEvent("thread unthread", "thread:"+strconv.FormatInt(threadID, 10), 0, actor, now)
		ev.ThreadID = threadID
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.afterCommit(ctx, ev)
	return nil
}
