package forum

// P2-M1b 内容治理：管理员隐藏他人主题 / 回复。
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
