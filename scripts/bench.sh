#!/usr/bin/env bash
# v0.5.0 / P3-M4：最小压测集合的执行封装。
# 只跑 4 个核心场景，不进 CI 默认 job（workflow_dispatch 或本地发版前手动跑）。
# 用法：scripts/bench.sh [benchtime，默认 3s]
# 说明：给的是回环环境相对延迟基线，不是 1 Mbps / 1000 在线的验收证据。

set -euo pipefail

BENCHTIME="${1:-3s}"
PKGS=(
  ./internal/transport/ssh/       # 场景1 SSH 连接建立/拆除
  ./internal/application/forum/   # 场景2 浏览列表（读多）
  ./internal/infrastructure/sqlite/ # 场景3 写队列背压
  ./internal/presentation/terminal/ # 场景4 并发读+渲染+presence
)
PATTERN='Benchmark(SSHHandshake|ListThreads|WriteQueueBackpressure|ConcurrentRenderWithPresence)'

echo "== motd 最小压测集（benchtime=$BENCHTIME）=="
echo "== 注意：回环环境相对基线，不构成 1 Mbps / 1000 在线验收 =="
go test -run '^$' -bench "$PATTERN" -benchtime="$BENCHTIME" -count=3 "${PKGS[@]}"
