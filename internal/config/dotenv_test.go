package config

// .env 自动加载回归。安全铁律：全部使用 t.TempDir() 自建占位 .env，
// 绝不读取、打印、断言项目根那份含真实授权码的 .env，也不复制进 testdata。
// 所有入口都走 applyDotEnv(注入临时目录)，不调用 LoadDotEnv()，杜绝误读真实文件。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dotEnvTestDirs(t *testing.T) (cwdDir, exeDir, exePath string) {
	t.Helper()
	base := t.TempDir()
	cwdDir = filepath.Join(base, "cwd")
	exeDir = filepath.Join(base, "bin")
	for _, d := range []string{cwdDir, exeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return cwdDir, exeDir, filepath.Join(exeDir, "motd-test-bin")
}

func writeFakeDotEnv(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 进程已有值优先：.env 同键不同值不得覆盖，也不得进日志。
func TestDotEnvProcessEnvWins(t *testing.T) {
	t.Setenv(DotEnvDisabledKey, "")
	t.Setenv("MOTD_DOTENV_TEST_KEY", "from-process")
	cwd, _, exe := dotEnvTestDirs(t)
	writeFakeDotEnv(t, cwd, "MOTD_DOTENV_TEST_KEY=from-file\n")

	src, err := applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe})
	if err != nil || src == "" {
		t.Fatalf("应加载 cwd .env: src=%q err=%v", src, err)
	}
	if got := os.Getenv("MOTD_DOTENV_TEST_KEY"); got != "from-process" {
		t.Fatalf("进程值必须优先，实际 %q", got)
	}
	// 启动日志只会出现这一行：只含路径，不含任何键值。
	line := "配置来源 file=" + src
	if strings.Contains(line, "from-file") || strings.Contains(line, "from-process") {
		t.Fatalf("来源日志不得含键值: %q", line)
	}
}

// 查找顺序：cwd 命中即停且不合并；cwd 无则退到可执行文件目录；都没有走纯 env。
func TestDotEnvLookupOrder(t *testing.T) {
	t.Run("cwd 命中且不合并程序目录", func(t *testing.T) {
		t.Setenv(DotEnvDisabledKey, "")
		t.Setenv("MOTD_DOTENV_CWD", "")
		t.Setenv("MOTD_DOTENV_EXE", "")
		cwd, exeDir, exe := dotEnvTestDirs(t)
		cwdFile := writeFakeDotEnv(t, cwd, "MOTD_DOTENV_CWD=cwd-hit\n")
		writeFakeDotEnv(t, exeDir, "MOTD_DOTENV_EXE=exe-hit\n")

		src, err := applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe})
		if err != nil || src != cwdFile {
			t.Fatalf("应只用 cwd 文件: src=%q want=%q err=%v", src, cwdFile, err)
		}
		if os.Getenv("MOTD_DOTENV_CWD") != "cwd-hit" || os.Getenv("MOTD_DOTENV_EXE") != "" {
			t.Fatal("命中 cwd 后不得再合并可执行文件目录的 .env")
		}
	})

	t.Run("程序目录兜底", func(t *testing.T) {
		t.Setenv(DotEnvDisabledKey, "")
		t.Setenv("MOTD_DOTENV_EXE", "")
		cwd, exeDir, exe := dotEnvTestDirs(t)
		exeFile := writeFakeDotEnv(t, exeDir, "MOTD_DOTENV_EXE=exe-hit\n")

		src, err := applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe})
		if err != nil || src != exeFile {
			t.Fatalf("应用程序目录文件: src=%q want=%q err=%v", src, exeFile, err)
		}
		if os.Getenv("MOTD_DOTENV_EXE") != "exe-hit" {
			t.Fatal("程序目录 .env 未生效")
		}
	})

	t.Run("两处都没有", func(t *testing.T) {
		t.Setenv(DotEnvDisabledKey, "")
		cwd, _, exe := dotEnvTestDirs(t)
		src, err := applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe})
		if err != nil || src != "" {
			t.Fatalf("没有 .env 不是错误: src=%q err=%v", src, err)
		}
	})
}

