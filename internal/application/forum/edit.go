package forum

// 作者编辑自己的主题首帖 / 回复（P2-M2 / ADR-051）。
//
// 三条不变量写在这里，命令层不得复制：
//  1. 所有权判定只有 canModify 一个入口（与删除共用）；编辑是**作者权**，
//     不复用 requireAdmin——管理员不能编辑他人内容，这是两条不同权限。
//  2. 单写事务内「取作者 → 判权 → 改正文与 edited_at」；只更新 body_md / edited_at，
//     不碰 status / created_at / last_reply_at，也不写审计事件
//     （作者改自己的内容是普通写，不是治理动作，见 ADR-050 的区分）。
//  3. 非作者 / 访客 / 目标不存在一律收敛为 ErrNotEditor（「仅作者可编辑」），
//     不区分存在与否、不回显 ID，杜绝通过「能否编辑」探测内容（延续删除的防枚举口径）。
//
// 这里刻意不调用 Actor.checkWrite()：访客拿到 ErrReadOnly 会与非作者提示不同，
// 破坏第 3 条统一口径（与 Delete 同一理由）。
//
// 接受「投票对象可能是改前内容」的语义代价：首版不做版本历史 / diff / 审核流。

import (
	"context"
	"errors"
)

// EditPostInput 是编辑正文的输入。PostID 是 posts 表的主键，首帖与回复统一由此定位。
type EditPostInput struct {
	PostID int64
	Body   string
}

// CanEditPost 是进入 composer **之前**的只读判权：调用方（命令层）据此决定
// 要不要让用户重新输入全文，避免非作者白输一遍再被拒。
//
// 它复用 canModify 单一谓词、与 EditPost 同一错误口径（ErrNotEditor），
// 且走只读端口（不占写队列）。真正的判权仍在 EditPost 的写事务里复核——
// 本方法只是体验前置，不是信任决策；两者之间作者归属若被并发改变，
// 提交时 EditPost 仍会拒绝。
func (s *Service) CanEditPost(ctx context.Context, actor Actor, postID int64) error {
	if postID <= 0 {
		return ErrNotEditor
	}
	owner, err := s.repos.PostAuthor(ctx, postID)
	if errors.Is(err, ErrNotFound) {
		return ErrNotEditor
	}
	if err != nil {
		return err
	}
	if !canModify(actor.UserID, owner) {
		return ErrNotEditor
	}
	return nil
}

// EditPost 在单个事务里完成「取作者 → 判权 → 覆盖正文并打编辑时间」。
// 取作者走 WriteTx.PostOwner（只认 status='normal'），保证检查与写入同一快照；
// editedAt 由用例时钟提供（Unix 秒），仓储不得自己读时钟（RenderSystem 的确定性边界同理）。
func (s *Service) EditPost(ctx context.Context, actor Actor, in EditPostInput) error {
	// 正文校验复用发帖约束（空白 / 8 KiB 上限），与 composer 的输入上限一致。
	// 失败在开事务之前返回，不产生任何写事务。
	if err := validateBody(in.Body); err != nil {
		return err
	}
	// 非法 ID 与「不存在」同口径，不给外部区分信号。
	if in.PostID <= 0 {
		return ErrNotEditor
	}

	editedAt := s.now().UTC().Unix()
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		owner, err := tx.PostOwner(ctx, in.PostID)
		if errors.Is(err, ErrNotFound) {
			return ErrNotEditor
		}
		if err != nil {
			return err // 真实存储故障必须透传，不能伪装成权限拒绝
		}
		if !canModify(actor.UserID, owner) {
			return ErrNotEditor
		}
		return tx.EditPost(ctx, in.PostID, in.Body, editedAt)
	})
	return err
}
