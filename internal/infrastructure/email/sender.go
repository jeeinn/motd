package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 本包只有一种投递实现：SMTP over 隐式 TLS（465）。
// 不预留第二 Provider、不做「按配置切换」的抽象——那属于孤儿能力（§2.9）。

const (
	// maxConcurrent 是有界并发上限：验证码邮件是低频动作，4 路足够，
	// 再多只会把对端 163 的连接数打满。
	maxConcurrent = 4
	// sendTimeout 独立于写事务的 3s 超时：网络调用绝不进写队列。
	sendTimeout = 10 * time.Second
	dialTimeout = 5 * time.Second
)

var (
	// ErrSendFailed 是投递失败的唯一对外错误。底层错误（可能含 AUTH 串）
	// 绝不外泄：Send 只返回本哨兵，细节留给调用方写「发送失败」固定文案。
	ErrSendFailed = errors.New("邮件投递失败")
	// ErrBusy 表示并发已满，调用方应稍后重试（不阻塞会话主循环）。
	ErrBusy = errors.New("邮件投递通道繁忙")
)

// Sender 是投递端口。只声明一个方法，避免把「验证码状态」泄漏进投递层
// （状态在 email_tokens，由 application 管）。
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Config 是构造 SMTPSender 的必填项，由 config.EmailConfig 映射而来。
// 凭据只在进程内存中存在，不提供任何序列化方法。
type Config struct {
	Host string
	Port int
	User string
	Pass string
	From string
}

// SMTPSender 是唯一的 Sender 实现。sem 把并发压到 4 路，deliver 是真正的
// 投递动作（构造时固定为 SMTP，测试可在同包内替换以注入失败，不改接口）。
type SMTPSender struct {
	cfg     Config
	sem     chan struct{}
	deliver func(context.Context, *Config, string, string, string) error
	mu      sync.Mutex
	closed  bool
}

var _ Sender = (*SMTPSender)(nil)

// NewSMTPSender 构造投递器。缺项属于装配错误：直接 panic，不留到运行期
// 退化成「发给空地址」或「用空授权码认证」。
func NewSMTPSender(cfg Config) *SMTPSender {
	if cfg.Host == "" || cfg.Port <= 0 || cfg.User == "" || cfg.Pass == "" || cfg.From == "" {
		panic("email.NewSMTPSender: host/port/user/pass/from 不能为空")
	}
	return &SMTPSender{
		cfg:     cfg,
		sem:     make(chan struct{}, maxConcurrent),
		deliver: deliverSMTP,
	}
}

// Send 在事务外投递一封纯文本邮件。参数经最小语法收口（拒绝 CRLF 注入），
// 底层失败一律归一为 ErrSendFailed，绝不把 AUTH / 密码原文带进错误链。
func (s *SMTPSender) Send(ctx context.Context, to, subject, body string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// 先判取消：semaphore 的 select 里「有空位」与「ctx 已取消」同时就绪时
	// 会随机选，导致已取消的请求仍触发投递。这里先收口一次。
	if err := ctx.Err(); err != nil {
		return err
	}
	to = strings.TrimSpace(to)
	// 收件人 / 主题里出现 CR/LF 会改写 SMTP 信封或头，直接拒绝。
	if to == "" || strings.ContainsAny(to, "\r\n") || strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("%w: 收件人或主题非法", ErrSendFailed)
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrBusy
	}

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: 投递器已关闭", ErrSendFailed)
	}

	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	// deliver 的错误一律丢弃原文：net/smtp 在 AUTH 失败时可能回显服务器响应，
	// 而服务器响应有可能回显凭据片段。对外只暴露哨兵。
	if err := s.deliver(sctx, &s.cfg, to, subject, body); err != nil {
		return ErrSendFailed
	}
	return nil
}

// Close 标记投递器停止工作（幂等）。close 不等待在途投递，Session 侧不应
// 依赖它做优雅关闭；进程退出由上层 writer / audit 的停机顺序负责。
func (s *SMTPSender) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// deliverSMTP 用 465 隐式 TLS + PLAIN AUTH 连 163。
// tls.Client 包住原始连接后再交给 smtp.NewClient，后者识别 *tls.Conn 后
// 才会允许 PlainAuth（普通 smtp.NewClient 会拒绝明文 AUTH）。
func deliverSMTP(ctx context.Context, cfg *Config, to, subject, body string) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: cfg.Host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return err
	}

	client, err := smtp.NewClient(tlsConn, cfg.Host)
	if err != nil {
		_ = tlsConn.Close()
		return err
	}
	defer func() { _ = client.Close() }()

	// 传输超时也覆盖 SMTP 对话阶段：textproto 的 deadline 由底层 tls.Conn 承担。
	if deadline, ok := ctx.Deadline(); ok {
		if err := tlsConn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	if err := client.Auth(smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)); err != nil {
		return err
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	msg := buildMessage(cfg.From, to, subject, body)
	if _, err := w.Write([]byte(msg)); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// buildMessage 生成最小 RFC 5322 文本邮件。subject / body 由 application
// 用固定模板构造（验证码是 6 位数字），这里再做一次 CRLF 收口作为纵深防御。
func buildMessage(from, to, subject, body string) string {
	cleanHeader := func(s string) string {
		return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", cleanHeader(from))
	fmt.Fprintf(&b, "To: %s\r\n", cleanHeader(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", cleanHeader(subject))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	normalized := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(body)
	b.WriteString(normalized)
	if !strings.HasSuffix(normalized, "\n") {
		b.WriteString("\r\n")
	}
	return b.String()
}
