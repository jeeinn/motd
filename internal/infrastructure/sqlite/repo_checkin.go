package sqlite

// 签到与用户级解锁的持久化（v0.6.0 / P3-M6 扩展）。
// 实现 application/user.CheckInStore 与 cmd 层的解锁端口，端口定义在使用方。

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/domain/node"
)

var _ appuser.CheckInStore = (*CheckInStore)(nil)

type CheckInStore struct {
	pools  *Pools
	writer *Writer
}

func NewCheckInStore(pools *Pools, writer *Writer) *CheckInStore {
	return &CheckInStore{pools: pools, writer: writer}
}

// Record 幂等写入签到：ON CONFLICT DO NOTHING（同日重复登录不新增）。
func (s *CheckInStore) Record(ctx context.Context, userID int64, localDate string, at time.Time) error {
	return s.writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO daily_checkins (user_id, local_date, created_at)
			VALUES (?, ?, ?)
			ON CONFLICT(user_id, local_date) DO NOTHING`,
			userID, localDate, toUnix(at))
		if err != nil {
			return fmt.Errorf("写入签到: %w", err)
		}
		return nil
	})
}

// Streak 从 localDate 往前数连续签到天数。一次查询取最近 N 天日期，
// 在 Go 里按日期递减比对——避免递归 CTE 的复杂度，且次数有上限。
func (s *CheckInStore) Streak(ctx context.Context, userID int64, localDate string) (int, error) {
	const lookback = 400 // 上限：覆盖连续签到 7 天足够，防止极端数据拉长查询
	rows, err := s.pools.QueryContext(ctx, `
		SELECT local_date FROM daily_checkins
		WHERE user_id = ? AND local_date <= ?
		ORDER BY local_date DESC LIMIT ?`, userID, localDate, lookback)
	if err != nil {
		return 0, fmt.Errorf("查询签到记录: %w", err)
	}
	defer rows.Close()

	var dates []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return 0, fmt.Errorf("扫描签到日期: %w", err)
		}
		dates = append(dates, d)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("遍历签到记录: %w", err)
	}
	if len(dates) == 0 {
		return 0, nil
	}

	// 校验第一天的日期格式，防止脏数据导致的错误比对。
	if !validDate(dates[0]) {
		return 0, fmt.Errorf("签到日期格式非法: %q", dates[0])
	}
	streak := 0
	cur := localDate
	for _, d := range dates {
		if d != cur {
			break
		}
		streak++
		cur = prevDate(cur)
	}
	return streak, nil
}

// validDate 只接受 YYYY-MM-DD，避免日期字符串比较被脏数据干扰。
func validDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for _, i := range []int{0, 1, 2, 3, 5, 6, 8, 9} {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// prevDate 返回 YYYY-MM-DD 的前一天（不解析时区，纯日历减法）。
func prevDate(s string) string {
	year := atoi(s[0:4])
	month := atoi(s[5:7])
	day := atoi(s[8:10])
	day--
	if day < 1 {
		month--
		if month < 1 {
			month = 12
			year--
		}
		day = daysInMonth(year, month)
	}
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}

func daysInMonth(year, month int) int {
	switch month {
	case 2:
		if (year%4 == 0 && year%100 != 0) || year%400 == 0 {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	default:
		return 31
	}
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// UnlockNode 记录用户解锁了隐藏节点（幂等）。
func (s *CheckInStore) UnlockNode(ctx context.Context, userID, nodeID int64, at time.Time) error {
	return s.writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO user_node_unlocks (user_id, node_id, unlocked_at)
			VALUES (?, ?, ?)
			ON CONFLICT(user_id, node_id) DO NOTHING`,
			userID, nodeID, toUnix(at))
		if err != nil {
			return fmt.Errorf("记录节点解锁: %w", err)
		}
		return nil
	})
}

// IsNodeUnlocked 检查用户是否已解锁指定节点。
func (s *CheckInStore) IsNodeUnlocked(ctx context.Context, userID, nodeID int64) (bool, error) {
	var one int
	err := s.pools.QueryRowContext(ctx, `
		SELECT 1 FROM user_node_unlocks WHERE user_id = ? AND node_id = ?`, userID, nodeID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询节点解锁: %w", err)
	}
	return true, nil
}

// UnlockedNodes 返回用户已解锁的隐藏节点（JOIN user_node_unlocks × nodes）。
// 只返回彩蛋隐藏节点（slug='/dev/null'）；管理员归档的普通版块不算彩蛋节点。
func (s *CheckInStore) UnlockedNodes(ctx context.Context, userID int64) ([]node.Node, error) {
	rows, err := s.pools.QueryContext(ctx, `
		SELECT n.id, n.slug, n.name, n.description
		FROM user_node_unlocks u JOIN nodes n ON n.id = u.node_id
		WHERE u.user_id = ? AND n.slug = '/dev/null'
		ORDER BY n.id ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("查询已解锁节点: %w", err)
	}
	defer rows.Close()
	var out []node.Node
	for rows.Next() {
		var n node.Node
		if err := rows.Scan(&n.ID, &n.Slug, &n.Name, &n.Description); err != nil {
			return nil, fmt.Errorf("扫描已解锁节点: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UnlockSlug 解锁彩蛋隐藏节点。slug 必须精确为服务端固定的 "/dev/null"，
// 其它值一律拒绝——调用方不该、也不能通过参数选择解锁目标。
func (s *CheckInStore) UnlockSlug(ctx context.Context, userID int64, slug string, at time.Time) error {
	if slug != "/dev/null" {
		return fmt.Errorf("非法的彩蛋节点 slug: %q", slug)
	}
	var nodeID int64
	err := s.pools.QueryRowContext(ctx, `SELECT id FROM nodes WHERE slug = ?`, slug).Scan(&nodeID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("隐藏节点不存在: %s: %w", slug, err)
	}
	if err != nil {
		return fmt.Errorf("查询隐藏节点: %w", err)
	}
	return s.UnlockNode(ctx, userID, nodeID, at)
}
