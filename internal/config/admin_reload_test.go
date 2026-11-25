package config

// R-15 / ADR-079 管理员白名单热重载的配置层回归：
// 强制重读 .env 的新值，不受"进程已设值优先"影响；非法指纹拒绝；
// 文件无该键时回退进程环境（systemd Environment= 部署方式）。

import (
	"os"
	"testing"
)

const adminReloadFP = "SHA256:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"

func TestReloadAdminFingerprintsRereadsFile(t *testing.T) {
	t.Setenv(DotEnvDisabledKey, "")
	t.Setenv("MOTD_ADMIN_FINGERPRINTS", "")
	cwd, _, exe := dotEnvTestDirs(t)
	writeFakeDotEnv(t, cwd, "MOTD_ADMIN_FINGERPRINTS="+adminReloadFP+"\n")

	got, err := reloadAdminFingerprintsFrom(dotEnvLookup{cwdDir: cwd, exePath: exe})
	if err != nil {
		t.Fatalf("重载应成功: %v", err)
	}
	if _, ok := got[adminReloadFP]; !ok {
		t.Fatalf("应重读到 .env 的新指纹，实际 %v", got)
	}
	// 关键：不能写回进程环境（避免污染其它配置/后续重载）。
	if v := os.Getenv("MOTD_ADMIN_FINGERPRINTS"); v != "" {
		t.Fatalf("重载不应修改进程环境，实际 %q", v)
	}
}

func TestReloadAdminFingerprintsRejectsInvalid(t *testing.T) {
	t.Setenv(DotEnvDisabledKey, "")
	cwd, _, exe := dotEnvTestDirs(t)
	writeFakeDotEnv(t, cwd, "MOTD_ADMIN_FINGERPRINTS=not-a-fingerprint\n")

	if _, err := reloadAdminFingerprintsFrom(dotEnvLookup{cwdDir: cwd, exePath: exe}); err == nil {
		t.Fatal("非法指纹必须返回 error，调用方据此保留旧名单")
	}
}

func TestReloadAdminFingerprintsFallsBackToEnv(t *testing.T) {
	t.Setenv(DotEnvDisabledKey, "")
	t.Setenv("MOTD_ADMIN_FINGERPRINTS", adminReloadFP)
	cwd, _, exe := dotEnvTestDirs(t)
	writeFakeDotEnv(t, cwd, "MOTD_OTHER_KEY=value\n") // 无管理员键

	got, err := reloadAdminFingerprintsFrom(dotEnvLookup{cwdDir: cwd, exePath: exe})
	if err != nil {
		t.Fatalf("回退进程环境应成功: %v", err)
	}
	if _, ok := got[adminReloadFP]; !ok {
		t.Fatalf("应回退到进程环境指纹，实际 %v", got)
	}
}
