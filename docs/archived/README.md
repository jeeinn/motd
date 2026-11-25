# docs/archived — 归档文档

> **归档 = 已完成使命，只在追溯时读。** 这里的文件不再回改；新结论一律进 `docs/DECISIONS.md`（决策）或 `docs/ROADMAP.md`（待办）。
> 归档时已把**未完成项迁出**到 `docs/ROADMAP.md`，所以归档不等于丢信息。

## 归档规则

1. 功能已发版，且结论已固化进 ADR → 设计稿归档。
2. 阶段目标已全部交付 → 阶段计划归档。
3. 被更新文档取代（如原始评审稿被 `DEVIATIONS.md` + `MVP-DELIVERY.md` + ADR 取代）→ 归档。

## 路径变更说明（重要）

这些文件原位于 `docs/` 根目录，现移入 `docs/archived/`。
**历史上的 ADR / 计划以 `docs/xxx.md` 行内代码形式引用旧路径，未回改**——`DECISIONS.md` 是历史原文，为一次路径调整去改 100 多处引用会污染 ADR 历史。看到旧路径时，请到本目录找同名文件。

只有 3 处真正的 markdown 链接（会 404 的那种）已修正，都在 `RELEASE_NOTES.md` 的历史版本说明里。

## 一、设计稿（功能已发版）

| 文件 | 对应版本 | 落地 ADR | 归档原因 / 遗留项 |
| --- | --- | --- | --- |
| `DESIGN-whoami-who.md` | v0.8.0 | ADR-065 | `/whoami` `/who` 已发版，`/karma` 退役 |
| `DESIGN-back-focus.md` | v0.8.1 | ADR-066 | `q` 返回列表按行 ID 回填焦点已发版 |
| `DESIGN-composer-draft.md` | v0.8.2 | ADR-067 | 提交失败保留会话草稿已发版；**二期 backlog 已迁 ROADMAP R-07** |
| `DESIGN-hint-reply-focus.md` | v0.8.4 | ADR-068 | hint 灰阶 + 回复后焦点锚定已发版；**O3 详情屏分页未做 → ROADMAP R-06** |
| `DESIGN-v090-display-i18n.md` | v0.9.0 | ADR-069 / ADR-070 | 物理行模型 + i18n 基建已发版；**全量文案翻译 → ROADMAP R-08** |
| `DESIGN-clean-redraw.md` | v0.9.0 | ADR-071 / ADR-072 | 全屏干净重绘已发版并部署；**resize 即时对齐 → ROADMAP R-09** |

## 二、阶段计划（目标已全部交付）

| 文件 | 覆盖阶段 | 完成标志 |
| --- | --- | --- |
| `P0-PLAN.md` | P0 MVP 核心论坛闭环 | MVP 交付，复盘见 `../MVP-DELIVERY.md` |
| `PHASE1-PLAN.md` | P1 内核安全与数据完整性 | v0.1.0 之后的安全/完整性收口 |
| `PHASE2-PLAN.md` | v0.2.1 / v0.3.0 版本节奏与体验 | 编辑策略与邮箱认证落地 |
| `P2-PLAN.md` | P2 社区治理 | v0.4.0（迁移 0007→0009，ADR 048→053） |
| `P3-PLAN.md` | P3 稳定运营 + 占位缺口收口 | v0.5.0 / v0.6.0（ADR-056~062） |
| `P4-PLAN.md` | P4 可靠性收尾 + 基础多色 | v0.7.0（ADR-063/064 同批） |
| `P4.5-PLAN.md` | P4.5 运营可见性（`/latest` + `/stats`） | v0.7.1（PR #2） |

## 三、原始设计稿

| 文件 | 说明 |
| --- | --- |
| `命令行论坛整体设计方案-评审稿v0.2.md` | 立项基线（2026-11-18 导出）。**不回改**：对它的所有偏差登记在 `../DEVIATIONS.md`（R1~R22），交付判定见 `../MVP-DELIVERY.md` |

## 四、仍然有效、不要归档的文档

| 文件 | 为什么留在 `docs/` |
| --- | --- |
| `../DECISIONS.md` | ADR-001~072，决策单一事实源，持续追加 |
| `../OPS.md` | 运维手册，随部署实战持续更新（§9/§10） |
| `../ROADMAP.md` | 本目录各文档的"未完成项"都汇到这里 |
| `../DEVIATIONS.md` | 活文档，偏差可持续追加（最新 R22） |
| `../MVP-DELIVERY.md` | 理解项目起点与历史判定的唯一入口（D1~D7、A1~A8） |
| `../eggs.md` | 彩蛋清单，随彩蛋增减更新 |
| `../images/` | README 引用的截图 |
