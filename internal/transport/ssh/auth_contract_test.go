package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	gossh "golang.org/x/crypto/ssh"
)

// 契约测试：transport 用 golang.org/x/crypto/ssh 算出的真实指纹，
// 必须能被 application/auth 的格式校验接受。
// 若哪天上游改了 FingerprintSHA256 的输出格式（例如加了 base64 填充），
// 这条会先失败，而不是等部署后所有 SSH 连接被拒之门外。
func TestFingerprintFormatMatchesAuthValidator(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	fp := gossh.FingerprintSHA256(signer.PublicKey())
	if !auth.ValidFingerprint(fp) {
		t.Fatalf("认证层算出的指纹必须被 application/auth 接受，实际 %q (len=%d)", fp, len(fp))
	}
}
