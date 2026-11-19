package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/presentation/terminal"
)

// /keys <fingerprint> 详情：创建/最近使用时间必须与论坛时间同源于展示时区，
// 后缀跟随配置，不再硬编码 " UTC"（用户实测反馈的直接触发点）。

const keySampleUnix int64 = 1_700_000_000

func withServerDisplayLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	prev := terminal.DisplayLocation()
	terminal.SetDisplayLocation(loc)
	t.Cleanup(func() { terminal.SetDisplayLocation(prev) })
	return loc
}

func TestFormatKeyTimeUsesDisplayTimezone(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"UTC", "2023-11-14 22:13:20 UTC"},
		{"Asia/Shanghai", "2023-11-15 06:13:20 CST"},
		{"America/New_York", "2023-11-14 17:13:20 EST"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withServerDisplayLocation(t, c.name)
			if got := formatKeyTime(keySampleUnix); got != c.want {
				t.Fatalf("formatKeyTime = %q，期望 %q", got, c.want)
			}
		})
	}
}

func TestRunKeysDetailUsesDisplayTimezone(t *testing.T) {
	ctx, _, _, pools, writer := setupMVPRuntime(t)
	fp := "SHA256:" + strings.Repeat("K", 43)
	created := time.Unix(keySampleUnix, 0).UTC()
	lastUsed := created.Add(time.Hour)

	err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ssh_keys (user_id, fingerprint, public_key, created_at, last_used_at)
			VALUES (1, ?, 'ssh-ed25519 AAAAExample tz@host', ?, ?)`,
			fp, created.Unix(), lastUsed.Unix())
		return err
	})
	if err != nil {
		t.Fatalf("预置公钥: %v", err)
	}
	h := newInputHandler().withKeyBinding(newKeyBindingService(pools, writer, noopKeyRevoker{}))
	principal := auth.Principal{Kind: auth.KindUser, UserID: 1, KeyFingerprint: fp}
	inv := command.Invocation{Name: "keys", Args: []string{fp}}

	t.Run("Asia/Shanghai", func(t *testing.T) {
		withServerDisplayLocation(t, "Asia/Shanghai")
		out := h.runKeys(ctx, principal, inv)
		for _, want := range []string{
			"创建：2023-11-15 06:13:20 CST",
			"最近使用：2023-11-15 07:13:20 CST",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("详情缺少 %q: %q", want, out)
			}
		}
		if strings.Contains(out, " UTC") {
			t.Fatalf("后缀不得再硬编码 UTC: %q", out)
		}
	})

	t.Run("UTC", func(t *testing.T) {
		withServerDisplayLocation(t, "UTC")
		out := h.runKeys(ctx, principal, inv)
		for _, want := range []string{
			"创建：2023-11-14 22:13:20 UTC",
			"最近使用：2023-11-14 23:13:20 UTC",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("详情缺少 %q: %q", want, out)
			}
		}
	})
}
