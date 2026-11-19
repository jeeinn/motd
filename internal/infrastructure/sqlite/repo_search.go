package sqlite

// 正文检索的 SQLite 实现。FTS5 外部内容表 posts_fts 由 0007 迁移建立，
// 读走 Pools 的只读门面，不占用唯一写连接。

import (
	"context"
	"database/sql"
	"fmt"
	"unicode/utf8"

	"github.com/jeeinn/motd/internal/application/forum"
)

var _ forum.SearchReader = (*ForumRepos)(nil)

func normalizeSearchLimit(n int) int {
	if n <= 0 {
		return forum.SearchDefaultPageSize
	}
	if n > forum.SearchMaxPageSize {
		return forum.SearchMaxPageSize
	}
	return n
}

// SearchPosts 根据查询长度选择索引或回退路径：
//   - ≥3 码点走 FTS5 trigram（MATCH 参数化、查询词按字面量转义）；
//   - <3 码点 trigram 实测返回 0，改走 LIKE 并转义通配符。
//
// 两条 SQL 都 JOIN posts 过滤 status='normal'，并多取一条判断 hasMore。
// v0.2.1 追加 JOIN threads 过滤 status='active'：作者软删主题时回复行按产品决策保留，
// 若不连带过滤所属主题，已删主题的内容仍会被 /search 命中并泄漏。
func (r *ForumRepos) SearchPosts(ctx context.Context, q forum.SearchQuery) ([]forum.SearchHit, bool, error) {
	limit := normalizeSearchLimit(q.Limit)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}

	var (
		rows *sql.Rows
		err  error
	)
	if utf8.RuneCountInString(q.Query) >= forum.FTSMinRunes {
		// MATCH 的值整体来自 FTSMatchTerm（引号包裹 + 内部引号翻倍），且只作为绑定参数。
		rows, err = r.pools.QueryContext(ctx, `
			SELECT p.id, p.thread_id, snippet(posts_fts, 0, '', '', '…', 8)
			FROM posts_fts
			JOIN posts p ON p.id = posts_fts.rowid AND p.status = 'normal'
			JOIN threads t ON t.id = p.thread_id AND t.status = 'active'
			WHERE posts_fts MATCH ?
			ORDER BY rank
			LIMIT ? OFFSET ?`,
			forum.FTSMatchTerm(q.Query), limit+1, offset)
	} else {
		// 短查询回退 LIKE：pattern 已转义 % / _ / \，不会退化成通配扫描。
		rows, err = r.pools.QueryContext(ctx, `
			SELECT p.id, p.thread_id, p.body_md
			FROM posts p
			JOIN threads t ON t.id = p.thread_id AND t.status = 'active'
			WHERE p.status = 'normal' AND p.body_md LIKE ? ESCAPE '\'
			ORDER BY p.id DESC
			LIMIT ? OFFSET ?`,
			"%"+forum.EscapeLikePattern(q.Query)+"%", limit+1, offset)
	}
	if err != nil {
		return nil, false, fmt.Errorf("检索正文: %w", err)
	}
	defer rows.Close()

	hits := make([]forum.SearchHit, 0, limit)
	for rows.Next() {
		var h forum.SearchHit
		if err := rows.Scan(&h.PostID, &h.ThreadID, &h.Excerpt); err != nil {
			return nil, false, fmt.Errorf("扫描检索结果: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("遍历检索结果: %w", err)
	}
	hasMore := len(hits) > limit
	if hasMore {
		hits = hits[:limit]
	}
	return hits, hasMore, nil
}

// RebuildFTS 从 posts 重建全文索引（运维恢复后用 motdctl fts-rebuild 调用）。
//
// 不用 FTS5 的 'rebuild' 命令：它会连 status='deleted'/'hidden' 的内容一起索引，
// 与本项目的可见性口径不一致。这里清空倒排表与标记表后，只回填 normal 行；
// 两步在同一事务内完成，中途失败不留半截索引。返回实际索引的行数。
func RebuildFTS(ctx context.Context, p *Pools) (int64, error) {
	tx, err := p.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("重建索引开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 不能用 DELETE FROM posts_fts：外部内容表对「索引里没有的内容行」会在
	// 删除时算出负向倒排项，实测直接报 disk image is malformed。
	// FTS5 的 delete-all 特殊命令直接清空倒排，不读外部内容表，安全且完整。
	if _, err := tx.ExecContext(ctx, `INSERT INTO posts_fts(posts_fts) VALUES('delete-all')`); err != nil {
		return 0, fmt.Errorf("清空全文索引: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM posts_fts_indexed`); err != nil {
		return 0, fmt.Errorf("清空索引标记: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO posts_fts(rowid, body_md)
		SELECT id, body_md FROM posts WHERE status = 'normal'`); err != nil {
		return 0, fmt.Errorf("回填全文索引: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO posts_fts_indexed(post_id)
		SELECT id FROM posts WHERE status = 'normal'`)
	if err != nil {
		return 0, fmt.Errorf("回填索引标记: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("统计索引行数: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("重建索引提交: %w", err)
	}
	return n, nil
}
