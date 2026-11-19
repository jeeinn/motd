package config

// .env 自动加载（v0.3.1 用户明确要求的功能）。
//
// 口径（已拍板，勿反转）：
//  1. 查找顺序 cwd/.env → 可执行文件所在目录/.env，找到第一个即停，不合并两份；
//  2. 进程已有环境变量优先，.env 只填空缺（systemd EnvironmentFile 注入的凭据
//     绝不能被仓库里的文件静默覆盖）；
//  3. MOTD_DOTENV_DISABLED=true 可整体关闭（运维逃生舱）。
//
// 刻意不把副作用塞进 Load()：Load() 被大量单测直接调用，若它在内部读文件并
// 改进程环境，每个单测都会把项目根那份含真实授权码的 .env 读进来。副作用只留在
// 进程入口 cmd/server/main.go，测试则只用 dotEnvLookup 指向 t.TempDir()。
// 本文件零新依赖、纯标准库。

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DotEnvDisabledKey 是关闭 .env 自动加载的逃生舱开关；只认 true（大小写不敏感）。
const DotEnvDisabledKey = "MOTD_DOTENV_DISABLED"

// dotEnvLookup 把「当前目录 / 可执行文件」两个注入点抽出来，便于测试指向临时目录。
// 生产由 LoadDotEnv 用真实值填充；测试只调 applyDotEnv，绝不读项目根 .env。
type dotEnvLookup struct {
	cwdDir  string
	exePath string
}

// LoadDotEnv 供进程入口在 config.Load() 之前调用。
// 返回加载到的文件绝对路径；空串表示未加载（开关关闭或两处都没有 .env）。
// 返回的只是路径，调用方据此打一行日志，绝不打印任何键值。
func LoadDotEnv() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "" // 拿不到 cwd 就只查可执行文件目录，不算错误
	}
	exe, err := os.Executable()
	if err == nil {
		// Linux 下 os.Executable 可能给出 /proc/self/exe 之类软链，解析后再取目录。
		if resolved, e := filepath.EvalSymlinks(exe); e == nil {
			exe = resolved
		}
	} else {
		exe = ""
	}
	return applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe})
}

// applyDotEnv 是查找 → 解析 → 填空的纯编排；所有测试入口都走这里。
func applyDotEnv(l dotEnvLookup) (string, error) {
	if dotEnvDisabled() {
		return "", nil
	}
	path := l.find()
	if path == "" {
		return "", nil // 两处都没有文件不是错误：走纯环境变量
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开 .env 文件失败: %w", err) // 只含路径，不含内容
	}
	defer f.Close()
	values, err := parseDotEnv(f)
	if err != nil {
		return "", fmt.Errorf("读取 .env 文件失败: %w", err)
	}
	// 排序只为确定性：键之间无依赖，但可复现的顺序更容易排查问题。
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// 死口径：进程已设值优先。空串按「未设置」处理，与 config.env 一致。
		if os.Getenv(k) == "" {
			if err := os.Setenv(k, values[k]); err != nil {
				return "", fmt.Errorf("设置环境变量失败: %w", err)
			}
		}
	}
	return path, nil
}

// find 按 cwd → 可执行文件目录顺序返回第一个存在的普通文件。
func (l dotEnvLookup) find() string {
	if l.cwdDir != "" {
		if p := dotEnvFileIn(l.cwdDir); p != "" {
			return p
		}
	}
	if l.exePath != "" {
		if p := dotEnvFileIn(filepath.Dir(l.exePath)); p != "" {
			return p
		}
	}
	return ""
}

func dotEnvFileIn(dir string) string {
	p := filepath.Join(dir, ".env")
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return ""
	}
	return p
}

func dotEnvDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(DotEnvDisabledKey)), "true")
}

// parseDotEnv 解析一个 .env 流。口径：
//   - 整行 TrimSpace；空行与首字符为 '#' 的整行注释忽略；
//   - 可选 `export ` 前缀（后面必须是空白）；
//   - 第一个 '=' 分割；键必须非空且只含 ASCII 字母/数字/下划线（首位非数字）；
//   - 值 TrimSpace 后若被成对单/双引号包裹则去掉包裹符；不成对视为非法行；
//   - 未加引号的值里 '#' 是普通字符（授权码可能含 #，绝不擅自截断）；
//   - 非法行一律**静默忽略**，不报错也不部分应用；重复键以最后一行为准；
//   - 只有底层读失败才返回 error，且错误只带 IO 原文，不拼入任何行内容。
func parseDotEnv(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024) // 单行上限 1MiB，超出按读失败处理
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export") && len(line) > len("export") &&
			(line[len("export")] == ' ' || line[len("export")] == '\t') {
			line = strings.TrimSpace(line[len("export"):])
		}
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validEnvKey(key) {
			continue
		}
		value := strings.TrimSpace(raw)
		if n := len(value); n > 0 && (value[0] == '\'' || value[0] == '"') {
			if n < 2 || value[n-1] != value[0] {
				continue // 引号未闭合 → 非法行忽略
			}
			value = value[1 : n-1]
		}
		out[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, err // 不包装行内容，防止凭据经错误文本外泄
	}
	return out, nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		letter := c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		digit := c >= '0' && c <= '9'
		if !letter && !(digit && i > 0) {
			return false
		}
	}
	return true
}
