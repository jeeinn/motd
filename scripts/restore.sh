#!/usr/bin/env bash
# 恢复演练 / 真实恢复入口。
# 目标路径必须不存在；拒绝覆盖是刻意设计，防止误把备份恢复到正在运行的库上。
#
# 用法:
#   scripts/restore.sh <backup-file> <target-path>
#   scripts/restore.sh --verify <backup-file>       # 只校验，不恢复
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${MOTDCTL_BIN:-$ROOT/bin/motdctl}"

if [[ ! -x "$BIN" ]]; then
  echo "未找到可执行的 motdctl: $BIN" >&2
  exit 1
fi

if [[ "${1:-}" == "--verify" ]]; then
  [[ $# -eq 2 ]] || { echo "用法: $0 --verify <backup-file>" >&2; exit 2; }
  exec "$BIN" verify "$2"
fi

[[ $# -eq 2 ]] || { echo "用法: $0 <backup-file> <target-path>" >&2; exit 2; }

BACKUP="$1"
TARGET="$2"

if [[ -e "$TARGET" ]]; then
  echo "目标已存在，拒绝覆盖: $TARGET" >&2
  echo "请先把现有库移走（保留现场），或使用另一个空目录。" >&2
  exit 1
fi

echo "恢复前验收:"
"$BIN" verify "$BACKUP"
echo
echo "执行恢复:"
"$BIN" restore "$BACKUP" --target "$TARGET"
echo
echo "恢复完成。请用 /healthz 与关键数据抽查确认后再对外提供服务。"
