package forum

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// 不透明游标：终端只透传字符串，不解释内容。
// 格式 v1|<sort>|<last_reply_at>|<score>|<id>，用 URL 安全的 base64 编码。
// 不带签名：游标不含权限信息，伪造最多导致查询错位，解码时会被严格校验拦下。
const cursorVersion = "v1"

func encodeThreadCursor(c ThreadCursor) string {
	raw := strings.Join([]string{
		cursorVersion,
		strconv.FormatUint(uint64(c.Sort), 10),
		strconv.FormatInt(c.LastReplyAt, 10),
		strconv.FormatInt(c.Score, 10),
		strconv.FormatInt(c.ID, 10),
	}, "|")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeThreadCursor 把空串视为第一页；其余必须严格满足格式与取值域。
func decodeThreadCursor(s string) (ThreadCursor, error) {
	if s == "" {
		return ThreadCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ThreadCursor{}, fmt.Errorf("%w: base64 解码失败", ErrInvalidCursor)
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 5 || parts[0] != cursorVersion {
		return ThreadCursor{}, fmt.Errorf("%w: 版本或字段数不匹配", ErrInvalidCursor)
	}
	nums := make([]int64, 4)
	for i, p := range parts[1:] {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return ThreadCursor{}, fmt.Errorf("%w: 第 %d 个字段不是非负整数", ErrInvalidCursor, i+1)
		}
		nums[i] = n
	}
	c := ThreadCursor{
		Sort:        ThreadSort(nums[0]),
		LastReplyAt: nums[1],
		Score:       nums[2],
		ID:          nums[3],
	}
	if !c.Sort.Valid() || c.ID == 0 {
		return ThreadCursor{}, fmt.Errorf("%w: 排序值非法或缺少锚点 ID", ErrInvalidCursor)
	}
	return c, nil
}
