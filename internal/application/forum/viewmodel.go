package forum

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
)

// ViewContext 是调用方（transport）传入的会话上下文。
// application 不 import presence / auth：在线数与身份由入口层组装后传入，
// 这样同一份 Screen 构造逻辑既能服务 SSH，也能服务 P1 的浏览器入口。
type ViewContext struct {
	Online    int
	Active    int
	Guest     bool
	Username  string
	Karma     int64
	RowOffset int // 跨页累计的行数，保证翻页后序号连续
	Context   string
	Notice    viewmodel.Notice
	// UnlockedHiddenNodes 是入口层注入的「该用户已解锁的隐藏节点」（v0.6.0）。
	// forum 层不依赖解锁存储，也不做权限判断——它只把调用方明确放行的节点
	// 并入节点列表。隐藏节点默认不在 ListNodes（status='active' 过滤）里。
	UnlockedHiddenNodes []node.Node
}

func (v ViewContext) statusBar() viewmodel.StatusBar {
	return viewmodel.StatusBar{
		Online:   v.Online,
		Active:   v.Active,
		Identity: viewmodel.Identity{Guest: v.Guest, Username: v.Username},
		Karma:    v.Karma,
		Level:    user.Level(v.Karma), // Q5：0/10/50/200 → L1–L4
		Notice:   v.Notice,
		Context:  v.Context,
	}
}

// NoticeFromError 把用例层错误映射成状态栏语义提示。
// Renderer 只负责表现，错误分类与冷却时间在这里一次定死。
func NoticeFromError(err error) viewmodel.Notice {
	if err == nil {
		return viewmodel.Notice{Kind: viewmodel.NoticeNone}
	}
	n := viewmodel.Notice{
		Kind:    viewmodel.NoticeError,
		Code:    "internal",
		Message: err.Error(),
	}
	var rate *RateLimitError
	switch {
	case errors.As(err, &rate):
		n.Kind = viewmodel.NoticeWarning
		n.Code = "rate_limited"
		if sec := int(math.Ceil(rate.RetryAfter.Seconds())); sec > 0 {
			n.RetryAfterSec = sec
		}
	case errors.Is(err, ErrReadOnly):
		n.Kind, n.Code = viewmodel.NoticeInfo, "read_only"
	case errors.Is(err, ErrForbidden):
		n.Kind, n.Code = viewmodel.NoticeWarning, "forbidden"
	case errors.Is(err, ErrNotFound):
		n.Kind, n.Code = viewmodel.NoticeWarning, "not_found"
		// 固定文案：写事务会包一层「写入事务失败（不可重试）」，
		// 治理隐藏也复用本哨兵（目标不存在 / 已删都不区分），不能把内部链回显给终端。
		n.Message = ErrNotFound.Error()
	case errors.Is(err, ErrNotOwner):
		n.Kind, n.Code = viewmodel.NoticeWarning, "not_owner"
		// 哨兵错误必须有稳定的对外文案：写事务会把错误包成
		// 「写入事务失败（不可重试）: ...」，直接 err.Error() 既暴露内部实现，
		// 又破坏「仅作者可删除」的统一口径。
		n.Message = ErrNotOwner.Error()
	case errors.Is(err, ErrNotEditor):
		// 编辑是作者权（canModify），与删除同构但动作文案不同（ADR-051）。
		// 同样取哨兵固定文案，绝不回显 ID 或事务内部链。
		n.Kind, n.Code = viewmodel.NoticeWarning, "not_editor"
		n.Message = ErrNotEditor.Error()
	case errors.Is(err, ErrInvalidSort), errors.Is(err, ErrInvalidCursor),
		errors.Is(err, ErrInvalidLimit), errors.Is(err, ErrInvalidTitle),
		errors.Is(err, ErrInvalidBody), errors.Is(err, ErrInvalidVote),
		errors.Is(err, ErrInvalidVoteTarget), errors.Is(err, ErrInvalidSlug),
		errors.Is(err, ErrInvalidNodeName), errors.Is(err, ErrInvalidNodeID):
		n.Kind, n.Code = viewmodel.NoticeWarning, "invalid_input"
	case errors.Is(err, ErrAdminDenied), errors.Is(err, ErrNodeSlugExists),
		errors.Is(err, ErrSeedNodeRename):
		n.Kind, n.Code = viewmodel.NoticeWarning, "admin_denied"
		// 与 ErrNotOwner 同口径：写事务包装过错误，直接 err.Error() 会暴露内部实现。
		// 固定取哨兵文案，绝不回显用户提交的 slug / name / 指纹。
		switch {
		case errors.Is(err, ErrAdminDenied):
			n.Message = ErrAdminDenied.Error()
		case errors.Is(err, ErrNodeSlugExists):
			n.Message = ErrNodeSlugExists.Error()
		default:
			n.Message = ErrSeedNodeRename.Error()
		}
	}
	return n
}

