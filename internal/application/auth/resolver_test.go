package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeLookup struct {
	status KeyStatus
	err    error
	gotFP  string
	calls  int
}

func (f *fakeLookup) Lookup(_ context.Context, fp string) (KeyStatus, error) {
	f.calls++
	f.gotFP = fp
	return f.status, f.err
}

var testFingerprint = "SHA256:" + strings.Repeat("A", 43)

func TestResolveThreeStates(t *testing.T) {
	cases := []struct {
		name       string
		status     KeyStatus
		wantKind   Kind
		wantUserID int64
		wantErr    error
	}{
		{"未绑定→访客只读", KeyStatus{}, KindGuest, 0, nil},
		{"已绑定未吊销→用户", KeyStatus{Found: true, UserID: 42}, KindUser, 42, nil},
		{"已吊销→拒绝认证", KeyStatus{Found: true, Revoked: true, UserID: 42}, KindGuest, 0, ErrKeyRevoked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lk := &fakeLookup{status: tc.status}
			p, err := NewResolver(lk).Resolve(context.Background(), testFingerprint)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if p != (Principal{}) {
					t.Fatalf("失败时必须返回零值 Principal，实际 %+v", p)
				}
				return
			}
			if p.Kind != tc.wantKind || p.UserID != tc.wantUserID {
				t.Fatalf("Principal = %+v，期望 kind=%v userID=%d", p, tc.wantKind, tc.wantUserID)
			}
			if p.KeyFingerprint != testFingerprint {
				t.Fatalf("指纹必须原样带回，实际 %q", p.KeyFingerprint)
			}
			if p.IsUser() != (tc.wantKind == KindUser) || p.IsGuest() != (tc.wantKind == KindGuest) {
				t.Fatalf("IsUser/IsGuest 与 Kind 不一致: %+v", p)
			}
		})
	}
}

func TestResolveRejectsBadFingerprintBeforeLookup(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"deadbeef",
		"MD5:aa:bb:cc",
		"SHA256:short",
		"SHA256:" + strings.Repeat("A", 42),
		"SHA256:" + strings.Repeat("A", 44),
		"SHA256:" + strings.Repeat("A", 42) + "=", // RawStdEncoding 无填充
		"SHA256:" + strings.Repeat("A", 42) + " ", // 空白字符
		"SHA256:" + strings.Repeat("A", 42) + "中", // 非法 base64 字符
	}
	for _, fp := range bad {
		lk := &fakeLookup{status: KeyStatus{Found: true, UserID: 1}}
		_, err := NewResolver(lk).Resolve(context.Background(), fp)
		if !errors.Is(err, ErrEmptyFingerprint) && !errors.Is(err, ErrInvalidFingerprint) {
			t.Errorf("%q 应被指纹格式校验拦下，实际 %v", fp, err)
		}
		if lk.calls != 0 {
			t.Errorf("%q 非法指纹不应触发查库，实际查了 %d 次", fp, lk.calls)
		}
	}
}

func TestResolveDoesNotFallBackToGuestOnLookupError(t *testing.T) {
	sentinel := errors.New("read pool 不可用")
	lk := &fakeLookup{err: sentinel}
	p, err := NewResolver(lk).Resolve(context.Background(), testFingerprint)
	if !errors.Is(err, sentinel) {
		t.Fatalf("查库错误必须原样向上传播，实际 %v", err)
	}
	if p != (Principal{}) {
		t.Fatalf("查库失败绝不能降级成访客放行，实际 %+v", p)
	}
}

func TestResolveRejectsInvalidUserID(t *testing.T) {
	for _, uid := range []int64{0, -1} {
		lk := &fakeLookup{status: KeyStatus{Found: true, UserID: uid}}
		_, err := NewResolver(lk).Resolve(context.Background(), testFingerprint)
		if err == nil || errors.Is(err, ErrKeyRevoked) {
			t.Fatalf("UserID=%d 是脏数据，应返回解析失败而非放行，实际 %v", uid, err)
		}
	}
}

func TestResolveTrimsFingerprint(t *testing.T) {
	lk := &fakeLookup{status: KeyStatus{Found: true, UserID: 7}}
	if _, err := NewResolver(lk).Resolve(context.Background(), "  "+testFingerprint+"\t"); err != nil {
		t.Fatal(err)
	}
	if lk.gotFP != testFingerprint {
		t.Fatalf("传给查库端口的应是 trim 后的指纹，实际 %q", lk.gotFP)
	}
}

func TestNewResolverRejectsNilLookup(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil KeyLookup 属于装配期错误，必须 panic")
		}
	}()
	NewResolver(nil)
}

func TestValidFingerprint(t *testing.T) {
	if !ValidFingerprint(testFingerprint) {
		t.Fatal("合法指纹应被接受")
	}
	if !ValidFingerprint("SHA256:" + strings.Repeat("a/09+Z", 7) + "A") {
		t.Fatal("base64 字符集 A-Z a-z 0-9 + / 应全部允许")
	}
	for _, fp := range []string{"", "SHA256:", "sha256:" + strings.Repeat("A", 43)} {
		if ValidFingerprint(fp) {
			t.Errorf("%q 不应被接受", fp)
		}
	}
}
