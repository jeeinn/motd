package forum

// 正文检索用例（P1-M3）。这里只做策略与边界，SQL 细节留在 infrastructure。
//
// 三条安全纪律：
//  1. 查询词先清洗控制字符，再交给仓储；绝不把用户原文拼进 SQL；
//  2. FTS5 的 MATCH 串整体用双引号包裹、内部引号翻倍（转义），
//     所以 * / - / " 等 FTS 语法字符只是普通文本；
//  3. LIKE 回退路径转义 \ / % / _，避免通配符把「搜 3 个字符」变成全表扫描。

import (
	"context"
	"math"
	"strings"
	"unicode/utf8"
)

const (
	// SearchDefaultPageSize / SearchMaxPageSize 锁死 20 条：
	// 单条摘要 ≤ SearchMaxExcerptBytes，一屏响应稳定在几 KiB，适配 1 Mbps 链路。
	SearchDefaultPageSize = 20
	SearchMaxPageSize     = 20
	SearchMaxExcerptBytes = 240
	// FTSMinRunes 是 trigram 能命中的最短查询长度（按 Unicode 码点计）。
	FTSMinRunes = 3
)

// SearchInput 是终端/浏览器传入的检索参数。Page 从 1 开始。
type SearchInput struct {
	Query string
	Page  int
	Limit int
}

// SearchResult 是一次检索的语义结果。
type SearchResult struct {
	Hits    []SearchHit
	Page    int
	HasMore bool
}

// NormalizeSearchQuery 去掉首尾空白并把控制字符替换成空格。
// 控制字符若进入终端输出会绕过安全渲染，因此在应用边界就消掉。
func NormalizeSearchQuery(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// FTSMatchTerm 把查询词转成安全的 MATCH 字面量：整体加双引号，内部引号翻倍。
// 返回值只能作为**绑定参数**传给 MATCH，不得字符串拼进 SQL 文本。
func FTSMatchTerm(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// EscapeLikePattern 转义 LIKE 的通配符与转义符本身，配合 SQL 的 ESCAPE '\' 使用。
func EscapeLikePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// TruncateExcerpt 按字节上限截断摘要并保证不切断 UTF-8 码点。
func TruncateExcerpt(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	budget := maxBytes - len(ellipsis)
	if budget <= 0 {
		return ""
	}
	end := budget
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + ellipsis
}

// Search 执行一次正文检索。分页与页长在这里定死，仓储只是执行者。
func (s *Service) Search(ctx context.Context, in SearchInput) (*SearchResult, error) {
	q := NormalizeSearchQuery(in.Query)
	if q == "" {
		return nil, ErrInvalidQuery
	}
	limit := in.Limit
	if limit <= 0 {
		limit = SearchDefaultPageSize
	}
	if limit > SearchMaxPageSize {
		limit = SearchMaxPageSize
	}
	page := in.Page
	if page <= 0 {
		page = 1
	}
	offset64 := int64(page-1) * int64(limit)
	if offset64 > math.MaxInt32 {
		return nil, ErrInvalidQuery
	}

	hits, hasMore, err := s.repos.SearchPosts(ctx, SearchQuery{
		Query: q, Limit: limit, Offset: int(offset64),
	})
	if err != nil {
		return nil, err
	}
	// 摘要上限在用例层兜底：即使某个仓储实现忘了裁剪，响应体也不会失控。
	for i := range hits {
		hits[i].Excerpt = TruncateExcerpt(hits[i].Excerpt, SearchMaxExcerptBytes)
	}
	return &SearchResult{Hits: hits, Page: page, HasMore: hasMore}, nil
}
