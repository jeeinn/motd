package archtest

import (
	"os"
	"path/filepath"
	"testing"
)

// P0-13 验收：在线状态与心跳不得写 SQLite（§2.6）。
// 静态保证 presence 包不接触任何持久化依赖；运行时「写库次数为 0」
// 另由 infrastructure/sqlite 的集成测试用真实 Writer.Commits() 验证。
var presenceBannedImports = []string{
	"database/sql",
	"modernc.org/sqlite",
	"github.com/jeeinn/motd/internal/infrastructure",
}

func TestPresenceHasNoPersistenceImports(t *testing.T) {
	root := findModuleRoot(t)
	problems, err := scanBannedImports(root, filepath.Join("internal", "application", "presence"), presenceBannedImports)
	if err != nil {
		t.Fatalf("扫描 presence 包失败: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("在线注册表出现持久化依赖:\n  %v", problems)
	}
}

func TestPresenceGuardDetectsInjection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "application", "presence")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `package presence

import _ "database/sql"
import _ "github.com/jeeinn/motd/internal/infrastructure/sqlite"
`
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := scanBannedImports(root, filepath.Join("internal", "application", "presence"), presenceBannedImports)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 2 {
		t.Fatalf("注入 2 条持久化 import 应全部检出，实际 %d 条: %v", len(problems), problems)
	}
}
