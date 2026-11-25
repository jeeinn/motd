#!/usr/bin/env python3
"""从 CHANGELOG.md 提取指定版本的段落，作为 GitHub Release 正文。

用法:
    python3 scripts/extract_release_notes.py <version> <changelog> <outfile>

    version   : 版本号，可带 v 前缀（v0.10.0 与 0.10.0 均可）
    changelog : CHANGELOG.md 路径
    outfile   : 输出文件路径（写入纯 Markdown 正文）

退出码:
    0  成功提取并写出
    1  参数错误 / 文件缺失 / 版本段不存在（CI 据此 fail-fast，不发空说明）

段落边界约定（Keep a Changelog 格式）:
    起: '## [<version>]' 行（方括号内精确匹配版本号，忽略 v 前缀）
    止: 下一个 '## [' 开头的行（含 Unreleased 与更旧版本）
    '## [Unreleased]' 永不作为目标版本输出（防止把未发布内容当发布说明）

设计取舍:
    - 只做文本切分，不解析 Markdown AST：CHANGELOG 格式由本项目约定锁定，
      段落标题形态稳定；拒绝模糊匹配（如 '0.10' 命中 '0.10.0'）。
    - 版本精确匹配：'## [0.10.0]' 只在方括号内完全一致时命中。
    - 不改动 CHANGELOG.md 原文件，只读。
"""

import re
import sys
from pathlib import Path


def normalize_version(raw: str) -> str:
    v = raw.strip()
    if v.startswith("v") or v.startswith("V"):
        v = v[1:]
    return v


def extract(changelog_text: str, version: str) -> str | None:
    """返回该版本段落的正文（含标题行），找不到返回 None。"""
    lines = changelog_text.splitlines(keepends=True)
    start = None
    heading_re = re.compile(r"^##\s+\[([^\]]+)\]")
    for i, line in enumerate(lines):
        m = heading_re.match(line)
        if not m:
            continue
        found = normalize_version(m.group(1))
        if found == version:
            # 显式拒绝 Unreleased（即便有人传 --version Unreleased）
            if found.lower() == "unreleased":
                return None
            start = i
            break

    if start is None:
        return None

    end = len(lines)
    for j in range(start + 1, len(lines)):
        if lines[j].startswith("## ["):
            end = j
            break

    body = "".join(lines[start:end]).strip("\n")
    return body + "\n" if body else None


def main() -> int:
    if len(sys.argv) != 4:
        print(
            "用法: extract_release_notes.py <version> <changelog> <outfile>",
            file=sys.stderr,
        )
        return 1

    version = normalize_version(sys.argv[1])
    changelog = Path(sys.argv[2])
    outfile = Path(sys.argv[3])

    if not version:
        print("错误: 版本号为空", file=sys.stderr)
        return 1
    if not changelog.is_file():
        print(f"错误: CHANGELOG 文件不存在: {changelog}", file=sys.stderr)
        return 1

    text = changelog.read_text(encoding="utf-8")
    body = extract(text, version)
    if body is None:
        print(
            f"错误: CHANGELOG.md 中未找到版本 [{version}] 的段落，"
            "拒绝生成空的 Release 说明（请先在 CHANGELOG.md 登记该版本）",
            file=sys.stderr,
        )
        return 1

    outfile.parent.mkdir(parents=True, exist_ok=True)
    outfile.write_text(body, encoding="utf-8")
    print(f"已提取 [{version}] 段落 -> {outfile} ({len(body)} 字符)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
