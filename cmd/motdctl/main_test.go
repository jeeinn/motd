package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// 演练时踩过：flag 包遇到首个位置参数就停止解析，
// `restore <file> --target <path>` 会把 --target 吞掉。这里固化归一化行为。
func TestNormalizeFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"后置 target", []string{"b.db", "--target", "x.db"}, []string{"--target", "x.db", "b.db"}},
		{"前置 target", []string{"--target", "x.db", "b.db"}, []string{"--target", "x.db", "b.db"}},
		{"等号形式", []string{"b.db", "--target=x.db"}, []string{"--target=x.db", "b.db"}},
		{"无 flag", []string{"b.db"}, []string{"b.db"}},
	}
	for _, c := range cases {
		got := normalizeFlags(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: normalizeFlags(%v) = %v，期望 %v", c.name, c.in, got, c.want)
		}
	}
}

func TestRestoreRequiresBothArguments(t *testing.T) {
	if err := cmdRestore(t.Context(), []string{"b.db"}); err == nil {
		t.Error("缺少 --target 时应返回用法错误")
	}
	if err := cmdRestore(t.Context(), []string{"--target", "x.db"}); err == nil {
		t.Error("缺少备份文件时应返回用法错误")
	}
}

// fts-rebuild 的验收：索引被清空后，命令必须从 posts 回填，
// 且 status<>normal 的行不能被索引。
func TestFTSRebuildRestoresIndexFromPosts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "motd.db")
	pools, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	if err := pools.Close(); err != nil {
		t.Fatal(err)
	}

	// 建好库后用独立连接准备数据；关闭后再跑命令，避免 WAL 连接占用。
	setup, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1_700_000_000)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (username, status, created_at, updated_at) VALUES ('fts_user','active',?,?)`, []any{now, now}},
		{`INSERT INTO nodes (slug, name) VALUES ('/t','测试')`, nil},
		{`INSERT INTO threads (node_id,user_id,title,last_reply_at,created_at) VALUES (1,1,'主题',?,?)`, []any{now, now}},
		{`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (1,1,'你好世界 visible','normal',?)`, []any{now}},
		{`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (1,1,'你好世界 hidden','deleted',?)`, []any{now}},
	} {
		if _, err := setup.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("准备数据失败: %v", err)
		}
	}
	// 模拟索引损坏：只清倒排与标记，posts 正文保持完好。
	for _, q := range []string{
		`INSERT INTO posts_fts(posts_fts) VALUES('delete-all')`,
		`DELETE FROM posts_fts_indexed`,
	} {
		if _, err := setup.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}

	if err := cmdFTSRebuild(ctx, []string{"--db", path}); err != nil {
		t.Fatalf("fts-rebuild 失败: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var hits, tracked int
	if err := db.QueryRow(`SELECT count(*) FROM posts_fts WHERE posts_fts MATCH '"你好世"'`).Scan(&hits); err != nil {
		t.Fatalf("重建后检索失败: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM posts_fts_indexed`).Scan(&tracked); err != nil {
		t.Fatal(err)
	}
	if hits != 1 || tracked != 1 {
		t.Fatalf("重建应只索引 normal 行: hits=%d tracked=%d", hits, tracked)
	}
}

func TestFTSRebuildRejectsPositionalArgs(t *testing.T) {
	if err := cmdFTSRebuild(t.Context(), []string{"unexpected"}); err == nil {
		t.Error("fts-rebuild 不应接受位置参数")
	}
}
