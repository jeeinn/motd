#!/usr/bin/env bash
# P1-M2 认证限流 / 审计落库本地复现。
#
# 安全约束：只在 mktemp 出的隔离目录里建库，显式给驱动传 -db；
# 绝不读写 X:/motd/data/，也不启动生产进程 / SSH / HTTP 监听。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "隔离目录: $WORK"
echo "项目根:   $ROOT"

cd "$ROOT"
go run ./scripts/repro_auth_limit -db "$WORK/motd.db"
