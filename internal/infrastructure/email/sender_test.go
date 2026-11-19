package email

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testSender() *SMTPSender {
	return NewSMTPSender(Config{
		Host: "smtp.example.com", Port: 465,
		User: "user@example.com", Pass: "placeholder-auth-code", From: "user@example.com",
	})
}

func TestNewSMTPSenderRejectsIncompleteConfig(t *testing.T) {
	base := Config{Host: "h", Port: 465, User: "u", Pass: "p", From: "f"}
	fields := []func(*Config){
		func(c *Config) { c.Host = "" },
		func(c *Config) { c.Port = 0 },
		func(c *Config) { c.User = "" },
		func(c *Config) { c.Pass = "" },
		func(c *Config) { c.From = "" },
	}
	for i, mutate := range fields {
		t.Run(fmt.Sprintf("field_%d", i), func(t *testing.T) {
			c := base
			mutate(&c)
			defer func() {
				if recover() == nil {
					t.Fatal("缺项必须在构造期 panic，不能留到运行期静默降级")
				}
			}()
			_ = NewSMTPSender(c)
		})
	}
}

func TestSendPassesEnvelopeToDeliver(t *testing.T) {
	s := testSender()
	var gotTo, gotSubject, gotBody string
	s.deliver = func(_ context.Context, cfg *Config, to, subject, body string) error {
		gotTo, gotSubject, gotBody = to, subject, body
		if cfg.Host != "smtp.example.com" || cfg.Port != 465 {
			t.Errorf("投递层收到的配置不对: %+v", cfg)
		}
		return nil
	}
	if err := s.Send(context.Background(), "dest@example.com", "验证码", "123456"); err != nil {
		t.Fatal(err)
	}
	if gotTo != "dest@example.com" || gotSubject != "验证码" || gotBody != "123456" {
		t.Fatalf("信封参数未正确下传: to=%q subject=%q body=%q", gotTo, gotSubject, gotBody)
	}
}

// 关键安全断言：底层错误（可能含 AUTH 串）绝不能经 Send 外泄。
func TestSendNeverLeaksCredentialInError(t *testing.T) {
	s := testSender()
	secret := "AUTH_SECRET_SHOULD_NOT_ESCAPE"
	s.deliver = func(context.Context, *Config, string, string, string) error {
		return fmt.Errorf("535 Authentication failed: %s", secret)
	}
	err := s.Send(context.Background(), "dest@example.com", "验证码", "123456")
	if err == nil {
		t.Fatal("投递失败必须返回错误")
	}
	if !errors.Is(err, ErrSendFailed) {
		t.Fatalf("失败必须归一为 ErrSendFailed，实际 %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误里绝不能带 AUTH 串: %q", err.Error())
	}
}

func TestSendRejectsHeaderInjection(t *testing.T) {
	s := testSender()
	called := false
	s.deliver = func(context.Context, *Config, string, string, string) error {
		called = true
		return nil
	}
	for _, to := range []string{"a@example.com\r\nBcc: evil@example.com", "a@example.com\nX: y", "  "} {
		if err := s.Send(context.Background(), to, "验证码", "123456"); !errors.Is(err, ErrSendFailed) {
			t.Errorf("to=%q 应拒绝，实际 %v", to, err)
		}
	}
	if err := s.Send(context.Background(), "a@example.com", "主题\r\nBcc: evil", "123456"); !errors.Is(err, ErrSendFailed) {
		t.Errorf("主题含 CRLF 应拒绝，实际 %v", err)
	}
	if called {
		t.Fatal("非法参数不得触发真实投递")
	}
}

func TestSendHonorsContextCancellation(t *testing.T) {
	s := testSender()
	s.deliver = func(ctx context.Context, _ *Config, _, _, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Send(ctx, "a@example.com", "验证码", "123456"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应直接返回取消错误，实际 %v", err)
	}
}

// 有界并发：第 5 路必须快速失败 ErrBusy，而不是阻塞会话主循环。
func TestSendBoundsConcurrency(t *testing.T) {
	s := testSender()
	var active, peak int64
	release := make(chan struct{})
	s.deliver = func(context.Context, *Config, string, string, string) error {
		cur := atomic.AddInt64(&active, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
				break
			}
		}
		<-release
		atomic.AddInt64(&active, -1)
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, maxConcurrent+1)
	for i := 0; i <= maxConcurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Send(context.Background(), "a@example.com", "验证码", "123456")
		}(i)
	}
	// 等前 4 路都进入 deliver，再断言第 5 路已被拒。
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&active) < maxConcurrent && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&peak); got != maxConcurrent {
		t.Fatalf("并发峰值应为 %d，实际 %d", maxConcurrent, got)
	}
	busy := 0
	for i, err := range errs {
		if errors.Is(err, ErrBusy) {
			busy++
		} else if err != nil {
			t.Errorf("第 %d 路意外错误: %v", i, err)
		}
	}
	if busy != 1 {
		t.Fatalf("应有且仅有 1 路因并发上限快速失败，实际 %d", busy)
	}
}

func TestBuildMessageSanitizesBodyNewlines(t *testing.T) {
	msg := buildMessage("from@example.com", "to@example.com", "主题", "第一行\r\n第二行\r第三行")
	if strings.Contains(msg, "\r\n第二行") {
		t.Fatal("正文的裸 CR 应被归一，不得产生额外 SMTP 行")
	}
	if !strings.Contains(msg, "第一行\n第二行\n第三行") {
		t.Fatalf("正文换行未归一: %q", msg)
	}
}
