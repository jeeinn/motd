package forum

import "context"

// PublicQuery 是暴露给插件的只读查询实现。
//
// 它只依赖 application 自己声明的 ThreadReader 端口，返回语义字段，
// 不返回 *sql.Rows 或领域外的存储细节。plugin/registry.ReadOnlyQuery
// 通过结构化接口匹配使用它，插件无需 import application。
type PublicQuery struct {
	threads ThreadReader
}

func NewPublicQuery(threads ThreadReader) *PublicQuery {
	return &PublicQuery{threads: threads}
}

// ThreadTitle 返回主题标题；主题不存在时透传 ErrNotFound。
func (q *PublicQuery) ThreadTitle(ctx context.Context, threadID int64) (string, error) {
	t, err := q.threads.GetThread(ctx, threadID)
	if err != nil {
		return "", err
	}
	return t.Title, nil
}
