package forum

import (
	"context"
	"errors"
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
	case errors.Is(err, ErrNotOwner):
		n.Kind, n.Code = viewmodel.NoticeWarning, "not_owner"
		// 哨兵错误必须有稳定的对外文案：写事务会把错误包成
		// 「写入事务失败（不可重试）: ...」，直接 err.Error() 既暴露内部实现，
		// 又破坏「仅作者可删除」的统一口径。
		n.Message = ErrNotOwner.Error()
	case errors.Is(err, ErrInvalidSort), errors.Is(err, ErrInvalidCursor),
		errors.Is(err, ErrInvalidLimit), errors.Is(err, ErrInvalidTitle),
		errors.Is(err, ErrInvalidBody), errors.Is(err, ErrInvalidVote),
		errors.Is(err, ErrInvalidVoteTarget):
		n.Kind, n.Code = viewmodel.NoticeWarning, "invalid_input"
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
	rows := make([]viewmodel.Row, 0, len(nodes))
	for i, n := range nodes {
		rows = append(rows, nodeRow(n, v.RowOffset+i+1))
	}
	return screenOf(viewmodel.ScreenNodes, rows, v), nil
}

// ScreenThreads 输出主题列表屏；NextCursor 原样带上，交回入口层保管。
func (s *Service) ScreenThreads(ctx context.Context, q ListThreadsQuery, v ViewContext) (viewmodel.Screen, error) {
	page, err := s.ListThreads(ctx, q)
	if err != nil {
		return viewmodel.Screen{}, err
	}
	rows := make([]viewmodel.Row, 0, len(page.Rows))
	for i, t := range page.Rows {
		rows = append(rows, threadRow(t, v.RowOffset+i+1))
	}
	sc := screenOf(viewmodel.ScreenThreads, rows, v)
	sc.Pagination.HasMore = page.HasMore
	sc.Pagination.NextCursor = page.NextCursor
	return sc, nil
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
		rows = detailRows(d.Posts[1:], v.RowOffset)
	}
	sc := screenOf(viewmodel.ScreenThreadDetail, rows, v)
	sc.Detail = detail
	sc.Pagination.HasMore = d.HasMore
	return sc, nil
}

// ScreenSearchResults 输出检索结果屏。Row.ID 取 threadID，Enter 可直接打开主题；
// postID 放在 Meta.Extra 里作为语义元信息，不参与渲染，避免把用户文本带进固定文案。
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