// WithNotice 把提示挂到已有屏幕上，用于写操作失败时保留当前列表。
func WithNotice(s viewmodel.Screen, n viewmodel.Notice) viewmodel.Screen {
	s.StatusBar.Notice = n
	return s
}

// ScreenNodes 输出版块列表屏。
func (s *Service) ScreenNodes(ctx context.Context, v ViewContext) (viewmodel.Screen, error) {
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return viewmodel.Screen{}, err
	}
	// v0.6.0：把入口层放行的隐藏节点追加到列表末尾。去重按 ID，
	// 防止解锁记录脏数据导致同一节点出现两行（Row.Index 必须唯一）。
	seen := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		seen[n.ID] = true
	}
	for _, n := range v.UnlockedHiddenNodes {
		if !seen[n.ID] && n.ID > 0 {
			nodes = append(nodes, n)
			seen[n.ID] = true
		}
	}
	rows := make([]viewmodel.Row, 0, len(nodes))
	for i, n := range nodes {
		rows = append(rows, nodeRow(n, v.RowOffset+i+1))
	}
	return screenOf(viewmodel.ScreenNodes, rows, v), nil
}

// ScreenThreads 输出主题列表屏；NextCursor 原样带上，交回入口层保管。
//
// q.NodeID == 0 是跨版块列表（/latest，v0.7.1 / ADR-063）：这里做两件版块内屏不做的事——
//  1. 用一次 ListNodes（公开 active，数量级小）∪ ViewContext.UnlockedHiddenNodes 算出
//     可见版块白名单交给仓储；白名单是**放行**语义，不是「排除归档」语义，因此已解锁的
//     /dev/null（status='archived'）能正常出现在 /latest 里。
//  2. 建立 id→slug 映射，把版块 slug 投影进各行 RowMeta.Node（版块内屏不需要、保持为空）。
//
// 版块内屏（NodeID > 0）路径完全不变：不多查 ListNodes、不填 slug。
func (s *Service) ScreenThreads(ctx context.Context, q ListThreadsQuery, v ViewContext) (viewmodel.Screen, error) {
	var slugs map[int64]string
	if q.NodeID == 0 {
		visible, err := s.visibleNodes(ctx, v)
		if err != nil {
			return viewmodel.Screen{}, err
		}
		slugs = make(map[int64]string, len(visible))
		if len(q.AllowedNodeIDs) == 0 {
			q.AllowedNodeIDs = make([]int64, 0, len(visible))
			for _, n := range visible {
				q.AllowedNodeIDs = append(q.AllowedNodeIDs, n.ID)
				slugs[n.ID] = n.Slug
			}
		}
	}
	page, err := s.ListThreads(ctx, q)
	if err != nil {
		return viewmodel.Screen{}, err
	}
	rows := make([]viewmodel.Row, 0, len(page.Rows))
	for i, t := range page.Rows {
		rows = append(rows, threadRow(t, v.RowOffset+i+1))
	}
	if slugs != nil {
		for i, t := range page.Rows {
			rows[i].Meta.Node = slugs[t.NodeID]
		}
	}
	if err := s.fillRowAuthors(ctx, rows, nil); err != nil {
		return viewmodel.Screen{}, err
	}
	if err := s.fillRowReplyCounts(ctx, rows); err != nil {
		return viewmodel.Screen{}, err
	}
	sc := screenOf(viewmodel.ScreenThreads, rows, v)
	sc.Pagination.HasMore = page.HasMore
	sc.Pagination.NextCursor = page.NextCursor
	return sc, nil
}

