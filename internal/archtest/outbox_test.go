package archtest

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R12 静态守卫：outbox 的时序纪律必须落在源码结构上。
//
//  1. AppendOutbox 必须在 Write 闭包内（与业务写同事务，同提交同回滚）；
//  2. DomainEventSink.Publish 绝不能在写事务闭包内（handler 不得看到未提交状态）。
//
// 复用 TestSessionRevokerIsCalledAfterWriteCommit 的 AST 偏移手法，
// 并配负向注入证明守卫真会报警。

type callSpan struct {
	start int
	end   int
}

// collectOutboxPlacementIssues 扫描 dir 下非测试 Go 源码，返回时序违规描述。
func collectOutboxPlacementIssues(dir string) ([]string, error) {
	files, fsets, err := parseGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	totalAppend, totalPublish := 0, 0

	for i, f := range files {
		fset := fsets[i]
		var writes []callSpan
		var appends, publishes []int
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Write":
				writes = append(writes, callSpan{
					start: fset.Position(call.Pos()).Offset,
					end:   fset.Position(call.End()).Offset,
				})
			case "AppendOutbox":
				appends = append(appends, fset.Position(call.Pos()).Offset)
				totalAppend++
			case "Publish":
				publishes = append(publishes, fset.Position(call.Pos()).Offset)
				totalPublish++
			}
			return true
		})

		insideWrite := func(pos int) bool {
			for _, w := range writes {
				if pos > w.start && pos < w.end {
					return true
				}
			}
			return false
		}
		for _, pos := range appends {
			if !insideWrite(pos) {
				issues = append(issues, "AppendOutbox 必须出现在 Write 闭包内（与业务写同事务）")
			}
		}
		for _, pos := range publishes {
			if insideWrite(pos) {
				issues = append(issues, "Publish 绝不能出现在写事务闭包内（handler 只能在提交后执行）")
			}
		}
	}

	if totalAppend == 0 {
		issues = append(issues, "扫描不到 AppendOutbox 调用，守卫可能失效")
	}
	if totalPublish == 0 {
		issues = append(issues, "扫描不到 Publish 调用，守卫可能失效")
	}
	return issues, nil
}

func TestOutboxCallsArePlacedCorrectly(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "application", "forum")
	issues, err := collectOutboxPlacementIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("outbox 时序守卫未通过:\n  %s", strings.Join(issues, "\n  "))
	}
}

func TestGuardDetectsAppendOutboxOutsideWrite(t *testing.T) {
	tmp := t.TempDir()
	bad := `package forum

func bad(o *Outbox, sink DomainEventSink) {
	sink.Publish(ctx, ev)
	o.store.AppendOutbox(ctx, ev) // 故意放在写事务外
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectOutboxPlacementIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "AppendOutbox 必须出现在 Write 闭包内") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入的「AppendOutbox 在写事务外」未被检出: %v", issues)
	}
}

func TestGuardDetectsPublishInsideWrite(t *testing.T) {
	tmp := t.TempDir()
	bad := `package forum

func bad(sink DomainEventSink, tx WriteTx) {
	repos.Write(ctx, func(tx WriteTx) error {
		tx.AppendOutbox(ctx, ev)
		sink.Publish(ctx, ev) // 故意在写事务里调 handler
		return nil
	})
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectOutboxPlacementIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "Publish 绝不能出现在写事务闭包内") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入的「Publish 在写事务内」未被检出: %v", issues)
	}
}

func TestGuardDetectsMissingOutboxCalls(t *testing.T) {
	tmp := t.TempDir()
	empty := `package forum

func noop() {}
`
	if err := os.WriteFile(filepath.Join(tmp, "empty.go"), []byte(empty), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectOutboxPlacementIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("空实现应触发两条「守卫失效」告警，实际 %v", issues)
	}
}
