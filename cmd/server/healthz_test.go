package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
)

func newProbeTestDeps(t *testing.T) (*sqlite.Pools, *sqlite.Writer, *presence.Registry) {
	t.Helper()
	ctx := context.Background()
	pools, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)
	return pools, w, presence.New()
}

func TestHealthzExposesAuditAndLimitMetrics(t *testing.T) {
	pools, w, reg := newProbeTestDeps(t)
	audit := auth.NewAuditLogger(sqlite.NewAuthEventStore(w), auth.WithAuditCapacity(3))
	limiter := auth.NewRateLimiter()
	for i := 0; i < 2; i++ {
		if err := audit.Record(auth.AuditLoginFailure, 0, "github", "ip", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := limiter.AllowDeviceStart("ip"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	probeMux(pools, w, reg, audit, limiter).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz 状态 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"audit_depth=2", "audit_cap=3", "audit_dropped=0",
		"audit_flushed=0", "audit_failed=0", "limit_keys=1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("healthz 缺少 %q: %s", want, body)
		}
	}
}

func TestHealthzDroppedCounterAndBackwardCompat(t *testing.T) {
	pools, w, reg := newProbeTestDeps(t)

	// 未接审计/限流的调用方仍可用（旧签名兼容）。
	rec := httptest.NewRecorder()
	probeMux(pools, w, reg, nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "ok ") {
		t.Fatalf("兼容调用失败: %d %s", rec.Code, rec.Body.String())
	}

	audit := auth.NewAuditLogger(sqlite.NewAuthEventStore(w), auth.WithAuditCapacity(1))
	if err := audit.Record(auth.AuditLogout, 1, "github", "ip", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := audit.Record(auth.AuditLogout, 1, "github", "ip", `{}`); err == nil {
		t.Fatal("第二条应触发溢出")
	}
	rec = httptest.NewRecorder()
	probeMux(pools, w, reg, audit, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !strings.Contains(rec.Body.String(), "audit_depth=1") ||
		!strings.Contains(rec.Body.String(), "audit_dropped=1") {
		t.Fatalf("溢出计数未上报: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "limit_keys=") {
		t.Fatal("未接限流器时不应输出 limit_keys")
	}
}

// P4-T2：/healthz 暴露进程级 tx/rx；计数只增不减，且不产生任何 DB 提交。
func TestHealthzExposesBandwidthWithoutDBWrites(t *testing.T) {
	pools, w, reg := newProbeTestDeps(t)
	bw := &sshd.Bandwidth{}
	handler := probeMux(pools, w, reg, nil, nil, bw)
	probe := func() string {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("healthz 状态 %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	body := probe()
	for _, want := range []string{"tx_bytes=0", "rx_bytes=0"} {
		if !strings.Contains(body, want) {
			t.Errorf("healthz 缺少 %q: %s", want, body)
		}
	}
	// 旧五参数调用方也必须拿到带宽字段（走进程级兜底计数器）。
	legacy := httptest.NewRecorder()
	probeMux(pools, w, reg, nil, nil).ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	for _, want := range []string{"tx_bytes=", "rx_bytes="} {
		if !strings.Contains(legacy.Body.String(), want) {
			t.Errorf("兼容调用缺少 %q: %s", want, legacy.Body.String())
		}
	}

	commitsBefore := w.Commits()
	bw.AddRx(128)
	bw.AddTx(512)
	body = probe()
	if !strings.Contains(body, "tx_bytes=512") || !strings.Contains(body, "rx_bytes=128") {
		t.Fatalf("healthz 带宽数值不对: %s", body)
	}
	if got := w.Commits(); got != commitsBefore {
		t.Fatalf("字节累加不得写库: commits %d → %d", commitsBefore, got)
	}

	bw.AddTx(1)
	body = probe()
	if !strings.Contains(body, "tx_bytes=513") || !strings.Contains(body, "rx_bytes=128") {
		t.Fatalf("带宽计数必须单调且不回退: %s", body)
	}
}
