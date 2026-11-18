package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
)

// 编译期确认：端口由 application 定义，实现在本包（infrastructure → application 单向合法）。
var (
	_ forum.Repos   = (*ForumRepos)(nil)
	_ forum.WriteTx = (*sqlWriteTx)(nil)
)

// ForumRepos 把 P0-08 的仓储端口落到 SQLite。
// 读走 Pools 的只读门面，写走 Writer.Submit 的单事务；本包之外拿不到裸 *sql.DB。
type ForumRepos struct {
	pools  *Pools
	writer *Writer
}

// NewForumRepos 装配仓储。writer 必须已 Start（由 cmd/server 负责生命周期）。
func NewForumRepos(pools *Pools, writer *Writer) *ForumRepos {
	return &ForumRepos{pools: pools, writer: writer}
}

func fromUnix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }
func toUnix(t time.Time) int64     { return t.Unix() }

// normalizeLimit 防御非法页大小；用例层也会裁剪，这里再兜一层避免 LIMIT 0 / 负数。
func normalizeLimit(n int) int {
	if n <= 0 {
		return 20
	}
	if n > 100 {
		return 100
	}
	return n
}

// ---------------------------------------------------------------------------
// NodeReader
// ---------------------------------------------------------------------------

const selectNodes = `SELECT id, slug, name, description FROM nodes`

func scanNode(s interface{ Scan(...any) error }) (node.Node, error) {
	var n node.Node
	err := s.Scan(&n.ID, &n.Slug, &n.Name, &n.Description)
	return n, err
}

func (r *ForumRepos) ListNodes(ctx context.Context) ([]node.Node, error) {
	rows, err := r.pools.QueryContext(ctx, selectNodes+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("列出版块: %w", err)
	}
	defer rows.Close()

	var out []node.Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描版块: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历版块: %w", err)
	}
	return out, nil
}

