// Package registry 维护编译期注册的插件表（评审稿 v0.2 §2.4）。
//
// 只有 init() 注册，没有动态加载、插件市场或沙箱。插件通过 Context
// 只能拿到「只读查询」与「申请解锁成就」两条能力，拿不到数据库句柄。
package registry

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/domain/event"
)

// Command 是插件登记的命令。P0 只登记不执行；命令分发在 M3 接入。
type Command struct {
	Name        string
	Usage       string
	Description string
}

// ReadOnlyQuery 是插件唯一允许的数据入口：只读，且只暴露公开语义字段。
// 实现由 application 层提供，插件拿不到 *sql.DB / *sql.Rows。
type ReadOnlyQuery interface {
	ThreadTitle(ctx context.Context, threadID int64) (string, error)
}

// Context 是派发给插件的受限能力集合。
// Achievements 是唯一的写路径，且必须经过 application/user 校验（§2.4 权限表）。
type Context struct {
	Read         ReadOnlyQuery
	Achievements user.AchievementGate
}

// Handler 处理订阅的领域事件。
type Handler interface {
	Handle(ctx context.Context, e event.DomainEvent, c Context) error
}

// Plugin 是编译期注册的插件描述。
type Plugin struct {
	Name                string
	Version             string
	Commands            []Command
	DomainSubscriptions []event.DomainEventType
	Handler             Handler
}

var (
	mu      sync.RWMutex
	plugins = make(map[string]Plugin)
)

// Register 登记插件；非法描述与重名都直接拒绝，避免运行时才发现配置坏了。
func Register(p Plugin) error {
	if p.Name == "" {
		return fmt.Errorf("插件名不能为空")
	}
	if p.Handler == nil {
		return fmt.Errorf("插件 %q 缺少事件处理器", p.Name)
	}
	for _, t := range p.DomainSubscriptions {
		if !t.Valid() {
			return fmt.Errorf("插件 %q 订阅了非法事件种类 %d", p.Name, t)
		}
	}
	for _, c := range p.Commands {
		if c.Name == "" {
			return fmt.Errorf("插件 %q 登记了无名命令", p.Name)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if _, exists := plugins[p.Name]; exists {
		return fmt.Errorf("插件 %q 重复注册", p.Name)
	}
	plugins[p.Name] = clonePlugin(p)
	return nil
}

// MustRegister 供 init() 使用：注册失败应在启动期就暴露，而不是静默跳过。
func MustRegister(p Plugin) {
	if err := Register(p); err != nil {
		panic(err)
	}
}

// All 返回按名字排序的副本；外部修改返回值不能影响注册表。
func All() []Plugin {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Plugin, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, clonePlugin(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Subscribers 返回订阅了某种领域事件的插件副本。
func Subscribers(t event.DomainEventType) []Plugin {
	var out []Plugin
	for _, p := range All() {
		for _, sub := range p.DomainSubscriptions {
			if sub == t {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// Reset 清空注册表，仅供测试隔离使用；运行时不应调用。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	plugins = make(map[string]Plugin)
}

func clonePlugin(p Plugin) Plugin {
	out := p
	out.Commands = append([]Command(nil), p.Commands...)
	out.DomainSubscriptions = append([]event.DomainEventType(nil), p.DomainSubscriptions...)
	return out
}
