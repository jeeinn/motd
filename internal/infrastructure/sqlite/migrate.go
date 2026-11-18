package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration 是一个已解析的迁移单元。
type migration struct {
	Version int64
	Name    string
	SQL     string
}

// Migrate 执行所有尚未应用的迁移。
//
// 设计依据（评审稿 v0.2 §5.4）：
//   - schema_migrations 记录版本，SQL 文件按序号命名、顺序执行、单事务提交；
//   - 数据库版本高于二进制已知版本时拒绝启动（不跨大版本自动降级）。
//
// 这里不需要「向后兼容旧版本」的逻辑：迁移只向前，回退靠备份恢复。
func Migrate(ctx context.Context, db *sql.DB) (applied []int64, err error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	if len(migrations) == 0 {
		return nil, fmt.Errorf("未找到任何迁移文件")
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		return nil, fmt.Errorf("初始化 schema_migrations: %w", err)
	}

	current, err := currentVersion(ctx, db)
	if err != nil {
		return nil, err
	}

	known := migrations[len(migrations)-1].Version
	if current > known {
		return nil, fmt.Errorf("数据库版本 %d 高于当前二进制支持的 %d，拒绝启动（请部署匹配版本或从备份恢复）", current, known)
	}

	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

// applyOne 在单事务内执行一个迁移文件并登记版本。
func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("迁移 %04d 开启事务: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("迁移 %04d（%s）执行失败: %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, unixepoch())`,
		m.Version, m.Name,
	); err != nil {
		return fmt.Errorf("迁移 %04d 登记版本: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("迁移 %04d 提交: %w", m.Version, err)
	}
	return nil
}

func currentVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("读取当前数据库版本: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// loadMigrations 解析 0001_xxx.sql 形式的文件名，按版本升序返回。
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录: %w", err)
	}

	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := migrationFiles.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("读取迁移 %s: %w", e.Name(), err)
		}
		out = append(out, migration{Version: version, Name: name, SQL: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("迁移版本重复: %d", out[i].Version)
		}
	}
	return out, nil
}

func parseMigrationName(fileName string) (int64, string, error) {
	base := strings.TrimSuffix(fileName, ".sql")
	prefix, rest, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("迁移文件名缺少序号前缀: %s（应形如 0001_init.sql）", fileName)
	}
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("迁移文件名序号非法: %s", fileName)
	}
	return version, rest, nil
}
