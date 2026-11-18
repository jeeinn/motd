package auth

import (
	"errors"
	"fmt"
	"testing"
)

func TestPrincipalGuestAndUser(t *testing.T) {
	g := Guest("SHA256:abc")
	if !g.IsGuest() || g.IsUser() {
		t.Fatalf("Guest 应为访客，实际 IsGuest=%v IsUser=%v", g.IsGuest(), g.IsUser())
	}
	if g.UserID != 0 || g.KeyFingerprint != "SHA256:abc" {
		t.Fatalf("Guest 字段不正确: %+v", g)
	}

	u := Principal{Kind: KindUser, UserID: 42, KeyFingerprint: "SHA256:def"}
	if !u.IsUser() || u.IsGuest() {
		t.Fatalf("已登录用户判定错误: %+v", u)
	}

	// Kind 与 UserID 必须同时满足，避免只靠 UserID 就拿到写权限。
	weird := Principal{Kind: KindGuest, UserID: 42}
	if weird.IsUser() {
		t.Fatal("KindGuest 但 UserID>0 不应被判为已登录")
	}
}

func TestErrKeyRevokedIsSentinel(t *testing.T) {
	wrapped := fmt.Errorf("解析失败: %w", ErrKeyRevoked)
	if !errors.Is(wrapped, ErrKeyRevoked) {
		t.Fatal("ErrKeyRevoked 必须能被 errors.Is 识别，SSH 入口据此区分「拒绝认证」与「解析失败」")
	}
}
