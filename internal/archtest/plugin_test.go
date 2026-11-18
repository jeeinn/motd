package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// P0-12 验收项：archtest 证明插件包无法 import infrastructure/sqlite。
// 除了 §2.2 的禁止表，这里对 plugin 目录做一次专门扫描（含标准库/第三方 import），
// 因为 groupOfImport 只识别模块内部路径，漏掉 database/sql 这类"绕道"依赖。
var pluginBannedImports = []string{
	"database/sql",
	"modernc.org/sqlite",
	"github.com/jeeinn/motd/internal/infrastructure",
	"github.com/jeeinn/motd/cmd/",
}

// scanBannedImports 返回 subdir 下所有命中禁用表的 import 描述。
func scanBannedImports(root, subdir string, banned []string) ([]string, error) {
	dir := filepath.Join(root, subdir)
	var problems []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if strings.Contains(p, b) {
					problems = append(problems, filepath.ToSlash(rel)+": 禁止 import "+p)
				}
			}
		}
		return nil
	})
	return problems, err
}

func TestPluginPackagesHaveNoDBAccess(t *testing.T) {
	root := findModuleRoot(t)
	problems, err := scanBannedImports(root, filepath.Join("internal", "plugin"), pluginBannedImports)
	if err != nil {
		t.Fatalf("扫描 plugin 目录失败: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("插件包出现 %d 处数据库/入口层依赖:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	// 同时确认 §2.2 禁止表覆盖 plugin → infrastructure。
	if !slices.Contains(forbidden["plugin"], "infrastructure") {
		t.Fatal("依赖禁止表漏了 plugin → infrastructure 规则")
	}
}

func TestPluginGuardDetectsInjection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "plugin", "bad")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `package bad

import _ "database/sql"
import _ "modernc.org/sqlite"
import _ "github.com/jeeinn/motd/internal/infrastructure/sqlite"
`
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := scanBannedImports(root, filepath.Join("internal", "plugin"), pluginBannedImports)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 3 {
		t.Fatalf("注入 3 条违规 import 应全部检出，实际 %d 条:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

// 事件端口必须定义在使用方：application 不能反向依赖 plugin。
// plugin/events.DomainBus 靠结构化接口匹配实现 forum.DomainEventSink。
func TestApplicationDoesNotImportPlugin(t *testing.T) {
	root := findModuleRoot(t)
	problems, err := scanBannedImports(root, filepath.Join("internal", "application"), []string{
		"github.com/jeeinn/motd/internal/plugin",
	})
	if err != nil {
		t.Fatalf("扫描 application 目录失败: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("application 反向依赖 plugin:\n  %s", strings.Join(problems, "\n  "))
	}
}
