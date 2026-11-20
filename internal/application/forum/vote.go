package forum

// 投票 / 撤销票用例（P1-M3）。
//
// 事务纪律：一个投票动作 = 一次 Write。权限复核、目标存在性、旧票读取、
// 票数增量、写票、改分、追加事件全部落在同一事务快照内。
//
// 最关键的约束：投票走 AdjustScore，**绝不调用 TouchThread**。
// TouchThread 会把 last_reply_at 一起推进，投票一旦复用它，旧主题就会被顶到
// SortLatest 的最前面。分数与「最后回复时间」是两个正交事实。

import (
	"context"
	"fmt"

	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/vote"
)

// VoteResult 是一次投票动作的语义结果。Value=0 表示已撤销；
// Delta 是 threads.score 的实际增量，同值重复投票时为 0（幂等）。
type VoteResult struct {
	TargetType vote.TargetType
	TargetID   int64
	ThreadID   int64
	Value      vote.Value
	Delta      int64
}

// CastVote 给 thread / post 投 up(+1) 或 down(-1)。
// 同一用户对同一目标改票时只记增量，不累加两次投票。
func (s *Service) CastVote(ctx context.Context, actor Actor, targetType vote.TargetType, targetID int64, value vote.Value) (VoteResult, error) {
	if !targetType.Valid() {
		return VoteResult{}, ErrInvalidVoteTarget
	}
	if !value.Valid() {
		return VoteResult{}, ErrInvalidVote
	}
	if err := actor.checkWrite(); err != nil {
		return VoteResult{}, err
	}
	return s.applyVote(ctx, actor, targetType, targetID, value)
}

// Unvote 撤销当前用户在该目标上的投票；本来没票则是幂等空操作。
func (s *Service) Unvote(ctx context.Context, actor Actor, targetType vote.TargetType, targetID int64) (VoteResult, error) {
	if !targetType.Valid() {
		return VoteResult{}, ErrInvalidVoteTarget
	}
	if err := actor.checkWrite(); err != nil {
		return VoteResult{}, err
	}
	return s.applyVote(ctx, actor, targetType, targetID, 0)
}

// applyVote 是 CastVote / Unvote 的共用事务路径。to=0 表示撤销。
func (s *Service) applyVote(ctx context.Context, actor Actor, targetType vote.TargetType, targetID int64, to vote.Value) (VoteResult, error) {
	if targetID <= 0 {
		return VoteResult{}, fmt.Errorf("%w: 投票目标 ID 非法", ErrNotFound)
	}
	now := s.now().UTC()

	var res VoteResult
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		// 会话里的 status 是快照；排队期间账号可能被冻结，必须在事务内复核。
		if err := s.checkWritePermission(ctx, tx, actor.UserID); err != nil {
			return err
		}
		threadID, err := s.voteThreadID(ctx, tx, targetType, targetID)
		if err != nil {
			return err
		}
		old, had, err := tx.GetVote(ctx, actor.UserID, targetType, targetID)
		if err != nil {
			return err
		}
		current := vote.Value(0)
		if had {
			current = old
		}
		delta := int64(to) - int64(current)
		res = VoteResult{
			TargetType: targetType, TargetID: targetID,
			ThreadID: threadID, Value: to, Delta: delta,
		}
		// 同值重复提交：不加分、不重写投票、不产生事件（幂等）。
		if delta == 0 {
			return nil
		}
		if to == 0 {
			if err := tx.DeleteVote(ctx, actor.UserID, targetType, targetID); err != nil {
				return err
			}
		} else if err := tx.UpsertVote(ctx, actor.UserID, targetType, targetID, to, now); err != nil {
			return err
		}
		// 只改分数，不碰 last_reply_at。
		if err := tx.AdjustScore(ctx, threadID, delta); err != nil {
			return err
		}
		// v0.5.0 / ADR-057：karma 只认 upvote，被踩不扣作者。
		// 按 upvote 状态差计算，改票（up↔down）与撤票（up→0）都精确同步。
		karmaDelta := voteKarma(to) - voteKarma(current)
		if karmaDelta != 0 {
			owner, err := s.voteOwner(ctx, tx, targetType, targetID)
			if err != nil {
				return err
			}
			// 给自己投票不给 karma（与 threads.score 允许自投票不同，避免自刷声望）。
			if owner != actor.UserID {
				if err := tx.AdjustKarma(ctx, owner, karmaDelta*karmaPerUpvote); err != nil {
					return err
				}
			}
		}
		ev = event.DomainEvent{
			ID:         fmt.Sprintf("vote:%d:%s:%d", actor.UserID, targetType, targetID),
			Type:       event.EventVoteCast,
			OccurredAt: now,
			ActorID:    actor.UserID,
			ThreadID:   threadID,
		}
		if targetType == vote.TargetPost {
			ev.PostID = targetID
		}
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return VoteResult{}, err
	}
	// 幂等空操作没有事件，不能把零值事件送进出口。
	if ev.Type != event.EventUnknown {
		s.afterCommit(ctx, ev)
	}
	return res, nil
}

// voteKarma 把投票值映射为"是否赞"：只有 upvote 给作者 karma，downvote/撤票均为 0。
func voteKarma(v vote.Value) int64 {
	if v > 0 {
		return 1
	}
	return 0
}

// voteOwner 取投票目标的作者 ID，用于 karma 记到目标作者而非投票者。
func (s *Service) voteOwner(ctx context.Context, tx WriteTx, targetType vote.TargetType, targetID int64) (int64, error) {
	switch targetType {
	case vote.TargetThread:
		return tx.ThreadOwner(ctx, targetID)
	case vote.TargetPost:
		return tx.PostOwner(ctx, targetID)
	default:
		return 0, ErrInvalidVoteTarget
	}
}

// voteThreadID 解析投票目标对应的主题：thread 目标就是自己，post 目标取其所属主题。
func (s *Service) voteThreadID(ctx context.Context, tx WriteTx, targetType vote.TargetType, targetID int64) (int64, error) {
	switch targetType {
	case vote.TargetThread:
		exists, err := tx.ThreadExists(ctx, targetID)
		if err != nil {
			return 0, err
		}
		if !exists {
			return 0, fmt.Errorf("%w: 投票主题不存在", ErrNotFound)
		}
		return targetID, nil
	case vote.TargetPost:
		return tx.PostThreadID(ctx, targetID)
	default:
		return 0, ErrInvalidVoteTarget
	}
}
