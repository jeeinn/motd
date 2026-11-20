// Package event 定义领域事件的纯数据结构（评审稿 v0.2 §2.4）。
//
// 领域事件在事务提交后产生，需可靠投递；交互事件（连接、按键、空闲）
// 生命周期短、允许丢失，不放在这里 —— 两类事件不共用总线。
// 本包只含值类型，不依赖任何上层实现。
package event

import (
	"fmt"
	"time"
)

// DomainEventType 是领域事件种类。用枚举而非字符串魔法值。
type DomainEventType uint8

const (
	EventUnknown        DomainEventType = iota
	EventPostCreated                    // 帖子创建（含主题首帖）
	EventThreadCreated                  // 主题创建
	EventUserRegistered                 // 用户注册
	EventVoteCast                       // 投票完成
	EventAdminAction                    // 治理动作（P2-M1：节点 CRUD，审计用）
)

// Valid 报告事件种类是否合法；零值不合法，发布前必须显式指定。
func (t DomainEventType) Valid() bool { return t >= EventPostCreated && t <= EventAdminAction }

// DomainEvent 是一次已提交业务动作的语义描述。
// ID 是幂等键（P0 用 "post:<id>"），供 P1 的 outbox 去重；
// 时间用 time.Time 而非 Unix 秒，保留纳秒精度做排序与诊断。
type DomainEvent struct {
	ID         string
	Type       DomainEventType
	OccurredAt time.Time
	// ActorID 对 EventAdminAction 可以为 0（白名单内的访客管理员）：
	// 该类事件的主体是下面的 AdminFingerprint，Validate 对此有专门分支。
	ActorID    int64
	NodeID     int64
	ThreadID   int64
	PostID     int64
	FirstPost  bool

	// 以下四个字段只属于 EventAdminAction，其它事件保持零值。
	// 指纹来自已认证会话（Principal.KeyFingerprint），不是命令参数。
	AdminFingerprint string
	AdminCommand     string
	AdminTarget      string
	AdminResult      string
}

// Validate 拦截畸形事件，避免订阅者各自猜测字段含义。
func (e DomainEvent) Validate() error {
	if !e.Type.Valid() {
		return fmt.Errorf("非法领域事件种类: %d", e.Type)
	}
	if e.ID == "" {
		return fmt.Errorf("领域事件缺少幂等键")
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("领域事件缺少发生时间")
	}
	// 治理事件允许访客管理员（UserID=0）：治理权来自白名单指纹，不来自账号。
	// 「谁」由 AdminFingerprint 承担，审计详情如实落地，绝不能因 UserID=0 丢事件。
	if e.Type == EventAdminAction {
		if e.AdminFingerprint == "" || e.AdminCommand == "" || e.AdminResult == "" {
			return fmt.Errorf("治理事件缺少指纹 / 命令 / 结果")
		}
		return nil
	}
	if e.ActorID <= 0 {
		return fmt.Errorf("领域事件缺少发起者")
	}
	return nil
}
