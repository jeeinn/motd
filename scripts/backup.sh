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

# 备份后立即验收（v0.5.0 / P3-M5）：备份的成功标准是能恢复，至少要先能 verify。
# 解析 motdctl backup 输出的 backup=<path> 行，失败即非零退出，便于 cron/timer 告警。
output="$("$BIN" backup --db "$DB" --dir "$DIR")"
echo "$output"
backup_file="$(printf '%s\n' "$output" | sed -n 's/^backup=//p' | head -1)"
if [[ -z "$backup_file" ]]; then
  echo "备份输出缺少 backup=<path>，无法验收" >&2
  exit 1
fi
"$BIN" verify "$backup_file"
