package forum

import (
	"errors"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/domain/user"
)

// checkWrite 是所有写用例的统一入口（service.go），这里直接断言它，
// 等价于断言 Principal → Actor 映射后调用 CreateThread/CreatePost 的结果。
func TestActorFromPrincipalGuestIsReadOnly(t *testing.T) {
	actor := ActorFromPrincipal(auth.Guest("SHA256:guest"), user.StatusActive)
	if !actor.Guest || actor.UserID != 0 {
		t.Fatalf("访客 Principal 必须映射成 Guest Actor，实际 %+v", actor)
	}
	if err := actor.checkWrite(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("访客写用例必须返回明确的 ErrReadOnly 而非 panic，实际 %v", err)
	}
}

func TestActorFromPrincipalUserStatusIsNotCachedByAuth(t *testing.T) {
	p := auth.Principal{Kind: auth.KindUser, UserID: 7}
	cases := []struct {
		name   string
		status user.Status
		want   error
	}{
		{"active 可写", user.StatusActive, nil},
		{"pending 冻结", user.StatusPending, ErrForbidden},
		{"deleting 冻结", user.StatusDeleting, ErrForbidden},
		{"deleted 冻结", user.StatusDeleted, ErrForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actor := ActorFromPrincipal(p, tc.status)
			if actor.Guest || actor.UserID != 7 || actor.Status != tc.status {
				t.Fatalf("用户 Principal 映射错误: %+v", actor)
			}
			err := actor.checkWrite()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("active 应可写，实际 %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v，期望 %v", err, tc.want)
			}
		})
	}
}
