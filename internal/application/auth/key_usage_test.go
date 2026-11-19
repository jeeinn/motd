package auth

import (
	"context"
	"errors"
	"testing"
)

type stubResolver struct {
	p   Principal
	err error
	n   int
}

func (s *stubResolver) Resolve(_ context.Context, _ string) (Principal, error) {
	s.n++
	return s.p, s.err
}

type countingUsage struct{ seen []string }

func (c *countingUsage) NoteUse(fp string) { c.seen = append(c.seen, fp) }

const usageTestFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestUsageRecordingResolverOnlyRecordsBoundUser(t *testing.T) {
	inner := &stubResolver{p: Principal{Kind: KindUser, UserID: 7, KeyFingerprint: usageTestFP}}
	usage := &countingUsage{}
	r := NewUsageRecordingResolver(inner, usage)

	p, err := r.Resolve(context.Background(), usageTestFP)
	if err != nil || p.UserID != 7 || inner.n != 1 {
		t.Fatalf("解析语义被破坏: p=%+v err=%v calls=%d", p, err, inner.n)
	}
	if len(usage.seen) != 1 || usage.seen[0] != usageTestFP {
		t.Fatalf("命中绑定钥匙应记一次使用: %v", usage.seen)
	}
}

func TestUsageRecordingResolverSkipsGuestAndErrors(t *testing.T) {
	guest := &stubResolver{p: Guest(usageTestFP)}
	guestUsage := &countingUsage{}
	if _, err := NewUsageRecordingResolver(guest, guestUsage).Resolve(context.Background(), usageTestFP); err != nil {
		t.Fatal(err)
	}
	if len(guestUsage.seen) != 0 {
		t.Fatalf("访客未绑定钥匙，不应记录使用: %v", guestUsage.seen)
	}

	failed := &stubResolver{err: errors.New("db down")}
	failedUsage := &countingUsage{}
	if _, err := NewUsageRecordingResolver(failed, failedUsage).Resolve(context.Background(), usageTestFP); err == nil {
		t.Fatal("解析错误应原样返回")
	}
	if len(failedUsage.seen) != 0 {
		t.Fatalf("解析失败不应记录使用: %v", failedUsage.seen)
	}
}

func TestNewUsageRecordingResolverRejectsNilDeps(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("空依赖应在装配期 panic")
		}
	}()
	_ = NewUsageRecordingResolver(nil, &countingUsage{})
}
