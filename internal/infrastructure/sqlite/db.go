// Package sqlite 负责 SQLite 的连接、PRAGMA 基线与读写连接池切分。
//
// 设计依据（评审稿 v0.2 §2.6）：
//   - 写连接恒为 1，读连接按 CPU 核数控制；
//   - 每个连接都必须生效 foreign_keys / busy_timeout 等 PRAGMA，
//     因此通过 DSN 的 _pragma 参数在连接建立时注入，而不是只执行一次；
//   - 高频状态（在线人数、心跳）不进这个池，走内存注册表。
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无 CGO
)

// Pools 把同一数据库文件拆成两个用途明确的句柄。
// 调用方不需要知道底层是同一文件，但必须知道「写一律走 Writer，读一律走 Reader」。
type Pools struct {
	Writer *sql.DB
	Reader *sql.DB
}

// pragmas 是需要逐连接生效的会话级设置。
// journal_mode 是库级持久设置，仍显式声明，便于新库首次初始化时立即进入 WAL。
var pragmas = []string{
	"journal_mode(WAL)",
	"foreign_keys(1)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
	"cache_size(-16000)", // 约 16 MiB，对应 §2.7 资源账本
}

// Open 打开数据库并完成连接层面的自检。
func Open(ctx context.Context, path string) (*Pools, error) {
	writer, err := openPool(path, 1, "_txlock=immediate", "writer")
	if err != nil {
		return nil, err
	}

	readers := runtime.NumCPU()
	if readers < 2 {
		readers = 2
	}
	reader, err := openPool(path, readers, "", "reader")
	if err != nil {
		_ = writer.Close()
		return nil, err
	}

	pools := &Pools{Writer: writer, Reader: reader}
	if err := pools.verify(ctx); err != nil {
		_ = pools.Close()
		return nil, err
	}
	return pools, nil
}

func openPool(path string, maxConns int, extra string, role string) (*sql.DB, error) {
	dsn := buildDSN(path, extra)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite（%s）: %w", role, err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0) // SQLite 本地文件，不因时间主动回收连接

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 SQLite（%s）: %w", role, err)
	}
	return db, nil
}

// buildDSN 生成带 _pragma 的 file DSN。
// Windows 路径统一转成斜杠形式，并做 URL 编码，避免空格与中文目录串味。
func buildDSN(path string, extra string) string {
	slash := filepath.ToSlash(path)
	if !strings.HasPrefix(slash, "/") {
		// 相对路径与 Windows 盘符路径都按普通路径编码；盘符中的冒号需保留，
		// url.PathEscape 会破坏它，因此只转义确实危险的字符。
		slash = strings.ReplaceAll(slash, " ", "%20")
		slash = strings.ReplaceAll(slash, "#", "%23")
		slash = strings.ReplaceAll(slash, "?", "%3F")
	} else {
		slash = url.PathEscape(slash)
		slash = strings.ReplaceAll(slash, "%3A", ":")
	}

	params := make([]string, 0, len(pragmas)+1)
	for _, p := range pragmas {
		params = append(params, "_pragma="+p)
	}
	if extra != "" {
		params = append(params, extra)
	}
	return "file:" + slash + "?" + strings.Join(params, "&")
}

// verify 在启动阶段确认 PRAGMA 真正落地。
// 只信任查询结果，不信任 DSN 语法正确性——错拼的 PRAGMA 不会报错，只会静默失效。
func (p *Pools) verify(ctx context.Context) error {
	var foreignKeys int
	if err := p.Writer.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("读取 PRAGMA foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("PRAGMA foreign_keys 未生效（当前值 %d），拒绝启动", foreignKeys)
	}

	var journalMode string
	if err := p.Writer.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("读取 PRAGMA journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("journal_mode 未进入 WAL（当前值 %q），拒绝启动", journalMode)
	}
	return nil
}

// Close 关闭两个池。
func (p *Pools) Close() error {
	var firstErr error
	if p.Reader != nil {
		if err := p.Reader.Close(); err != nil {
			firstErr = err
		}
	}
	if p.Writer != nil {
		if err := p.Writer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
