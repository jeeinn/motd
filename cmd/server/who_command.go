package main

// ADR-065：/whoami（本人资料）与 /who（在线人员，参数分页）的输出层。
//
// 信任域（ADR-019）：固定标签 / 本地数字走 safe.RenderSystem；@username 是
// DB 用户数据，单独走 safe.RenderUser 净化链，两段绝不拼进同一次渲染调用。
// 失败一律固定文案，不回显错误链（ADR-026 零回显）。

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/presence"
	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// whoPageSize 是 /who 单页行数（在线规模为个位数，15 行足够覆盖多页边界测试）。
const whoPageSize = 15

// segSystem 写一段服务端固定文本（不补换行）。渲染失败静默丢弃，绝不裸写。
func segSystem(sess io.Writer, text string) {
	rendered, err := safe.RenderSystem(text, safe.StylePlain)
	if err == nil {
		_, _ = io.WriteString(sess, rendered)
	}
}

// segUser 写一段外部数据（RenderUser 净化链，不补换行）。
func segUser(sess io.Writer, text string) {
	_, _ = io.WriteString(sess, safe.RenderUser(text, safe.Options{}))
}

// segCRLF 结束当前行。
func segCRLF(sess io.Writer) { _, _ = io.WriteString(sess, "\r\n") }

// runWhoami 输出本人资料：账号 / 注册时间 / 声望等级（ADR-065）。
// 访客给固定提示；端口缺失 / DB 失败 / 记录不存在统一固定文案。
func (h *inputHandler) runWhoami(ctx context.Context, sess io.Writer, p auth.Principal) {
	if !p.IsUser() {
		writeSystem(sess, "请先 /login github 登录后查看我的信息\r\n")
		return
	}
	pr, ok := h.users.(appuser.ProfileReader)
	if !ok {
		writeSystem(sess, "个人信息查询尚未就绪\r\n")
		return
	}
	profiles, err := pr.ProfilesByIDs(ctx, []int64{p.UserID})
	if err != nil {
		writeSystem(sess, "个人信息查询失败，请稍后重试\r\n")
		return
	}
	v, found := profiles[p.UserID]
	if !found {
		writeSystem(sess, "个人信息查询失败，请稍后重试\r\n")
		return
	}

	writeSystem(sess, "我的信息\r\n")
	// 账号行分段渲染：固定前缀 RenderSystem，@username 走 RenderUser，ID 段回 RenderSystem。
	segSystem(sess, "  账号：")
	segUser(sess, "@"+v.Username)
	segSystem(sess, "（ID "+strconv.FormatInt(v.UserID, 10)+"）")
	segCRLF(sess)

	writeSystem(sess, "  注册："+formatKeyTime(v.CreatedAt)+"\r\n")

	level, next := karmaLevel(v.Karma)
	writeSystem(sess, fmt.Sprintf("  声望 %d · 等级 L%d · %s\r\n", v.Karma, level, progressDesc(level, next)))
}

