package terminal

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

// withDisplayLocation 让每个测试独立设置展示时区并在结束时复位，
// 避免测试间通过包级配置互相污染。
func withDisplayLocation(t *testing.T, loc *time.Location) {
	t.Helper()
	prev := DisplayLocation()
	SetDisplayLocation(loc)
	t.Cleanup(func() { SetDisplayLocation(prev) })
}

const sampleUnix int64 = 1_700_000_000 // 2023-11-14T22:13:20Z / 2023-11-15T06:13:20+08:00

func TestFormatTimeUsesDisplayLocation(t *testing.T) {
	cases := []struct {
		name string
		loc  *time.Location
		want string
	}{
		{"UTC", time.UTC, "11-14 22:13"},
		{"Asia/Shanghai", mustLocation(t, "Asia/Shanghai"), "11-15 06:13"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withDisplayLocation(t, c.loc)
			if got := formatTime(sampleUnix); got != c.want {
				t.Fatalf("formatTime = %q，期望 %q", got, c.want)
			}
		})
	}
}

func TestFormatTimeKeepsLayoutAndEmptyForNonPositive(t *testing.T) {
	withDisplayLocation(t, mustLocation(t, "Asia/Shanghai"))
	got := formatTime(sampleUnix)
	if len(got) != 11 || got[2] != '-' || got[5] != ' ' || got[8] != ':' {
		t.Fatalf("格式串必须保持 MM-DD HH:MM，实际 %q", got)
	}
	if strings.Contains(got, "2023") || strings.Contains(got, "CST") || strings.Contains(got, "+08") {
		t.Fatalf("终端窄，不得加年份或时区后缀: %q", got)
	}
	for _, sec := range []int64{0, -1, -9_223_372_036_854_775_808} {
		if got := formatTime(sec); got != "" {
			t.Errorf("formatTime(%d) 应为空串，实际 %q", sec, got)
		}
	}
}

func TestRenderScreenTimeFollowsDisplayLocation(t *testing.T) {
	sc := baseScreen(viewmodel.ScreenThreads)
	sc.Rows = []viewmodel.Row{{
		Index: 1, ID: 1, Title: "时区回归",
		Meta: viewmodel.RowMeta{CreatedAt: sampleUnix},
	}}

	for _, c := range []struct {
		name string
		loc  *time.Location
		want string
	}{
		{"UTC", time.UTC, "11-14 22:13"},
		{"Asia/Shanghai", mustLocation(t, "Asia/Shanghai"), "11-15 06:13"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withDisplayLocation(t, c.loc)
			out, err := RenderScreen(sc, 80)
			if err != nil {
				t.Fatalf("RenderScreen: %v", err)
			}
			if plain := stripSGR(out); !strings.Contains(plain, c.want) {
				t.Fatalf("屏幕时间应为 %q: %q", c.want, plain)
			}
			assertOnlySGR(t, out)
		})
	}
}

func TestDisplayLocationNilFallsBackToUTC(t *testing.T) {
	withDisplayLocation(t, time.UTC)
	SetDisplayLocation(nil)
	if loc := DisplayLocation(); loc != time.UTC {
		t.Fatalf("nil 必须回落到 UTC，实际 %v", loc)
	}
	if got := formatTime(sampleUnix); got != "11-14 22:13" {
		t.Fatalf("nil 回落 UTC 后时间错误: %q", got)
	}
}

// 并发读安全：多个 SSH 会话同时渲染，同时切换展示时区也不得 panic/竞态。
// 配合 go test -race 才能验证无锁读取的原子性。
func TestDisplayLocationConcurrentRead(t *testing.T) {
	withDisplayLocation(t, time.UTC)
	shanghai := mustLocation(t, "Asia/Shanghai")

	done := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-done:
				return
			default:
				SetDisplayLocation(shanghai)
				SetDisplayLocation(time.UTC)
			}
		}
	}()

	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 500; j++ {
				loc := DisplayLocation()
				if loc == nil {
					t.Error("DisplayLocation 不得返回 nil")
					return
				}
				if got := formatTime(sampleUnix); got != "11-14 22:13" && got != "11-15 06:13" {
					t.Errorf("并发渲染出现错误时间: %q", got)
					return
				}
			}
		}()
	}
	readers.Wait() // 读者全部跑完再停写者，保证读写确实重叠
	close(done)
	writer.Wait()
	SetDisplayLocation(time.UTC)
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}
