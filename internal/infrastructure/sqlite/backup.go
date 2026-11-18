// 备份与恢复（评审稿 v0.2 §7 / P0-07）。
//
// 成功标准不是「生成了备份文件」，而是「备份能被恢复」：
//   - VACUUM INTO 产出一致性快照（WAL 下不需要先停服）；
//   - 每个备份带 sha256 校验和 sidecar 文件；
//   - VerifyBackup 同时做校验和 + PRAGMA integrity_check + 关键表行数；
//   - Restore 先验后恢复，且拒绝覆盖已存在的目标库。
//
// 已知限制：VACUUM INTO 会整库重写，大库时 IO 峰值明显。
// 何时改用 SQLite Online Backup API 的评估条件见 docs/OPS.md。
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrChecksumMismatch 表示备份文件与 sidecar 校验和不一致。
	ErrChecksumMismatch = errors.New("备份校验和不匹配")
	// ErrIntegrityFailed 表示 PRAGMA integrity_check 未返回 ok。
	ErrIntegrityFailed = errors.New("备份完整性检查失败")
	// ErrTargetExists 表示恢复目标已存在，拒绝覆盖。
	ErrTargetExists = errors.New("恢复目标已存在，拒绝覆盖")
	// ErrSidecarMissing 表示校验和文件缺失。
	ErrSidecarMissing = errors.New("备份校验和文件缺失")
)

// criticalTables 是恢复验收要核对行数的表。
// 不列全部表：能证明「账号、内容、迁移版本」三类核心数据到位即可。
var criticalTables = []string{"users", "nodes", "threads", "posts", "schema_migrations"}

// BackupInfo 描述一次备份的产物。
type BackupInfo struct {
	Path      string
	Checksum  string
	Size      int64
	CreatedAt time.Time
}

// VerifyResult 是恢复前验收的结果。
type VerifyResult struct {
	Path        string
	Checksum    string
	Size        int64
	Integrity   string
	TableCounts map[string]int64
	SidecarPath string
}

// Backup 在 dir 下生成一份一致性快照与 sha256 sidecar 文件。
// 文件名形如 motd-20261118-113000.db；同一秒内重复调用会自动追加序号。
func Backup(ctx context.Context, p *Pools, dir string) (BackupInfo, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return BackupInfo{}, fmt.Errorf("创建备份目录 %s: %w", dir, err)
	}

	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(dir, "motd-"+stamp+".db")
	for i := 2; fileExists(dst); i++ {
		dst = filepath.Join(dir, fmt.Sprintf("motd-%s-%d.db", stamp, i))
	}

	// VACUUM INTO 的目标必须不存在；路径由程序拼接并做单引号转义。
	escaped := strings.ReplaceAll(dst, "'", "''")
	if _, err := p.writer.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		return BackupInfo{}, fmt.Errorf("生成备份快照: %w", err)
	}

	checksum, size, err := checksumFile(dst)
	if err != nil {
		return BackupInfo{}, err
	}
	sidecar := dst + ".sha256"
	line := fmt.Sprintf("%s  %s\n", checksum, filepath.Base(dst))
	if err := os.WriteFile(sidecar, []byte(line), 0o600); err != nil {
		return BackupInfo{}, fmt.Errorf("写入校验和文件: %w", err)
	}

	return BackupInfo{
		Path:      dst,
		Checksum:  checksum,
		Size:      size,
		CreatedAt: time.Now(),
	}, nil
}

// VerifyBackup 恢复前验收：校验和 → 完整性 → 关键表行数。
// 任一环节失败都返回错误，绝不把可疑文件当作可恢复备份。
func VerifyBackup(ctx context.Context, backupPath string) (VerifyResult, error) {
	result := VerifyResult{Path: backupPath, TableCounts: make(map[string]int64)}

	info, err := os.Stat(backupPath)
	if err != nil {
		return result, fmt.Errorf("备份文件不可读: %w", err)
	}
	result.Size = info.Size()

	sidecar := backupPath + ".sha256"
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		return result, fmt.Errorf("%w: %s", ErrSidecarMissing, sidecar)
	}
	expected := strings.Fields(string(raw))
	if len(expected) == 0 {
		return result, fmt.Errorf("校验和文件为空: %s", sidecar)
	}
	result.SidecarPath = sidecar

	actual, _, err := checksumFile(backupPath)
	if err != nil {
		return result, err
	}
	result.Checksum = actual
	if !strings.EqualFold(actual, expected[0]) {
		return result, fmt.Errorf("%w: 期望 %s，实际 %s", ErrChecksumMismatch, expected[0], actual)
	}

	db, err := sql.Open("sqlite", readOnlyDSN(backupPath))
	if err != nil {
		return result, fmt.Errorf("只读打开备份: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return result, fmt.Errorf("连接备份文件: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result.Integrity); err != nil {
		return result, fmt.Errorf("执行 integrity_check: %w", err)
	}
	if !strings.EqualFold(result.Integrity, "ok") {
		return result, fmt.Errorf("%w: %s", ErrIntegrityFailed, result.Integrity)
	}

	for _, table := range criticalTables {
		var n int64
		// 表名来自固定白名单，不是外部输入。
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			return result, fmt.Errorf("读取表 %s 行数: %w", table, err)
		}
		result.TableCounts[table] = n
	}
	return result, nil
}

// Restore 把已验收的备份恢复到 targetPath。
// 目标必须不存在（拒绝覆盖），恢复后再次做 integrity_check。
func Restore(ctx context.Context, backupPath, targetPath string) error {
	if _, err := VerifyBackup(ctx, backupPath); err != nil {
		return fmt.Errorf("恢复前验收未通过，已中止: %w", err)
	}
	if fileExists(targetPath) {
		return fmt.Errorf("%w: %s", ErrTargetExists, targetPath)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return fmt.Errorf("创建恢复目录: %w", err)
	}

	// 残留的 WAL / SHM 会让恢复出的库读到旧事务，必须一并清掉。
	// 只删除精确的三个路径，不用通配符。
	for _, suffix := range []string{"-wal", "-shm"} {
		stale := targetPath + suffix
		if fileExists(stale) {
			if err := os.Remove(stale); err != nil {
				return fmt.Errorf("清理残留文件 %s: %w", stale, err)
			}
		}
	}

	if err := copyFile(backupPath, targetPath); err != nil {
		return err
	}

	// 恢复后的库必须能自检通过（sqlite.Open 会验证 PRAGMA 基线）。
	pools, err := Open(ctx, targetPath)
	if err != nil {
		return fmt.Errorf("恢复后的库无法启动: %w", err)
	}
	defer func() { _ = pools.Close() }()

	var integrity string
	if err := pools.Ping(ctx); err != nil {
		return fmt.Errorf("恢复后的库不可读: %w", err)
	}
	if err := pools.reader.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("恢复后 integrity_check: %w", err)
	}
	if !strings.EqualFold(integrity, "ok") {
		return fmt.Errorf("%w（恢复后）: %s", ErrIntegrityFailed, integrity)
	}
	return nil
}

func checksumFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("打开 %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("计算校验和: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开备份: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("创建恢复目标: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("复制备份: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("关闭恢复目标: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