func (r *ForumRepos) GetNodeBySlug(ctx context.Context, slug string) (node.Node, error) {
	n, err := scanNode(r.pools.QueryRowContext(ctx, selectNodes+` WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return node.Node{}, forum.ErrNotFound
	}
	if err != nil {
		return node.Node{}, fmt.Errorf("按 slug 查询版块: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// ThreadReader
// ---------------------------------------------------------------------------

const selectThreads = `SELECT id, node_id, user_id, title, score, last_reply_at, created_at FROM threads`

func scanThread(s interface{ Scan(...any) error }) (thread.Thread, error) {
	var t thread.Thread
	var lastReply, created int64
	err := s.Scan(&t.ID, &t.NodeID, &t.UserID, &t.Title, &t.Score, &lastReply, &created)
	if err != nil {
		return thread.Thread{}, err
	}
	t.LastReplyAt = fromUnix(lastReply)
	t.CreatedAt = fromUnix(created)
	return t, nil
}

func (r *ForumRepos) GetThread(ctx context.Context, id int64) (thread.Thread, error) {
	t, err := scanThread(r.pools.QueryRowContext(ctx, selectThreads+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return thread.Thread{}, forum.ErrNotFound
	}
	if err != nil {
		return thread.Thread{}, fmt.Errorf("查询主题: %w", err)
	}
	return t, nil
}

// ListThreads 用 keyset 分页：排序列 + id 组成复合键，避免 OFFSET 的深翻页代价与数据漂移。
// 多取一条判断 hasMore，返回给用例层的行数恒 <= q.Limit。
func (r *ForumRepos) ListThreads(ctx context.Context, q forum.ThreadQuery) ([]thread.Thread, bool, error) {
	if !q.Sort.Valid() {
		return nil, false, forum.ErrInvalidSort
	}
	if !q.After.IsZero() && q.After.Sort != q.Sort {
		return nil, false, fmt.Errorf("%w: 游标排序与查询排序不一致", forum.ErrInvalidCursor)
	}
	limit := normalizeLimit(q.Limit)

	var query string
	var args []any
	switch q.Sort {
	case forum.SortLatest:
		query = selectThreads + ` WHERE node_id = ?`
		args = append(args, q.NodeID)
		if !q.After.IsZero() {
			query += ` AND (last_reply_at < ? OR (last_reply_at = ? AND id < ?))`
			args = append(args, q.After.LastReplyAt, q.After.LastReplyAt, q.After.ID)
		}
		query += ` ORDER BY last_reply_at DESC, id DESC LIMIT ?`
	case forum.SortScore:
		query = selectThreads + ` WHERE node_id = ?`
		args = append(args, q.NodeID)
		if !q.After.IsZero() {
			query += ` AND (score < ? OR (score = ? AND id < ?))`
			args = append(args, q.After.Score, q.After.Score, q.After.ID)
		}
		query += ` ORDER BY score DESC, id DESC LIMIT ?`
	}
	args = append(args, limit+1)

	rows, err := r.pools.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("列出主题: %w", err)
	}
	defer rows.Close()

	var out []thread.Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, false, fmt.Errorf("扫描主题: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("遍历主题: %w", err)
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// ---------------------------------------------------------------------------
// PostReader
// ---------------------------------------------------------------------------

const selectPosts = `SELECT id, thread_id, user_id, body_md, status, parent_post_id, created_at FROM posts`

func scanPost(s interface{ Scan(...any) error }) (post.Post, error) {
	var p post.Post
	var parent sql.NullInt64
	var status string
	var created int64
	err := s.Scan(&p.ID, &p.ThreadID, &p.UserID, &p.BodyMD, &status, &parent, &created)
	if err != nil {
		return post.Post{}, err
	}
	p.Status = post.Status(status)
	p.CreatedAt = fromUnix(created)
	if parent.Valid {
		id := parent.Int64
		p.ParentID = &id
	}
	return p, nil
}

// ListPosts 固定过滤 status='normal'：软删除是存储层职责，用例层不该重复判断。
func (r *ForumRepos) ListPosts(ctx context.Context, q forum.PostQuery) ([]post.Post, bool, error) {
	limit := normalizeLimit(q.Limit)
	rows, err := r.pools.QueryContext(ctx,
		selectPosts+` WHERE thread_id = ? AND status = 'normal' AND id > ? ORDER BY id ASC LIMIT ?`,
		q.ThreadID, q.AfterID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("列出主题内容: %w", err)
	}
	defer rows.Close()

	var out []post.Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, false, fmt.Errorf("扫描内容: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("遍历内容: %w", err)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// ---------------------------------------------------------------------------
// UnitOfWork / WriteTx
// ---------------------------------------------------------------------------

// Write 把用例闭包包进 Writer.Submit 的单事务。
// 用例层只看到 forum.WriteTx，拿不到 *sql.Tx；事务提交成功后才返回 nil（ADR-010）。
func (r *ForumRepos) Write(ctx context.Context, fn func(forum.WriteTx) error) error {
	if fn == nil {
		return errors.New("写入闭包为空")
	}
	return r.writer.Submit(ctx, func(_ context.Context, tx *sql.Tx) error {
		return fn(&sqlWriteTx{tx: tx})
	})
}

// sqlWriteTx 是事务内的受限句柄，所有语句都绑定同一个 *sql.Tx。
type sqlWriteTx struct {
	tx *sql.Tx
}

func (w *sqlWriteTx) UserStatus(ctx context.Context, userID int64) (user.Status, error) {
	var s string
	err := w.tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, userID).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", forum.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("查询用户状态: %w", err)
	}
	return user.Status(s), nil
}

func (w *sqlWriteTx) NodeExists(ctx context.Context, nodeID int64) (bool, error) {
	var one int
	err := w.tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ?`, nodeID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查版块: %w", err)
	}
	return true, nil
}

func (w *sqlWriteTx) ThreadExists(ctx context.Context, threadID int64) (bool, error) {
	var one int
	err := w.tx.QueryRowContext(ctx, `SELECT 1 FROM threads WHERE id = ?`, threadID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查主题: %w", err)
	}
	return true, nil
}

// RecentWriteStats 统计窗口内该用户创建的主题 + 内容。
// 索引见 0003_rate_limit_indexes.sql；COUNT/MAX/MIN 走覆盖扫描，避免全表。
func (w *sqlWriteTx) RecentWriteStats(ctx context.Context, userID int64, since time.Time) (forum.WriteStats, error) {
	sinceSec := toUnix(since)
	var count, maxSec, minSec int64
	err := w.tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(created_at), 0), COALESCE(MIN(created_at), 0)
		FROM (
			SELECT created_at FROM threads WHERE user_id = ? AND created_at >= ?
			UNION ALL
			SELECT created_at FROM posts   WHERE user_id = ? AND created_at >= ?
		)`, userID, sinceSec, userID, sinceSec).Scan(&count, &maxSec, &minSec)
	if err != nil {
		return forum.WriteStats{}, fmt.Errorf("统计近期写入: %w", err)
	}
	if count == 0 {
		return forum.WriteStats{}, nil
	}
	return forum.WriteStats{
		Count:       int(count),
		LastWriteAt: fromUnix(maxSec),
		OldestAt:    fromUnix(minSec),
	}, nil
}

func (w *sqlWriteTx) InsertThread(ctx context.Context, n forum.NewThread) (int64, error) {
	sec := toUnix(n.CreatedAt)
	res, err := w.tx.ExecContext(ctx, `
		INSERT INTO threads (node_id, user_id, title, score, last_reply_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		n.NodeID, n.UserID, n.Title, n.Score, sec, sec)
	if err != nil {
		return 0, fmt.Errorf("插入主题: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取主题 ID: %w", err)
	}
	return id, nil
}

func (w *sqlWriteTx) InsertPost(ctx context.Context, p forum.NewPost) (int64, error) {
	res, err := w.tx.ExecContext(ctx, `
		INSERT INTO posts (thread_id, user_id, body_md, status, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		p.ThreadID, p.UserID, p.BodyMD, string(p.Status), toUnix(p.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("插入内容: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取内容 ID: %w", err)
	}
	return id, nil
}

// TouchThread 在一条 UPDATE 里同时维护 last_reply_at 与 score，
// 保证「回复落地」和「主题活跃度」不会出现半提交状态。
func (w *sqlWriteTx) TouchThread(ctx context.Context, threadID int64, at time.Time, scoreDelta int64) error {
	res, err := w.tx.ExecContext(ctx, `
		UPDATE threads SET last_reply_at = ?, score = score + ? WHERE id = ?`,
		toUnix(at), scoreDelta, threadID)
	if err != nil {
		return fmt.Errorf("更新主题活跃度: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("检查主题更新行数: %w", err)
	}
	if affected == 0 {
		return forum.ErrNotFound
	}
	return nil
}
