package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P0-07 验收（对应 P0-PLAN D7）：备份的成功标准是能恢复，不是生成了文件。

func seedForBackup(t *testing.T, pools *Pools, n int) *Writer {
	t.Helper()
	if _, err := Migrate(context.Background(), pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)
	for i := 0; i < n; i++ {
		if err := w.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			return insertNodeTx(ctx, tx, fmt.Sprintf("seed-%02d", i))
		}); err != nil {
			t.Fatalf("写入种子数据失败: %v", err)
		}
	}
	return w
}

func TestBackupProducesRestorableSnapshot(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	seedForBackup(t, pools, 3)

	backupDir := filepath.Join(t.TempDir(), "backups")
	info, err := Backup(ctx, pools, backupDir)
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}

	// 备份之后继续写入，用来证明恢复点确实是备份那一刻的状态。
	w := pools.NewWriter()
	w.Start()
	defer w.Stop()
	for i := 3; i < 5; i++ {
		if err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
			return insertNodeTx(ctx, tx, fmt.Sprintf("after-%02d", i))
		}); err != nil {
			t.Fatalf("备份后写入失败: %v", err)
		}
	}
	if got := countNodes(ctx, pools); got != 5 {
		t.Fatalf("备份后应有 5 行，实际 %d", got)
	}

	result, err := VerifyBackup(ctx, info.Path)
	if err != nil {
		t.Fatalf("备份验收失败: %v", err)
	}
	if result.Integrity != "ok" {
		t.Fatalf("integrity_check 应为 ok，实际 %q", result.Integrity)
	}
	if got := result.TableCounts["nodes"]; got != 3 {
		t.Fatalf("备份中应有 3 个 nodes，实际 %d", got)
	}
	if got := result.TableCounts["schema_migrations"]; got != 1 {
		t.Fatalf("迁移版本应随备份保存，实际 %d", got)
	}

	restoredDir := filepath.Join(t.TempDir(), "restored")
	restoredPath := filepath.Join(restoredDir, "motd.db")
	if err := Restore(ctx, info.Path, restoredPath); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}

	restored, err := Open(ctx, restoredPath)
	if err != nil {
		t.Fatalf("恢复后的库无法启动: %v", err)
	}
	defer func() { _ = restored.Close() }()

	if got := countNodes(ctx, restored); got != 3 {
		t.Fatalf("恢复后应为备份点的 3 行，实际 %d", got)
	}
	applied, err := Migrate(ctx, restored)
	if err != nil {
		t.Fatalf("恢复后迁移检查失败: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("恢复后不应再应用迁移，实际 %v", applied)
	}
}

// TestRestoreToEmptyDirectory 直接对应 D7：恢复到空目录后服务能启动、能写。
func TestRestoreToEmptyDirectory(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	seedForBackup(t, pools, 2)

	info, err := Backup(ctx, pools, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	emptyDir := filepath.Join(t.TempDir(), "empty")
	target := filepath.Join(emptyDir, "motd.db")
	if err := Restore(ctx, info.Path, target); err != nil {
		t.Fatalf("恢复到空目录失败: %v", err)
	}

	db, err := Open(ctx, target)
	if err != nil {
		t.Fatalf("恢复后的库启动失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	w := db.NewWriter()
	w.Start()
	defer w.Stop()
	if err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return insertNodeTx(ctx, tx, "post-restore")
	}); err != nil {
		t.Fatalf("恢复后的库不可写: %v", err)
	}
	if got := countNodes(ctx, db); got != 3 {
		t.Fatalf("恢复后写入一行应有 3 行，实际 %d", got)
	}
}

func TestBackupChecksumDetectsTampering(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	seedForBackup(t, pools, 1)

	info, err := Backup(ctx, pools, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(info.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("tampered")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if _, err := VerifyBackup(ctx, info.Path); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("篡改后应返回 ErrChecksumMismatch，实际 %v", err)
	}
	if err := Restore(ctx, info.Path, filepath.Join(t.TempDir(), "x.db")); err == nil {
		t.Fatal("篡改过的备份不应被恢复")
	}
}

func TestBackupSidecarMissing(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	seedForBackup(t, pools, 1)

	info, err := Backup(ctx, pools, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(info.Path + ".sha256"); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(ctx, info.Path); !errors.Is(err, ErrSidecarMissing) {
		t.Fatalf("缺 sidecar 应返回 ErrSidecarMissing，实际 %v", err)
	}
}

func TestRestoreRefusesExistingTarget(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	seedForBackup(t, pools, 1)

	info, err := Backup(ctx, pools, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "motd.db")
	if err := os.WriteFile(target, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, info.Path, target); !errors.Is(err, ErrTargetExists) {
		t.Fatalf("覆盖已有库应被拒绝，实际 %v", err)
	}
	// 原文件必须原封不动。
	raw, _ := os.ReadFile(target)
	if string(raw) != "occupied" {
		t.Fatal("拒绝覆盖时不应改动目标文件")
	}
}

func TestBackupFileNamingAndSidecar(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	seedForBackup(t, pools, 1)

	dir := t.TempDir()
	first, err := Backup(ctx, pools, dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Backup(ctx, pools, dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path {
		t.Fatal("同一秒内连续备份不应覆盖已有文件")
	}
	for _, info := range []BackupInfo{first, second} {
		if !strings.HasPrefix(filepath.Base(info.Path), "motd-") || !strings.HasSuffix(info.Path, ".db") {
			t.Fatalf("备份文件名不符合约定: %s", info.Path)
		}
		if len(info.Checksum) != 64 {
			t.Fatalf("sha256 长度应为 64，实际 %d", len(info.Checksum))
		}
		if !fileExists(info.Path + ".sha256") {
			t.Fatalf("缺少 sidecar: %s", info.Path)
		}
	}
}
