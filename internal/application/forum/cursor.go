package forum

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// 不透明游标：终端只透传字符串，不解释内容。
// 格式 v2|<sort>|<mark>|<last_reply_at>|<score>|<id>，URL 安全 base64 编码。
//
// R-15 / ADR-079 升 v2：新增 mark 段作为置顶层锚点。v1（五段，无 mark）
// 仍可解码——存量游标诞生时全库 mark=0，解释为普通层锚点语义天然正确。
// encode 恒输出 v2；新游标给旧进程不兼容（SSH 单进程停机部署，不做滚动升级）。
// 不带签名：游标不含权限信息，伪造最多导致查询错位，解码时严格校验拦下。
const (
	cursorVersion   = "v2"
	legacyCursorVer = "v1"
)

func encodeThreadCursor(c ThreadCursor) string {
	raw := strings.Join([]string{
		cursorVersion,
		strconv.FormatUint(uint64(c.Sort), 10),
		strconv.FormatInt(int64(c.Mark), 10),
		strconv.FormatInt(c.LastReplyAt, 10),
		strconv.FormatInt(c.Score, 10),
		strconv.FormatInt(c.ID, 10),
	}, "|")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeThreadCursor 把空串视为第一页；其余必须严格满足格式与取值域。
// v1（5 段）与 v2（6 段）都接受：v1 无 mark 段，解码为 Mark=0（普通层）。
func decodeThreadCursor(s string) (ThreadCursor, error) {
	if s == "" {
		return ThreadCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ThreadCursor{}, fmt.Errorf("%w: base64 解码失败", ErrInvalidCursor)
	}
	parts := strings.Split(string(raw), "|")
	var nums []int64
	var mark int64
	switch {
	case len(parts) == 6 && parts[0] == cursorVersion:
		// v2|<sort>|<mark>|<lra>|<score>|<id>
		mark, err = strconv.ParseInt(parts[2], 10, 64)
		if err != nil || mark < 0 || mark > 1 {
			return ThreadCursor{}, fmt.Errorf("%w: mark 必须为 0 或 1", ErrInvalidCursor)
		}
		nums = make([]int64, 4)
		for i, p := range []string{parts[1], parts[3], parts[4], parts[5]} {
			n, e := strconv.ParseInt(p, 10, 64)
			if e != nil || n < 0 {
				return ThreadCursor{}, fmt.Errorf("%w: 第 %d 个字段不是非负整数", ErrInvalidCursor, i+1)
			}
			nums[i] = n
		}
	case len(parts) == 5 && parts[0] == legacyCursorVer:
		// v1|<sort>|<lra>|<score>|<id>：存量游标，mark 恒为 0（普通层）。
		mark = 0
		nums = make([]int64, 4)
		for i, p := range parts[1:] {
			n, e := strconv.ParseInt(p, 10, 64)
			if e != nil || n < 0 {
				return ThreadCursor{}, fmt.Errorf("%w: 第 %d 个字段不是非负整数", ErrInvalidCursor, i+1)
			}
			nums[i] = n
		}
	default:
		return ThreadCursor{}, fmt.Errorf("%w: 版本或字段数不匹配", ErrInvalidCursor)
	}
	c := ThreadCursor{
		Sort:        ThreadSort(nums[0]),
		LastReplyAt: nums[1],
		Score:       nums[2],
		ID:          nums[3],
		Mark:        int8(mark),
	}
	if !c.Sort.Valid() || c.ID == 0 {
		return ThreadCursor{}, fmt.Errorf("%w: 排序值非法或缺少锚点 ID", ErrInvalidCursor)
	}
	return c, nil
}
