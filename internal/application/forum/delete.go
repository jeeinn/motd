package forum

// 作者删除自己的主题 / 回复（v0.2.1 低风险补齐）。
//
// 三条不变量写在这里，命令层不得复制：
//  1. 所有权判定只有 canModify 一个入口；将来引入版主只在这里加分支。
//  2. 只改 status=deleted，不物理删除、不级联——回复行随主题一并不可见但留在库里。
//  3. 非作者 / 访客 / 目标不存在 / 目标已删，一律收敛为 ErrNotOwner（「仅作者可删除」），
//     不给外部任何区分「存在与否」「什么状态」的信号（延续 /keys 防指纹枚举口径）。
//
// 这里刻意不调用 Actor.checkWrite()：访客拿到 ErrReadOnly 会与非作者提示不同，
// 破坏第 3 条统一口径。删除是收缩可见性的低风险写，账号冻结的取舍见回传说明。

import (
	"context"
	"errors"
)

// DeleteTargetType 区分删除对象。取值由命令层硬编码，不来自用户文本。
type DeleteTargetType uint8

const (
	DeleteThread DeleteTargetType = iota + 1
	DeletePost
)

func (t DeleteTargetType) Valid() bool { return t == DeleteThread || t == DeletePost }

// DeleteResult 是删除动作的语义结果。删主题时 ThreadID==TargetID；
// 删回复时 ThreadID 未在此解析（命令层用当前打开的主题导航）。
type DeleteResult struct {
	TargetType DeleteTargetType
	TargetID   int64
	ThreadID   int64
}

// canModify 是全项目唯一的所有权判定：
// 只有登录用户（userID>0）且作者 ID 相同才能改。访客 UserID=0 天然被拒。
func canModify(userID, ownerID int64) bool {
	return userID > 0 && ownerID > 0 && userID == ownerID
}

// Delete 在单个事务里完成「取目标作者 → 判权 → 软删」。
// 取作者走 WriteTx 而不是 ThreadReader/PostReader，保证检查与写入同一快照，
// 并发下两次删除不会各自读到 status 后重复写入。
func (s *Service) Delete(ctx context.Context, actor Actor, targetType DeleteTargetType, targetID int64) (DeleteResult, error) {
	if !targetType.Valid() {
		return DeleteResult{}, ErrInvalidDeleteTarget
	}
	// 非法 ID 也收敛成统一提示，不区分「参数错」与「没权限」。
	if targetID <= 0 {
		return DeleteResult{}, ErrNotOwner
	}

	var res DeleteResult
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		switch targetType {
		case DeleteThread:
			owner, err := tx.ThreadOwner(ctx, targetID)
			if errors.Is(err, ErrNotFound) {
				return ErrNotOwner
			}
			if err != nil {
				return err // 真实存储故障必须透传，不能伪装成权限拒绝
			}
			if !canModify(actor.UserID, owner) {
				return ErrNotOwner
			}
			if err := tx.SoftDeleteThread(ctx, targetID); err != nil {
				return err
			}
			res = DeleteResult{TargetType: targetType, TargetID: targetID, ThreadID: targetID}
		case DeletePost:
			owner, err := tx.PostOwner(ctx, targetID)
			if errors.Is(err, ErrNotFound) {
				return ErrNotOwner
			}
			if err != nil {
				return err
			}
			if !canModify(actor.UserID, owner) {
				return ErrNotOwner
			}
			if err := tx.SoftDeletePost(ctx, targetID); err != nil {
				return err
			}
			res = DeleteResult{TargetType: targetType, TargetID: targetID}
		}
		return nil
	})
	if err != nil {
		return DeleteResult{}, err
	}
	return res, nil
}