// visibleNodes 返回本会话在跨版块列表里可见的版块：公开 active 版块在前，
// 调用方明确放行的已解锁隐藏版块按 ID 去重后追加（与 ScreenNodes 的去重口径一致）。
// 隐藏节点的放行权在入口层（ViewContext），forum 层只做合并，不查解锁存储。
func (s *Service) visibleNodes(ctx context.Context, v ViewContext) ([]node.Node, error) {
	nodes, err := s.repos.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		seen[n.ID] = true
	}
	for _, n := range v.UnlockedHiddenNodes {
		if !seen[n.ID] && n.ID > 0 {
			nodes = append(nodes, n)
			seen[n.ID] = true
		}
	}
	return nodes, nil
}

// ScreenThreadDetail 输出主题详情屏：主题正文放 Detail，回复列表放 Rows。
func (s *Service) ScreenThreadDetail(ctx context.Context, threadID int64, v ViewContext) (viewmodel.Screen, error) {
	d, err := s.GetThread(ctx, threadID)
	if err != nil {
		return viewmodel.Screen{}, err
	}
	detail := viewmodel.Detail{
		ID:        d.Thread.ID,
		Title:     d.Thread.Title,
		AuthorID:  d.Thread.UserID,
		CreatedAt: d.Thread.CreatedAt.Unix(),
	}
	rows := detailRows(d.Posts, v.RowOffset)
	// 首帖是内容列表的第一条（§5.2 Post 含首帖），详情屏把它作为主题正文展示，
	// 其余条目作为回复行；两者不重复计数。
	if len(d.Posts) > 0 {
		first := d.Posts[0]
		detail.Body = first.BodyMD
		detail.AuthorID = first.UserID
		detail.CreatedAt = first.CreatedAt.Unix()
		// 正文存在首帖，编辑标记也读首帖；0（DB NULL）= 从未编辑，详情页不显示标记。
		detail.EditedAt = first.EditedAt
		rows = detailRows(d.Posts[1:], v.RowOffset)
	}
	if err := s.fillRowAuthors(ctx, rows, &detail); err != nil {
		return viewmodel.Screen{}, err
	}
	sc := screenOf(viewmodel.ScreenThreadDetail, rows, v)
	sc.Detail = detail
	sc.Pagination.HasMore = d.HasMore
	return sc, nil
}

// ScreenThreadDetailPage 输出详情屏的**一页**：主题正文（整段，行窗口由呈现层切）
// + 从 afterID 之后取 limit 条回复（R-06）。
//
// 与 ScreenThreadDetail 的区别只在取数方式：后者一次取满 MaxPostsPerThread，
// 供既有调用方与测试沿用；本函数按 keyset 分页，供翻页使用。
func (s *Service) ScreenThreadDetailPage(ctx context.Context, threadID, afterID int64, limit int, v ViewContext) (viewmodel.Screen, error) {
	head, err := s.ThreadHead(ctx, threadID)
	if err != nil {
		return viewmodel.Screen{}, err
	}
	page, err := s.ListReplies(ctx, threadID, afterID, limit)
	if err != nil {
		return viewmodel.Screen{}, err
	}
	detail := viewmodel.Detail{
		ID:        head.Thread.ID,
		Title:     head.Thread.Title,
		Body:      head.Post.BodyMD,
		AuthorID:  head.Post.UserID,
		CreatedAt: head.Post.CreatedAt.Unix(),
		EditedAt:  head.Post.EditedAt,
	}
	rows := detailRows(page.Replies, v.RowOffset)
	if err := s.fillRowAuthors(ctx, rows, &detail); err != nil {
		return viewmodel.Screen{}, err
	}
	sc := screenOf(viewmodel.ScreenThreadDetail, rows, v)
	sc.Detail = detail
	sc.Pagination.HasMore = page.HasMore
	if page.NextCursor > 0 {
		sc.Pagination.NextCursor = strconv.FormatInt(page.NextCursor, 10)
	}
	return sc, nil
}

// ScreenSearchResults 输出检索结果屏。Row.ID 取 threadID，Enter 可直接打开主题；// postID 放在 Meta.Extra 里作为语义元信息，不参与渲染，避免把用户文本带进固定文案。
func (s *Service) ScreenSearchResults(ctx context.Context, in SearchInput, v ViewContext) (viewmodel.Screen, error) {
	res, err := s.Search(ctx, in)
	if err != nil {
		return viewmodel.Screen{}, err
	}
	rows := make([]viewmodel.Row, 0, len(res.Hits))
	for i, h := range res.Hits {
		rows = append(rows, viewmodel.Row{
			Index: v.RowOffset + i + 1,
			ID:    h.ThreadID,
			Body:  h.Excerpt,
			Meta:  viewmodel.RowMeta{Extra: strconv.FormatInt(h.PostID, 10)},
		})
	}
	sc := screenOf(viewmodel.ScreenSearchResults, rows, v)
	sc.Pagination.HasMore = res.HasMore
	return sc, nil
}

