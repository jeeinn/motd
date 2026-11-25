package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/event"
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

// inPlaceholders 生成 n 个逗号分隔的 ? 占位符（供 IN (...) 使用）。
// n 必须 > 0：调用方负责把「空集合」判在前面（空 IN () 是语法错误）。
func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// latestThreadsQuery 是跨版块最新列表的完整 SQL，**由仓储与 EXPLAIN 测试共用**——
// 执行计划验的必须是生产实际跑的那条串，不能测试里另抄一份（抄一份就会漂移）。
// 参数顺序固定：IN 值 × nodeCount → 游标行值 (last_reply_at, id) → LIMIT。
//
// 两处刻意的写法（ADR-063 / QA 实测）：
//  1. **INDEXED BY 强制索引**：不加提示时优化器在同一句 SQL、两份不同数据上会给出
//     不同计划（idx_threads_node + temp b-tree 或 idx_threads_last_reply），
//     索引断言本质上不可稳定复现。代价是缺索引时 prepare 期直接报错（fail-fast），
//     优于静默退化成全表扫。
//  2. **行值游标 `(a, b) < (?, ?)`** 而不是 `a < ? OR (a = ? AND b < ?)`：
//     OR 形态即使有索引也只能 SCAN（从头扫到游标），翻 k 页总代价 O(k×N)；
//     行值形态能 seek（SEARCH），总量 O(N)。首屏无差别，深翻页差三个数量级。
func latestThreadsQuery(nodeCount int, withCursor bool) string {
	q := selectThreadsScoped + ` INDEXED BY idx_threads_last_reply` +
		` WHERE t.status = 'active' AND t.node_id IN (` + inPlaceholders(nodeCount) + `)`
	if withCursor {
		q += ` AND (t.last_reply_at, t.id) < (?, ?)`
	}
	return q + ` ORDER BY t.last_reply_at DESC, t.id DESC LIMIT ?`
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

// ListNodes 是公开版块列表：只返回 active。archived 节点从列表消失，但线程详情
// 仍可经 GetNodeBySlug 取到版块名——历史帖所属节点被归档后不能让详情页失去版块标识。
// 过滤条件写进 SQL（不在应用层查完再筛），archtest/node_invariants_test.go 静态守卫。
func (r *ForumRepos) ListNodes(ctx context.Context) ([]node.Node, error) {
	rows, err := r.pools.QueryContext(ctx, selectNodes+` WHERE status = 'active' ORDER BY id ASC`)
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

// selectThreadsScoped 是**跨版块**查询的列投影：JOIN nodes 后所有列必须带别名，
// 且必须逐列书写、不能写 t.*——threads 自 0004 起有 8 列（多了 status），
// 而 scanThread 只扫 7 列，t.* 会让 Scan 在运行期报「期望 7 个目标」。
const selectThreadsScoped = `SELECT t.id, t.node_id, t.user_id, t.title, t.score, t.last_reply_at, t.created_at FROM threads t`

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
	// 只返回 active 主题；deleted / hidden 对公开读取不可见（0004_threads_status）。
	t, err := scanThread(r.pools.QueryRowContext(ctx, selectThreads+` WHERE id = ? AND status = 'active'`, id))
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
//
// NodeID == 0 是「跨版块」语义（v0.7.1 / ADR-063），**只接受显式允许列表**：
// 版块内列表靠「归档版块从 ListNodes 消失、导航不可达」隐藏，跨版块列表没有这层导航兜底，
// 因此可见性由用例层算好的 AllowedNodeIDs 决定（= 公开 active 版块 ∪ 该用户已解锁的隐藏版块），
// 仓储只做 `node_id IN (...)`。这里刻意**不**写 `JOIN nodes ... status='active'`——
// 那会把已解锁的 /dev/null（status='archived'）一并挡掉，让「解锁了却看不到新帖」。
// AllowedNodeIDs 为空时 fail-closed 返回空列表，绝不退化成「查全表」。
// 版块内分支（NodeID > 0）的 SQL 与改前逐字一致，既有行为零变化。
func (r *ForumRepos) ListThreads(ctx context.Context, q forum.ThreadQuery) ([]thread.Thread, bool, error) {
	if !q.Sort.Valid() {
		return nil, false, forum.ErrInvalidSort
	}
	if !q.After.IsZero() && q.After.Sort != q.Sort {
		return nil, false, fmt.Errorf("%w: 游标排序与查询排序不一致", forum.ErrInvalidCursor)
	}
	if q.NodeID < 0 {
		// 负值既不是合法版块 ID，也不是跨版块哨兵；显式拒绝，避免与 0 的语义混淆。
		return nil, false, fmt.Errorf("%w: 版块 ID 不能为负", forum.ErrInvalidNodeID)
	}
	limit := normalizeLimit(q.Limit)

	var query string
	var args []any
	switch q.Sort {
	case forum.SortLatest:
		if q.NodeID == 0 {
			if len(q.AllowedNodeIDs) == 0 {
				return nil, false, nil
			}
			query = latestThreadsQuery(len(q.AllowedNodeIDs), !q.After.IsZero())
			for _, id := range q.AllowedNodeIDs {
				args = append(args, id)
			}
			if !q.After.IsZero() {
				// 行值游标 (last_reply_at, id) < (?, ?)：与 latestThreadsQuery 的参数顺序一致。
				args = append(args, q.After.LastReplyAt, q.After.ID)
			}
		} else {
			query = selectThreads + ` WHERE node_id = ? AND status = 'active'`
			args = append(args, q.NodeID)
			if !q.After.IsZero() {
				query += ` AND (last_reply_at < ? OR (last_reply_at = ? AND id < ?))`
				args = append(args, q.After.LastReplyAt, q.After.LastReplyAt, q.After.ID)
			}
			query += ` ORDER BY last_reply_at DESC, id DESC LIMIT ?`
		}
	case forum.SortScore:
		// 跨版块热度榜不在 v0.7.1 范围（P4.5-PLAN §5 明确不做）。显式拒绝，
		// 绝不能落到 `node_id = 0` 上静默返回空列表——那会让「排序不支持」
		// 伪装成「没有主题」。
		if q.NodeID == 0 {
			return nil, false, fmt.Errorf("%w: 跨版块列表仅支持 SortLatest", forum.ErrInvalidSort)
		}
		query = selectThreads + ` WHERE node_id = ? AND status = 'active'`
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

// edited_at 用 COALESCE 归一到 0（领域层 0 = 从未编辑），避免把 sql.NullInt64 泄漏出仓储。
const selectPosts = `SELECT id, thread_id, user_id, body_md, status, parent_post_id, created_at, COALESCE(edited_at, 0) FROM posts`

func scanPost(s interface{ Scan(...any) error }) (post.Post, error) {
	var p post.Post
	var parent sql.NullInt64
	var status string
	var created int64
	err := s.Scan(&p.ID, &p.ThreadID, &p.UserID, &p.BodyMD, &status, &parent, &created, &p.EditedAt)
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

// ListPostsTail 返回主题下最后 limit 条内容（id > afterID），结果按 id 升序。
//
// 用于「跳到末页」：回复成功后要定位到刚发的那条，而它必然在末尾。这里用
// ORDER BY id DESC LIMIT n 再反转，避免为了算页码去 count 一遍全部回复。
// 与 ListPosts 一样固定过滤软删除。
func (r *ForumRepos) ListPostsTail(ctx context.Context, threadID, afterID int64, limit int) ([]post.Post, error) {
	limit = normalizeLimit(limit)
	rows, err := r.pools.QueryContext(ctx,
		selectPosts+` WHERE thread_id = ? AND status = 'normal' AND id > ? ORDER BY id DESC LIMIT ?`,
		threadID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出主题末尾内容: %w", err)
	}
	defer rows.Close()

	var out []post.Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描内容: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历内容: %w", err)
	}
	// 查询按 id DESC，返回前反转成升序，与 ListPosts 的口径一致。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// CountPosts 统计 id > afterID 的可见内容条数（同样过滤软删除）。
// 末页视图用它算出序号偏移：末页第一条的全局序号 = 总数 - 本页条数 + 1。
func (r *ForumRepos) CountPosts(ctx context.Context, threadID, afterID int64) (int, error) {
	var n int
	err := r.pools.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM posts WHERE thread_id = ? AND status = 'normal' AND id > ?`,
		threadID, afterID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计主题内容条数: %w", err)
	}
	return n, nil
}

// PostAuthor 供编辑入口进入 composer 前只读判权；只认 normal 内容，
// 不存在 / 软删 / 隐藏一律 ErrNotFound，与 PostOwner 的口径一致。
func (r *ForumRepos) PostAuthor(ctx context.Context, postID int64) (int64, error) {
	var owner int64
	err := r.pools.QueryRowContext(ctx,
		`SELECT user_id FROM posts WHERE id = ? AND status = 'normal'`, postID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, forum.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("查询内容作者: %w", err)
	}
	return owner, nil
}

// ---------------------------------------------------------------------------
// UsernameReader
// ---------------------------------------------------------------------------

// UsernamesByIDs 一次查询投影一批作者名，供 Screen 组装使用（非 N+1）。
// 参数全部走 ? 占位符：ids 只作为值绑定，绝不拼进 SQL 文本。
// 不对 users.status 做过滤——历史帖作者注销后仍应显示其用户名。
func (r *ForumRepos) UsernamesByIDs(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	// 去重、跳过非正 ID：同一屏里多个帖子作者相同是常态，不必重复绑定参数。
	args := make([]any, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	query := `SELECT id, username FROM users WHERE id IN (` + placeholders + `)`
	rows, err := r.pools.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("批量查询用户名: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var username string
		if err := rows.Scan(&id, &username); err != nil {
			return nil, fmt.Errorf("扫描用户名: %w", err)
		}
		out[id] = username
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户名: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// ReplyCountReader (P2-M3 / ADR-052)
// ---------------------------------------------------------------------------

// replyCountsQuery 由仓储与 EXPLAIN 测试共用，确保执行计划验的就是生产实际跑的 SQL。
func replyCountsQuery(n int) string {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", n), ",")
	return `SELECT thread_id, COUNT(*) FROM posts INDEXED BY idx_posts_thread WHERE thread_id IN (` +
		placeholders + `) AND status = 'normal' GROUP BY thread_id`
}

// ReplyCountsByThreadIDs 一次批量统计一屏主题的回复数（非 N+1）。
// 口径与详情页一致：只认 status='normal'，内容数减 1（首帖作正文不算回复），下限 0；
// 软删 / 隐藏的回复自动排除。ids 去重、跳过非正 ID，参数全走 ? 占位符。
//
// INDEXED BY idx_posts_thread 是必需的：SQLite 是代价优化器，在 posts 只有几十行时
// 即便索引存在也会为 `thread_id IN (...)` 选 SCAN posts；显式锁定后小表 / 大表都稳定
// 走 SEARCH posts USING INDEX idx_posts_thread。代价是索引名硬绑定——索引若被改名或
// 删除，查询会直接报错而不是静默退化成全表扫，这是可接受的 fail-fast。
func (r *ForumRepos) ReplyCountsByThreadIDs(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}

	rows, err := r.pools.QueryContext(ctx, replyCountsQuery(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("批量查询回复数: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var threadID int64
		var count int
		if err := rows.Scan(&threadID, &count); err != nil {
			return nil, fmt.Errorf("扫描回复数: %w", err)
		}
		if count > 0 {
			count-- // 首帖是正文（详情页 Posts[0]）；没有可见内容时下限 0。
		}
		out[threadID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历回复数: %w", err)
	}
	return out, nil
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

// RecentWriteStats 统计窗口内该用户的**发帖动作次数**。
//
// CreateThread 会在同一事务里写入 threads + 首帖 posts，两者共用同一个 created_at；
// 若直接 COUNT(*) 会把一次发帖算成两次，配额被双倍消耗。因此按 created_at 去重：
// 60 秒冷却保证同一用户不可能在同一秒内产生两次独立发帖，去重等价于按动作计数。
// 索引见 0003_rate_limit_indexes.sql；过滤走覆盖索引，避免全表。
func (w *sqlWriteTx) RecentWriteStats(ctx context.Context, userID int64, since time.Time) (forum.WriteStats, error) {
	sinceSec := toUnix(since)
	var count, maxSec, minSec int64
	err := w.tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT created_at), COALESCE(MAX(created_at), 0), COALESCE(MIN(created_at), 0)
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

// validateParentThread 在写事务内先读父帖的 thread_id，再让 InsertPost 落库。
// 应用层和 0005 触发器是两道独立防线：这里给出可读的领域错误，触发器兜住
// 任何绕过 WriteTx 的裸 SQL 写入。
func (w *sqlWriteTx) validateParentThread(ctx context.Context, parentPostID *int64, threadID int64) error {
	if parentPostID == nil {
		return nil
	}
	var parentThreadID int64
	err := w.tx.QueryRowContext(ctx, `SELECT thread_id FROM posts WHERE id = ?`, *parentPostID).Scan(&parentThreadID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 父帖 %d 不存在", forum.ErrNotFound, *parentPostID)
	}
	if err != nil {
		return fmt.Errorf("查询父帖所属主题: %w", err)
	}
	if parentThreadID != threadID {
		return fmt.Errorf("%w: 父帖 %d 属于主题 %d，回复要写入主题 %d",
			forum.ErrParentThreadMismatch, *parentPostID, parentThreadID, threadID)
	}
	return nil
}

func (w *sqlWriteTx) InsertPost(ctx context.Context, p forum.NewPost) (int64, error) {
	if err := w.validateParentThread(ctx, p.ParentPostID, p.ThreadID); err != nil {
		return 0, err
	}
	res, err := w.tx.ExecContext(ctx, `
		INSERT INTO posts (thread_id, user_id, body_md, status, parent_post_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		p.ThreadID, p.UserID, p.BodyMD, string(p.Status), p.ParentPostID, toUnix(p.CreatedAt))
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

// AppendOutbox 把领域事件写进 outbox，绑定在调用方的同一个 *sql.Tx 上。
// 事件先校验再序列化，event_id 冲突（同幂等键重复投递）由 UNIQUE + DO NOTHING 吸收。
func (w *sqlWriteTx) AppendOutbox(ctx context.Context, e event.DomainEvent) error {
	if err := e.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("序列化领域事件: %w", err)
	}
	_, err = w.tx.ExecContext(ctx, `
		INSERT INTO domain_event_outbox (event_id, type, payload, status, attempts, created_at)
		VALUES (?, ?, ?, 'pending', 0, ?)
		ON CONFLICT(event_id) DO NOTHING`,
		e.ID, strconv.Itoa(int(e.Type)), string(payload), toUnix(e.OccurredAt))
	if err != nil {
		return fmt.Errorf("追加 outbox 事件: %w", err)
	}
	return nil
}
