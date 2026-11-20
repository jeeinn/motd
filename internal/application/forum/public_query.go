package forum

import "context"

// PublicQuery 是暴露给插件的只读查询实现。
//
// 它只依赖 application 自己声明的 ThreadReader 端口，返回语义字段，
// 不返回 *sql.Rows 或领域外的存储细节。plugin/registry.ReadOnlyQuery
// 通过结构化接口匹配使用它，插件无需 import application。
type PublicQuery struct {
	threads ThreadReader
	posts   PostReader // v0.5.1：PostOwnerID 用；可为 nil（仅测 ThreadTitle 时）
}

// posts 用可变参数保持兼容：老调用只传 threads 时，PostOwnerID 返回 ErrNotFound。
func NewPublicQuery(threads ThreadReader, posts ...PostReader) *PublicQuery {
	q := &PublicQuery{threads: threads}
	if len(posts) > 0 {
		q.posts = posts[0]
	}
	return q
}

// ThreadTitle 返回主题标题；主题不存在时透传 ErrNotFound。
func (q *PublicQuery) ThreadTitle(ctx context.Context, threadID int64) (string, error) {
	t, err := q.threads.GetThread(ctx, threadID)
	if err != nil {
		return "", err
	}
	return t.Title, nil
}

// ThreadOwnerID 返回主题作者 ID（公开语义字段，不含 PII）。
func (q *PublicQuery) ThreadOwnerID(ctx context.Context, threadID int64) (int64, error) {
	t, err := q.threads.GetThread(ctx, threadID)
	if err != nil {
		return 0, err
	}
	return t.UserID, nil
}

// PostOwnerID 返回回复作者 ID；无 posts 端口时返回 ErrNotFound。
func (q *PublicQuery) PostOwnerID(ctx context.Context, postID int64) (int64, error) {
	if q.posts == nil {
		return 0, ErrNotFound
	}
	return q.posts.PostAuthor(ctx, postID)
}
