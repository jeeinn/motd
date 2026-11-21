package ssh

// P4-T2：带宽计数只做内存原子累加，不碰数据库、不改领域模型。
// 这里用可控的 Channel 桩精确验证 Read/Write 的字节归属与单调性。

import (
	"io"
	"sync"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

type bandwidthStubChannel struct {
	readData []byte
	readOff  int
	written  []byte
}

var _ gossh.Channel = (*bandwidthStubChannel)(nil)

func (c *bandwidthStubChannel) Read(p []byte) (int, error) {
	if c.readOff >= len(c.readData) {
		return 0, io.EOF
	}
	n := copy(p, c.readData[c.readOff:])
	c.readOff += n
	return n, nil
}

func (c *bandwidthStubChannel) Write(p []byte) (int, error) {
	c.written = append(c.written, p...)
	return len(p), nil
}

func (*bandwidthStubChannel) Close() error                                   { return nil }
func (*bandwidthStubChannel) CloseWrite() error                              { return nil }
func (*bandwidthStubChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (*bandwidthStubChannel) Stderr() io.ReadWriter                          { return nil }

func newBandwidthTestSession(t *testing.T, id string, ch gossh.Channel) *Session {
	t.Helper()
	reg := presence.New()
	if err := reg.Connect(id, presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	s := newSession(id, ch, "fp", "pk", auth.Guest("fp"), reg)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSessionReadWriteAccumulateBandwidth(t *testing.T) {
	ch := &bandwidthStubChannel{readData: []byte("jjk")}
	s := newBandwidthTestSession(t, "s-bw", ch)
	bw := &Bandwidth{}
	s.setBandwidth(bw)

	buf := make([]byte, 8)
	n, err := s.Read(buf)
	if err != nil || n != 3 {
		t.Fatalf("读桩通道失败: n=%d err=%v", n, err)
	}
	if got := bw.Rx(); got != 3 {
		t.Errorf("rx 应累加实际读入的 3 字节，实际 %d", got)
	}
	if got := bw.Tx(); got != 0 {
		t.Errorf("只读不应增加 tx，实际 %d", got)
	}

	for _, payload := range []string{"hello", "!!"} {
		n, err = s.Write([]byte(payload))
		if err != nil || n != len(payload) {
			t.Fatalf("写桩通道失败: n=%d err=%v", n, err)
		}
	}
	if got := bw.Tx(); got != 7 {
		t.Errorf("tx 应单调累加到 7，实际 %d", got)
	}
	if got := bw.Rx(); got != 3 {
		t.Errorf("写不应改变 rx，实际 %d", got)
	}
}

func TestSessionWithoutExplicitBandwidthUsesProcessCounter(t *testing.T) {
	ch := &bandwidthStubChannel{readData: []byte("x")}
	s := newBandwidthTestSession(t, "s-bw-default", ch)
	before := ProcessBandwidth().Tx()

	if _, err := s.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if got := ProcessBandwidth().Tx(); got <= before {
		t.Errorf("未显式挂载计数器时也应累加到进程级计数: before=%d after=%d", before, got)
	}
}

func TestBandwidthAddIsAtomicAndIgnoresNonPositive(t *testing.T) {
	bw := &Bandwidth{}
	bw.AddTx(-1)
	bw.AddRx(0)
	if bw.Tx() != 0 || bw.Rx() != 0 {
		t.Fatalf("非正数字节不得计数: tx=%d rx=%d", bw.Tx(), bw.Rx())
	}

	const workers, rounds = 8, 100
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				bw.AddTx(1)
				bw.AddRx(2)
			}
		}()
	}
	wg.Wait()
	if got := bw.Tx(); got != workers*rounds {
		t.Errorf("并发 tx 计数丢失: got=%d want=%d", got, workers*rounds)
	}
	if got := bw.Rx(); got != 2*workers*rounds {
		t.Errorf("并发 rx 计数丢失: got=%d want=%d", got, 2*workers*rounds)
	}
}
