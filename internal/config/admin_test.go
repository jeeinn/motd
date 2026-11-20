package config

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
)

const adminTestFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 43 位

func TestParseAdminFingerprints(t *testing.T) {
	second := "SHA256:" + strings.Repeat("B", 43)
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"空值", "", 0},
		{"纯空白", "   ", 0},
		{"单个", adminTestFP, 1},
		{"多个带空格", "  " + adminTestFP + " , " + second + "  ", 2},
		{"重复去重", adminTestFP + "," + adminTestFP, 1},
		{"空项忽略", ",," + adminTestFP + ",,", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAdminFingerprints(tc.raw)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("白名单长度 = %d，期望 %d（%v）", len(got), tc.want, got)
			}
			if tc.want > 0 {
				if _, ok := got[adminTestFP]; !ok {
					t.Fatalf("缺少首个指纹: %v", got)
				}
			}
		})
	}
}

func TestParseAdminFingerprintsRejectsInvalid(t *testing.T) {
	cases := []string{
		"ssh-ed25519 AAAAexample",
		"MD5:11:22:33",
		"SHA256:too-short",
		"sha256:" + strings.Repeat("A", 43), // 大小写敏感：前缀也必须一致
		adminTestFP + ",bad-fingerprint",
	}
	for _, raw := range cases {
		got, err := ParseAdminFingerprints(raw)
		if err == nil {
			t.Errorf("非法白名单 %q 应启动失败，实际解析为 %v", raw, got)
		}
		if len(got) != 0 {
			t.Errorf("失败时不得返回部分白名单: %v", got)
		}
	}
}

func TestLoadAdminFingerprintsFromEnv(t *testing.T) {
	t.Setenv("MOTD_ADMIN_FINGERPRINTS", adminTestFP)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.AdminFingerprints[adminTestFP]; !ok {
		t.Fatalf("白名单未进入 Config: %v", cfg.AdminFingerprints)
	}
}

func TestLoadRejectsInvalidAdminFingerprints(t *testing.T) {
	t.Setenv("MOTD_ADMIN_FINGERPRINTS", "not-a-fingerprint")
	if _, err := Load(); err == nil {
		t.Fatal("非法管理员指纹必须让启动失败")
	}
}

// 防止配置层的格式尺子与认证层漂移：两边必须接受/拒绝同一批样本。
func TestAdminFingerprintFormatMatchesAuth(t *testing.T) {
	samples := []string{
		adminTestFP,
		"SHA256:" + strings.Repeat("a", 43),
		"SHA256:" + strings.Repeat("A", 42),
		"SHA256:" + strings.Repeat("_", 43),
		"MD5:aa:bb",
		"",
	}
	for _, s := range samples {
		_, err := ParseAdminFingerprints(s)
		gotConfig := err == nil && s != ""
		gotAuth := auth.ValidFingerprint(s)
		if gotConfig != gotAuth {
			t.Errorf("样本 %q 的格式判定不一致: config=%v authRuler=%v", s, gotConfig, gotAuth)
		}
	}
}