// fillRowAuthors 一次批量查询把 username 投影到本屏所有作者字段。
// 这是 ADR-014「展示作者名时补仓储投影」的落点：Thread/Post 领域对象不含 username，
// 用户文本只以语义字段进入 ViewModel，净化仍由 Renderer 的 RenderUser 负责。
// 查询失败直接上抛，不静默丢弃——「查不到某个 id」由渲染层的 #ID 回退处理。
func (s *Service) fillRowAuthors(ctx context.Context, rows []viewmodel.Row, detail *viewmodel.Detail) error {
	ids := make([]int64, 0, len(rows)+1)
	if detail != nil && detail.AuthorID > 0 {
		ids = append(ids, detail.AuthorID)
	}
	for _, r := range rows {
		if r.Meta.AuthorID > 0 {
			ids = append(ids, r.Meta.AuthorID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	names, err := s.repos.UsernamesByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("投影作者用户名: %w", err)
	}
	for i := range rows {
		if rows[i].Meta.AuthorID > 0 {
			rows[i].Meta.Author = names[rows[i].Meta.AuthorID]
		}
	}
	if detail != nil && detail.AuthorID > 0 {
		detail.Author = names[detail.AuthorID]
	}
	return nil
}

// fillRowReplyCounts 一次批量查询把回复数投影到主题列表行的 RowMeta.Replies。
// 与 fillRowAuthors 同构（ADR-014 / ADR-052）：计数是展示投影，不进领域模型；
// 查询失败上抛，绝不静默显示 r0——「查失败」和「确实 0 回复」必须是两件事。
// 只在 ScreenThreads 调用：详情页 Rows 是回复列表、搜索屏不显示回复数。
func (s *Service) fillRowReplyCounts(ctx context.Context, rows []viewmodel.Row) error {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r.ID > 0 {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	counts, err := s.repos.ReplyCountsByThreadIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("投影回复数: %w", err)
	}
	for i := range rows {
		if rows[i].ID > 0 {
			rows[i].Meta.Replies = counts[rows[i].ID]
		}
	}
	return nil
}

func screenOf(kind viewmodel.ScreenKind, rows []viewmodel.Row, v ViewContext) viewmodel.Screen {
	focus := 0
	if len(rows) > 0 {
		focus = rows[0].Index
	}
	return viewmodel.Screen{
		Kind:      kind,
		Rows:      rows,
		StatusBar: v.statusBar(),
		Selection: viewmodel.Selection{FocusedIndex: focus, TopRowIndex: focus},
		Pagination: viewmodel.Pagination{
			FirstIndex: v.RowOffset + 1,
			Loaded:     len(rows),
		},
	}
}

func nodeRow(n node.Node, index int) viewmodel.Row {
	return viewmodel.Row{
		Index: index,
		ID:    n.ID,
		Title: n.Name,
		Meta: viewmodel.RowMeta{
			Node:  n.Slug,
			Extra: n.Description,
		},
	}
}

func threadRow(t thread.Thread, index int) viewmodel.Row {
	return viewmodel.Row{
		Index: index,
		ID:    t.ID,
		Score: t.Score,
		Title: t.Title,
		Meta: viewmodel.RowMeta{
			AuthorID:    t.UserID,
			CreatedAt:   t.CreatedAt.Unix(),
			LastReplyAt: t.LastReplyAt.Unix(),
		},
	}
}

func detailRows(posts []post.Post, offset int) []viewmodel.Row {
	rows := make([]viewmodel.Row, 0, len(posts))
	for i, p := range posts {
		rows = append(rows, viewmodel.Row{
			Index: offset + i + 1,
			ID:    p.ID,
			Body:  p.BodyMD,
			Meta: viewmodel.RowMeta{
				AuthorID:  p.UserID,
				CreatedAt: p.CreatedAt.Unix(),
			},
		})
	}
	return rows
}
