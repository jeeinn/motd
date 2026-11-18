package ssh

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHostKeyStablePrivateAndRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	s1, fp1, err := LoadOrCreateHostKey(dir)
	if err != nil {
		t.Fatalf("首次生成 host key: %v", err)
	}
	s2, fp2, err := LoadOrCreateHostKey(dir)
	if err != nil {
		t.Fatalf("重载 host key: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("重启后 host key 指纹必须不变: %s != %s", fp1, fp2)
	}
	if !bytes.Equal(s1.PublicKey().Marshal(), s2.PublicKey().Marshal()) {
		t.Fatal("重载得到的公钥与首次生成不一致")
	}

	info, err := os.Stat(filepath.Join(dir, hostKeyFileName))
	if err != nil {
		t.Fatalf("host key 未落盘: %v", err)
	}
	// Windows ACL 模型与 Unix 不同；部署目标是 Linux/systemd，那里必须 0600。
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("host key 权限应为 0600，实际 %o", info.Mode().Perm())
	}

	other, fp3, err := LoadOrCreateHostKey(t.TempDir())
	if err != nil {
		t.Fatalf("另一数据目录生成 host key: %v", err)
	}
	_ = other
	if fp3 == fp1 {
		t.Fatal("不同数据目录不应复用同一把 host key")
	}

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, hostKeyFileName), []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateHostKey(bad); err == nil {
		t.Fatal("损坏的 host key 文件必须报错，不能被静默覆盖")
	}
}
