package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jeeinn/motd/internal/application/forum"
	sqlitedrv "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// 节点治理的仓储实现（P2-M1）。按资源独立成文件，不把 node 写入塞进
// 已经很长的 repo_forum.go；所有写方法仍绑定 WriteTx 的同一个 *sql.Tx。

// ListAllNodes 是管理员专用只读变体：不过滤 status，含 active/archived。
// 公开 ListNodes 的 status='active' 过滤保持不变（ADR-048 / archtest 守卫）。
func (r *ForumRepos) ListAllNodes(ctx context.Context) ([]forum.NodeAdminView, error) {
	rows, err := r.pools.QueryContext(ctx, `SELECT id, slug, name, status FROM nodes ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("列出全部版块: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []forum.NodeAdminView
	for rows.Next() {
		var v forum.NodeAdminView
		if err := rows.Scan(&v.ID, &v.Slug, &v.Name, &v.Status); err != nil {
			return nil, fmt.Errorf("扫描版块管理行: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历版块管理行: %w", err)
	}
	return out, nil
}

// NodeIDBySlug 在写事务快照内解析目标；与后续 UPDATE 同一事务，没有 TOCTOU 窗口。
func (w *sqlWriteTx) NodeIDBySlug(ctx context.Context, slug string) (int64, error) {
	var id int64
	err := w.tx.QueryRowContext(ctx, `SELECT id FROM nodes WHERE slug = ?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, forum.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("按 slug 查询版块 ID: %w", err)
	}
	return id, nil
}

// CreateNode 插入 active 节点。status 显式写入，不依赖后续迁移的默认值。
// slug 的唯一性由 0001 的 UNIQUE 索引保证；这里把冲突映射成领域哨兵，
// 让用例层能给用户「slug 已存在」而不是原始驱动错误。
func (w *sqlWriteTx) CreateNode(ctx context.Context, n forum.NewNode) (int64, error) {
	// nodes 表没有 created_at 列（0001），NewNode.CreatedAt 只用于领域事件时间戳。
	res, err := w.tx.ExecContext(ctx, `
		INSERT INTO nodes (slug, name, description, status)
		VALUES (?, ?, ?, 'active')`,
		n.Slug, n.Name, n.Description)
	if err != nil {
		if isNodeSlugConflict(err) {
			return 0, forum.ErrNodeSlugExists
		}
		return 0, fmt.Errorf("创建版块: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取版块 ID: %w", err)
	}
	return id, nil
}

// RenameNode 只更新 name；slug 是稳定标识，任何情况下都不在这里改写。
func (w *sqlWriteTx) RenameNode(ctx context.Context, id int64, newName string) error {
	res, err := w.tx.ExecContext(ctx, `UPDATE nodes SET name = ? WHERE id = ?`, newName, id)
	if err != nil {
		return fmt.Errorf("版块改名: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("检查版块改名行数: %w", err)
	}
	if affected == 0 {
		return forum.ErrNotFound
	}
	return nil
}

// ArchiveNode 只改 nodes.status；threads 一行不动（用户拍板的归档语义）。
// 已归档再归档是合法 no-op，不报错，便于重试与幂等调用。
func (w *sqlWriteTx) ArchiveNode(ctx context.Context, id int64) error {
	if _, err := w.tx.ExecContext(ctx, `UPDATE nodes SET status = 'archived' WHERE id = ?`, id); err != nil {
		return fmt.Errorf("归档版块: %w", err)
	}
	return nil
}

func isNodeSlugConflict(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlitedrv.Error
	if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return true
	}
	return strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED: NODES.SLUG")
}
