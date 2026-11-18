package registry

import (
	"context"
	"testing"

	"github.com/jeeinn/motd/internal/domain/event"
)

type noopHandler struct{}

func (noopHandler) Handle(context.Context, event.DomainEvent, Context) error { return nil }

func plugin(name string, subs ...event.DomainEventType) Plugin {
	return Plugin{
		Name:                name,
		Version:             "0.1.0",
		Commands:            []Command{{Name: "/" + name, Usage: "/" + name}},
		DomainSubscriptions: subs,
		Handler:             noopHandler{},
	}
}

func TestRegisterAndLookup(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	if err := Register(plugin("b", event.EventVoteCast)); err != nil {
		t.Fatal(err)
	}
	if err := Register(plugin("a", event.EventPostCreated, event.EventVoteCast)); err != nil {
		t.Fatal(err)
	}

	all := All()
	if len(all) != 2 || all[0].Name != "a" || all[1].Name != "b" {
		t.Fatalf("All 应按名字排序返回全部插件: %+v", all)
	}
	if got := Subscribers(event.EventPostCreated); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("订阅过滤错误: %+v", got)
	}
	if got := Subscribers(event.EventVoteCast); len(got) != 2 {
		t.Fatalf("应返回两个订阅者，实际 %d", len(got))
	}
	if got := Subscribers(event.EventUserRegistered); len(got) != 0 {
		t.Fatalf("无人订阅的种类应返回空: %+v", got)
	}
}

func TestRegisterRejectsInvalidPlugin(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	cases := []struct {
		name string
		p    Plugin
	}{
		{"空名", plugin("")},
		{"缺处理器", Plugin{Name: "x"}},
		{"非法订阅", plugin("x", event.EventUnknown)},
		{"无名命令", Plugin{Name: "x", Commands: []Command{{}}, Handler: noopHandler{}}},
	}
	for _, c := range cases {
		if err := Register(c.p); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
	if len(All()) != 0 {
		t.Fatal("被拒绝的插件不应进入注册表")
	}

	if err := Register(plugin("dup")); err != nil {
		t.Fatal(err)
	}
	if err := Register(plugin("dup")); err == nil {
		t.Fatal("重名注册应被拒绝")
	}
	if len(All()) != 1 {
		t.Fatal("重名不应覆盖已有插件")
	}
}

func TestMustRegisterPanicsOnDuplicate(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	MustRegister(plugin("dup"))
	defer func() {
		if recover() == nil {
			t.Error("MustRegister 遇到重复注册应 panic，而不是静默跳过")
		}
	}()
	MustRegister(plugin("dup"))
}

// 插件表必须只读暴露：外部拿到 All() 的返回值后改动，不能污染注册表。
func TestAllReturnsDefensiveCopies(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	MustRegister(plugin("safe", event.EventPostCreated))

	first := All()
	first[0].Commands[0].Name = "tampered"
	first[0].DomainSubscriptions[0] = event.EventUnknown
	first[0].Name = "tampered-name"

	second := All()
	if len(second) != 1 || second[0].Name != "safe" ||
		second[0].Commands[0].Name != "/safe" ||
		second[0].DomainSubscriptions[0] != event.EventPostCreated {
		t.Fatalf("注册表被外部改动污染: %+v", second)
	}
}
