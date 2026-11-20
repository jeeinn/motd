package event

import (
	"testing"
	"time"
)

func TestDomainEventTypeValid(t *testing.T) {
	if EventUnknown.Valid() {
		t.Error("零值事件种类不应合法")
	}
	for _, k := range []DomainEventType{EventPostCreated, EventThreadCreated, EventUserRegistered, EventVoteCast, EventAdminAction} {
		if !k.Valid() {
			t.Errorf("事件种类 %d 应合法", k)
		}
	}
	if (DomainEventType(6)).Valid() || (DomainEventType(255)).Valid() {
		t.Error("越界事件种类不应合法")
	}
}

// 治理事件允许访客管理员：ActorID=0 但必须有指纹/命令/结果。
func TestAdminActionEventSupportsGuestActor(t *testing.T) {
	ev := validEvent()
	ev.Type = EventAdminAction
	ev.ActorID = 0
	ev.AdminFingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	ev.AdminCommand = "node create"
	ev.AdminResult = "success"
	if err := ev.Validate(); err != nil {
		t.Fatalf("访客治理事件应合法: %v", err)
	}
	ev.AdminFingerprint = ""
	if err := ev.Validate(); err == nil {
		t.Fatal("治理事件缺指纹必须拒绝，审计不能丢主体")
	}
	// 非治理事件仍然必须有 UserID。
	plain := validEvent()
	plain.ActorID = 0
	if err := plain.Validate(); err == nil {
		t.Fatal("普通领域事件缺 UserID 仍应拒绝")
	}
}

func validEvent() DomainEvent {
	return DomainEvent{
		ID:         "post:1",
		Type:       EventPostCreated,
		OccurredAt: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
		ActorID:    1,
	}
}

func TestDomainEventValidate(t *testing.T) {
	if err := validEvent().Validate(); err != nil {
		t.Fatalf("合法事件被拒绝: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*DomainEvent)
	}{
		{"缺幂等键", func(e *DomainEvent) { e.ID = "" }},
		{"非法种类", func(e *DomainEvent) { e.Type = EventUnknown }},
		{"缺时间", func(e *DomainEvent) { e.OccurredAt = time.Time{} }},
		{"缺发起者", func(e *DomainEvent) { e.ActorID = 0 }},
		{"负发起者", func(e *DomainEvent) { e.ActorID = -1 }},
	}
	for _, c := range cases {
		e := validEvent()
		c.mutate(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
}
