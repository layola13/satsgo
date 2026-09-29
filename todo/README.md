# tsgo → SA 后续计划（总览）

> 背景：单文件 286/286 demo 已关（commit `56ad4ec8d`），约束不变——**扫测门禁、拒则大声、复用 `sci/sa_std`**。
> 参考实现：`/content/sa_all/sa_plugin_ts`（TS 参考）、`/content/sa_all/sa_plugin_sla`（工程化参考）、
> `/content/sa_all/sa_plugin_pkg`（包管理）、`/content/sa_all/sa_plugin_react`（React 目标端）。

## 阶段与顺序（严格按序，每阶段有可交付的数）

| # | 计划文件 | 目标 | 交付数字 | 信心 |
|---|---|---|---|---|
| 1 | `todo/01_program_link.md` | Program 级多文件链接（import 图 + 合并 `.sai`） | 多文件 demo 项目端到端 `sa build` 通过 | 高 |
| 2 | `todo/02_checker.md` | 接入 binder/checker，替换手写 scope/类型猜测 | 同 286 扫测保持 286/286（零回退）+ 删 `matchLayout` 猜测 | 高 |
| 3 | `todo/03_npm.md` | npm 通道：白名单实测 + `package.json`→`sa.mod` + SA 原生库盘点 | lodash-es/date-fns 通过率数字；映射模块落地 | 机制高/覆盖率待量 |
| 4 | `todo/04_tsx.md` | tsx→SAX（无 hooks 子集）→ hooks/DOM 子集 | counter 级组件 `sa react build` 跑通 | 中高/中 |

## 全局纪律（所有阶段）

1. 新功能必须附带扫测增量（demo 或实测），拒绝数只减不增。
2. 语义拿不准的一律 gate 拒绝，不静默错译（`?.` 可空、`delete` 语义等已有先例）。
3. 新 `sa_std` 符号必须进 `StdProjectionTable` + `tools/check_sa_std_projection.sh` 名单。
4. 每个阶段结束提交推送到 `layola13/satsgo` 一个版本。
