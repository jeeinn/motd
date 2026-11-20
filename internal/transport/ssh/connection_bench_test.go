package ssh

// v0.5.0 / P3-M4（ADR-059）：最小压测集 —— 场景 1「SSH 连接建立/拆除」。
// 复用同包装配逻辑，零新增导出 API。不做 100/300/500/1000 全量；
// 这里只给核心闭环在回环环境的相对延迟基线，不构成 1 Mbps/1000 在线验收证据。

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

// benchSigner 生成测试用公钥签名器（与 newClientSigner 同逻辑，接受 *testing.B）。
func benchSigner(b *testing.B) gossh.Signer {
	b.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	s, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkSSHHandshake(b *testing.B) {
	for _, parallel := range []int{8, 50} { // ≤ 默认 MaxConns=100
		b.Run(fmt.Sprintf("parallel=%d", parallel), func(b *testing.B) {
			cfg := Config{
				DataDir:  b.TempDir(),
				Addr:     "127.0.0.1:0",
				Resolver: testResolver{},
				Presence: presence.New(),
				Handler:  newScriptedHandler(),
			}
			s, err := New(cfg)
			if err != nil {
				b.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			go func() { _ = s.Serve(ctx) }()
			defer func() { cancel(); s.Close() }()
			// Addr:0 时监听地址由 Serve 异步绑定，轮询等待就绪。
			var addr string
			for i := 0; i < 500; i++ {
				if addr = s.Addr(); addr != "" {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			if addr == "" {
				b.Fatal("监听未就绪")
			}
			clientCfg := &gossh.ClientConfig{
				User:            "tester",
				Auth:            []gossh.AuthMethod{gossh.PublicKeys(benchSigner(b))},
				HostKeyCallback: gossh.InsecureIgnoreHostKey(),
				Timeout:         3 * time.Second,
			}

			b.SetParallelism(parallel)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					cl, err := gossh.Dial("tcp", addr, clientCfg)
					if err != nil {
						b.Errorf("握手失败: %v", err)
						return
					}
					_ = cl.Close()
				}
			})
		})
	}
}
