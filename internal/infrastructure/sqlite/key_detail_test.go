package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

func TestKeyDetailGetByFingerprint(t *testing.T) {
	ctx := context.Background()
	repos, _, w := setupKeyBindingRepo(t)
	alice := seedKeyBindingUser(t, w, "alice")

	created := time.Unix(1_700_000_000, 0).UTC()
	if err := repos.Write(ctx, func(tx auth.KeyBindingWriteTx) error {
		return tx.Insert(ctx, auth.NewKeyBinding{
			UserID: alice, Fingerprint: kbFingerprint,
			PublicKey: "ssh-ed25519 AAAAExample wei@host", CreatedAt: created,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.TouchLastUsed(ctx, kbFingerprint, created.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := repos.GetByFingerprint(ctx, alice, kbFingerprint)
	if err != nil {
		t.Fatalf("详情查询失败: %v", err)
	}
	if got.Fingerprint != kbFingerprint || got.CreatedAtUnix != created.Unix() ||
		!got.HasLastUsed || got.LastUsedUnix != created.Add(time.Hour).Unix() {
		t.Fatalf("详情字段不符: %+v", got)
	}
	if got.Label != "" {
		t.Fatalf("label 当前无输入口，应恒为空: %q", got.Label)
	}
}

func TestKeyDetailRejectsMissingAndCrossUser(t *testing.T) {
	ctx := context.Background()
	repos, _, w := setupKeyBindingRepo(t)
	alice := seedKeyBindingUser(t, w, "alice")
	bob := seedKeyBindingUser(t, w, "bob")
	if err := repos.Write(ctx, func(tx auth.KeyBindingWriteTx) error {
		return tx.Insert(ctx, auth.NewKeyBinding{
			UserID: alice, Fingerprint: kbFingerprint,
			PublicKey: "ssh-ed25519 AAAAExample wei@host", CreatedAt: time.Now(),
		})
	}); err != nil {
		t.Fatal(err)
	}

	// 跨用户：查得到行但归属不符，必须与「不存在」返回同一个错误。
	if _, err := repos.GetByFingerprint(ctx, bob, kbFingerprint); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("跨用户查询应拒绝，实际 %v", err)
	}
	// 不存在。
	if _, err := repos.GetByFingerprint(ctx, alice, kbFingerprint2); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("不存在的指纹应拒绝，实际 %v", err)
	}
}

func TestKeyDetailRejectsRevokedKey(t *testing.T) {
	ctx := context.Background()
	repos, _, w := setupKeyBindingRepo(t)
	alice := seedKeyBindingUser(t, w, "alice")
	if err := repos.Write(ctx, func(tx auth.KeyBindingWriteTx) error {
		if err := tx.Insert(ctx, auth.NewKeyBinding{
			UserID: alice, Fingerprint: kbFingerprint,
			PublicKey: "ssh-ed25519 AAAAExample wei@host", CreatedAt: time.Now(),
		}); err != nil {
			return err
		}
		_, err := tx.Revoke(ctx, alice, kbFingerprint, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repos.GetByFingerprint(ctx, alice, kbFingerprint); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("已吊销钥匙不应出现在详情，实际 %v", err)
	}
}
