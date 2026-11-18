package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

const (
	hostKeyFileName = "ssh_host_ed25519_key"
	hostKeyFileMode = 0o600
	hostKeyDirMode  = 0o750
)

// LoadOrCreateHostKey 返回本机的 ed25519 host key 与它的 SHA256 指纹。
//
// 首次启动生成并落到 DataDir（权限 0600）；重启必须复用同一把钥匙，
// 否则客户端每次连接都会报 host key changed。私钥用 PKCS#8 PEM 保存，
// 不引额外依赖；写入走「临时文件 + 原子替换」，避免半文件被下次启动读到。
func LoadOrCreateHostKey(dataDir string) (gossh.Signer, string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, "", fmt.Errorf("DataDir 不能为空")
	}
	path := filepath.Join(dataDir, hostKeyFileName)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		return parseHostKey(b)
	case errors.Is(err, os.ErrNotExist):
		return createHostKey(dataDir, path)
	default:
		return nil, "", fmt.Errorf("读取 host key %s: %w", path, err)
	}
}

func createHostKey(dataDir, path string) (gossh.Signer, string, error) {
	if err := os.MkdirAll(dataDir, hostKeyDirMode); err != nil {
		return nil, "", fmt.Errorf("创建数据目录: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("生成 ed25519 密钥: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", fmt.Errorf("序列化 host key: %w", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, hostKeyFileMode); err != nil {
		return nil, "", fmt.Errorf("写入 host key 临时文件: %w", err)
	}
	// Windows 的 ACL 模型与 Unix 不同，chmod 无意义；部署目标是 Linux/systemd，
	// 那里必须显式收紧到 0600（os.WriteFile 会被 umask 影响）。
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmp, hostKeyFileMode); err != nil {
			_ = os.Remove(tmp)
			return nil, "", fmt.Errorf("设置 host key 权限: %w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, "", fmt.Errorf("落盘 host key: %w", err)
	}
	return parseHostKey(encoded)
}

func parseHostKey(b []byte) (gossh.Signer, string, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, "", fmt.Errorf("host key 不是合法 PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("解析 host key: %w", err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		return nil, "", fmt.Errorf("构造 SSH signer: %w", err)
	}
	return signer, gossh.FingerprintSHA256(signer.PublicKey()), nil
}
