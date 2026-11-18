package event

import (
	"testing"
	"time"
)

func TestDomainEventTypeValid(t *testing.T) {
	if EventUnknown.Valid() {
		t.Error("零值事件种类不应合法")
	}
	for _, k := range []DomainEventType{EventPostCreated, EventThreadCreated, EventUserRegistered, EventVoteCast} {
		if !k.Valid() {
			t.Errorf("事件种类 %d 应合法", k)
		}
	}
	if (DomainEventType(5)).Valid() || (DomainEventType(255)).Valid() {
		t.Error("越界事件种类不应合法")
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
