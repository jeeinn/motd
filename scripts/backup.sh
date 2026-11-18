#!/usr/bin/env bash
# 每日备份入口，供 cron / systemd timer 调用。
# 只生成备份与校验和，不自动删除任何文件；保留策略见 docs/OPS.md。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${MOTDCTL_BIN:-$ROOT/bin/motdctl}"
DB="${MOTD_DB_PATH:-$ROOT/data/motd.db}"
DIR="${MOTD_BACKUP_DIR:-$ROOT/data/backups}"

if [[ ! -x "$BIN" ]]; then
  echo "未找到可执行的 motdctl: $BIN" >&2
  echo "先执行: go build -o bin/motdctl ./cmd/motdctl" >&2
  exit 1
fi

"$BIN" backup --db "$DB" --dir "$DIR"
