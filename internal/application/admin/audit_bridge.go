package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/domain/event"
)

// AuditBridge 把治理领域事件翻译成 P1 的 auth_events 审计通道。
//
// 调用时机：forum.Outbox 在写事务**提交后**投递事件时（cmd 注册的订阅者）。
// 这里只调 AuditLogger.Record——入有界环形缓冲即返回，绝不在此落库、不起 goroutine；
// 落库由 cmd 持有的 ticker 调 Flush 完成。队列满时把错误交回 outbox 重试，
// 不静默丢审计。
func AuditBridge(logger *auth.AuditLogger) func(context.Context, event.DomainEvent) error {
	return func(_ context.Context, e event.DomainEvent) error {
		if logger == nil {
			return fmt.Errorf("治理审计器未装配")
		}
		if e.Type != event.EventAdminAction {
			return nil
		}
		detail, err := json.Marshal(map[string]string{
			"command":     e.AdminCommand,
			"target":      e.AdminTarget,
			"result":      e.AdminResult,
			"fingerprint": e.AdminFingerprint,
			"node_id":     strconv.FormatInt(e.NodeID, 10),
			"occurred_at": e.OccurredAt.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return fmt.Errorf("序列化治理审计字段: %w", err)
		}
		return logger.Record(auth.AuditAdminAction, e.ActorID, "ssh", "", string(detail))
	}
}