// runWho 列出当前在线人员（ADR-065）。参数只接受正整数页码；单次快照派生
// 计数与名单，避免多次 Snapshot() 漂移。所有人可见（含访客），不加 requireAdmin。
func (h *inputHandler) runWho(ctx context.Context, sess io.Writer, inv command.Invocation) {
	page := 1
	if len(inv.Args) > 0 {
		n, err := strconv.Atoi(inv.Args[0])
		if err != nil || n < 1 {
			// 非数字 / 0 / 负数 / @xxx 一律固定提示，不查库、不回显原文。
			writeSystem(sess, "用法：/who [页]（页为正整数，从 1 开始）\r\n")
			return
		}
		page = n
	}

	var snap []presence.Session
	timeout := presence.DefaultHeartbeatTimeout
	now := time.Now()
	if h.presence != nil {
		snap = h.presence.Snapshot()
		timeout = h.presence.HeartbeatTimeout()
	}
	online, active, guests := whoCounts(snap, now, timeout)

	// 用户名投影失败降级为 #ID（与 fillRowAuthors 同口径），不阻塞名单展示。
	profiles := map[int64]appuser.ProfileView{}
	if pr, ok := h.users.(appuser.ProfileReader); ok {
		if ids := whoUserIDs(snap); len(ids) > 0 {
			if p, err := pr.ProfilesByIDs(ctx, ids); err == nil {
				profiles = p
			}
		}
	}
	entries := whoEntries(snap, profiles, now)

	totalPages := (len(entries) + whoPageSize - 1) / whoPageSize
	writeSystem(sess, fmt.Sprintf("在线 %d · 活跃 %d · 访客 %d\r\n", online, active, guests))
	if totalPages == 0 {
		writeSystem(sess, "当前没有在线人员\r\n")
		return
	}
	if page > totalPages {
		writeSystem(sess, fmt.Sprintf("页码超出范围（共 %d 页）\r\n", totalPages))
		return
	}

	start := (page - 1) * whoPageSize
	end := start + whoPageSize
	if end > len(entries) {
		end = len(entries)
	}
	for _, e := range entries[start:end] {
		segSystem(sess, "  ")
		if e.username == "" {
			segUser(sess, "#"+strconv.FormatInt(e.userID, 10))
		} else {
			segUser(sess, "@"+e.username)
		}
		segSystem(sess, "    "+whoStateLabel(e.state)+" · "+whoOnlineText(e.onlineSec))
		segCRLF(sess)
	}
	writeSystem(sess, fmt.Sprintf("—— 第 %d/%d 页（/who <页> 翻页）\r\n", page, totalPages))
}

// whoCounts 从单次快照派生在线 / 活跃 / 访客计数。访客 = UserID 0 的连接
// （含认证中），不逐行列名。活跃用 presence.ActiveWithin（含心跳窗口）判定，
// 与状态栏、/stats 的 presence.Active() 同口径，避免三处给出不同「活跃 N」。
func whoCounts(snap []presence.Session, now time.Time, timeout time.Duration) (online, active, guests int) {
	for _, s := range snap {
		online++
		if presence.ActiveWithin(s, now, timeout) {
			active++
		}
		if s.UserID == 0 {
			guests++
		}
	}
	return online, active, guests
}

func whoUserIDs(snap []presence.Session) []int64 {
	ids := make([]int64, 0, len(snap))
	seen := make(map[int64]struct{}, len(snap))
	for _, s := range snap {
		if s.UserID <= 0 {
			continue
		}
		if _, ok := seen[s.UserID]; ok {
			continue
		}
		seen[s.UserID] = struct{}{}
		ids = append(ids, s.UserID)
	}
	return ids
}

// whoEntry 是一行在线人员的展示数据。
type whoEntry struct {
	userID    int64
	username  string // 空 → 渲染 #ID
	state     presence.State
	onlineSec int64
	lastSeen  time.Time
}

// whoEntries 组装用户行并排序：活跃态（浏览/编辑）在前，其余按 LastSeenAt
// 倒序；同键稳定。访客连接不进名单。
func whoEntries(snap []presence.Session, profiles map[int64]appuser.ProfileView, now time.Time) []whoEntry {
	entries := make([]whoEntry, 0, len(snap))
	for _, s := range snap {
		if s.UserID <= 0 {
			continue
		}
		e := whoEntry{
			userID:    s.UserID,
			state:     s.State,
			onlineSec: int64(now.Sub(s.ConnectedAt).Seconds()),
			lastSeen:  s.LastSeenAt,
		}
		if v, ok := profiles[s.UserID]; ok {
			e.username = v.Username
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ai, aj := entries[i].state.Active(), entries[j].state.Active()
		if ai != aj {
			return ai
		}
		return entries[i].lastSeen.After(entries[j].lastSeen)
	})
	return entries
}

func whoStateLabel(s presence.State) string {
	switch s {
	case presence.StateBrowsing:
		return "浏览中"
	case presence.StateComposing:
		return "编辑中"
	case presence.StateIdle:
		return "空闲"
	default:
		return "认证中"
	}
}

// whoOnlineText 把在线秒数格式化为固定文案：<1 分钟按整分钟记。
func whoOnlineText(sec int64) string {
	if sec < 60 {
		return "在线 <1 分钟"
	}
	return fmt.Sprintf("在线 %d 分钟", sec/60)
}