// 解析容错：注释/空行/空白/引号/export 前缀/非法行/重复键。
func TestDotEnvParserFormats(t *testing.T) {
	t.Setenv(DotEnvDisabledKey, "")
	for _, k := range []string{
		"MOTD_DOTENV_A", "MOTD_DOTENV_B", "MOTD_DOTENV_C", "MOTD_DOTENV_D",
		"MOTD_DOTENV_E", "MOTD_DOTENV_F", "exportMOTD_DOTENV_G",
		"invalid-key", "1LEADING_DIGIT",
	} {
		t.Setenv(k, "")
	}
	cwd, _, exe := dotEnvTestDirs(t)
	writeFakeDotEnv(t, cwd, `# 整行注释
   # 缩进注释

MOTD_DOTENV_A = first
export MOTD_DOTENV_B = "double quoted"
MOTD_DOTENV_C='single quoted'
MOTD_DOTENV_D=
MOTD_DOTENV_E=value # inline 是普通字符
这一行没有等号
MOTD_DOTENV_F="未闭合引号
exportMOTD_DOTENV_G=no-prefix
invalid-key=ignored
1LEADING_DIGIT=ignored
MOTD_DOTENV_A=last-wins
`)

	if _, err := applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"MOTD_DOTENV_A":       "last-wins", // 重复键最后一行为准
		"MOTD_DOTENV_B":       "double quoted",
		"MOTD_DOTENV_C":       "single quoted",
		"MOTD_DOTENV_D":       "", // 合法空值等于未设置
		"MOTD_DOTENV_E":       "value # inline 是普通字符",
		"exportMOTD_DOTENV_G": "no-prefix", // export 前缀要求后面是空白
		"invalid-key":         "",          // 非法键整行忽略
		"1LEADING_DIGIT":      "",          // 数字开头整行忽略
		"MOTD_DOTENV_F":       "",          // 引号未闭合整行忽略
	}
	for k, want := range cases {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q，期望 %q", k, got, want)
		}
	}
}

// 逃生舱：MOTD_DOTENV_DISABLED=true（大小写不敏感）时一个键都不加载。
func TestDotEnvDisabledSwitch(t *testing.T) {
	for _, raw := range []string{"true", "TRUE", " true "} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(DotEnvDisabledKey, raw)
			t.Setenv("MOTD_DOTENV_OFF", "")
			cwd, _, exe := dotEnvTestDirs(t)
			writeFakeDotEnv(t, cwd, "MOTD_DOTENV_OFF=should-not-load\n")
			src, err := applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe})
			if err != nil || src != "" {
				t.Fatalf("关闭时不读文件: src=%q err=%v", src, err)
			}
			if os.Getenv("MOTD_DOTENV_OFF") != "" {
				t.Fatal("关闭后不得加载任何键")
			}
		})
	}
}

// 读失败的错误只带 IO 原文，不带任何行内容（凭据不因错误外泄）。
type failAfterLineReader struct {
	line []byte
	done bool
}

func (r *failAfterLineReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("disk read failed")
	}
	r.done = true
	return copy(p, r.line), nil
}

func TestDotEnvReadErrorNeverLeaksLineContent(t *testing.T) {
	reader := &failAfterLineReader{line: []byte("MOTD_SMTP_PASS=PLACEHOLDER_SECRET_123\n")}
	_, err := parseDotEnv(reader)
	if err == nil {
		t.Fatal("读失败应返回错误")
	}
	if strings.Contains(err.Error(), "PLACEHOLDER_SECRET") || strings.Contains(err.Error(), "MOTD_SMTP_PASS") {
		t.Fatalf("错误里不得出现任何键值: %q", err.Error())
	}
}

// 配置字段只读展示开关值；Load 本身不读文件（安全边界）。
func TestConfigLoadDotEnvDisabledField(t *testing.T) {
	t.Setenv(DotEnvDisabledKey, "true")
	cfg, err := Load()
	if err != nil || !cfg.DotEnvDisabled {
		t.Fatalf("开关应体现到配置: %+v err=%v", cfg, err)
	}
	t.Setenv(DotEnvDisabledKey, "")
	cfg, err = Load()
	if err != nil || cfg.DotEnvDisabled {
		t.Fatalf("默认应为开启加载: %+v err=%v", cfg, err)
	}
}
