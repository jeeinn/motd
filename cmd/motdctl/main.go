// Command motdctl 是运维命令行工具：备份、校验、恢复。
//
// 单独成二进制而不是依赖 sqlite3 CLI：部署形态是「一个 Go 二进制 + 一个 SQLite 文件」，
// 运维机上不一定装 sqlite3，且恢复流程需要在同一套 PRAGMA 基线下自检。
//
// 用法：
//
//	motdctl backup  [--db <path>] [--dir <dir>]
//	motdctl verify  <backup-file>
//	motdctl restore <backup-file> --target <path>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "backup":
		err = cmdBackup(ctx, os.Args[2:])
	case "verify":
		err = cmdVerify(ctx, os.Args[2:])
	case "restore":
		err = cmdRestore(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法:
  motdctl backup  [--db <path>] [--dir <dir>]
  motdctl verify  <backup-file>
  motdctl restore <backup-file> --target <path>

环境变量:
  MOTD_DB_PATH      默认 ./data/motd.db
  MOTD_BACKUP_DIR   默认 ./data/backups`)
}

func cmdBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dbPath := fs.String("db", env("MOTD_DB_PATH", "./data/motd.db"), "源数据库路径")
	dir := fs.String("dir", env("MOTD_BACKUP_DIR", "./data/backups"), "备份目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("backup 不接受位置参数")
	}
	if _, err := os.Stat(*dbPath); err != nil {
		return fmt.Errorf("源数据库不存在或不可读: %w", err)
	}

	pools, err := sqlite.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = pools.Close() }()

	info, err := sqlite.Backup(ctx, pools, *dir)
	if err != nil {
		return err
	}
	// 统一输出斜杠路径，便于脚本跨平台解析（Windows 版 filepath.Join 会给出反斜杠）。
	fmt.Printf("backup=%s\nsha256=%s\nsize=%d\n", filepath.ToSlash(info.Path), info.Checksum, info.Size)
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("用法: motdctl verify <backup-file>")
	}

	result, err := sqlite.VerifyBackup(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("backup=%s\nsha256=%s\nsize=%d\nintegrity=%s\n",
		result.Path, result.Checksum, result.Size, result.Integrity)

	names := make([]string, 0, len(result.TableCounts))
	for name := range result.TableCounts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("table %-20s %d rows\n", name, result.TableCounts[name])
	}
	return nil
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	target := fs.String("target", "", "恢复目标路径（必须不存在）")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *target == "" {
		return fmt.Errorf("用法: motdctl restore <backup-file> --target <path>")
	}
	if err := sqlite.Restore(ctx, fs.Arg(0), *target); err != nil {
		return err
	}
	fmt.Printf("restored=%s\n", *target)
	return nil
}

// normalizeFlags 把 --target 及其值提到参数最前面。
// flag 包遇到第一个位置参数就停止解析，因此 `restore <file> --target <path>`
// 这种更符合直觉的顺序需要归一化，否则 --target 会被当成位置参数吞掉。
func normalizeFlags(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--target" && i+1 < len(args):
			flags = append(flags, args[i], args[i+1])
			i++
		case strings.HasPrefix(args[i], "--target="):
			flags = append(flags, args[i])
		default:
			rest = append(rest, args[i])
		}
	}
	return append(flags, rest...)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
