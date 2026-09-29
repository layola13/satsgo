# 阶段 1：Program 级多文件链接

## 目标

`tsgo-sa` 从单文件 CLI 升级为项目构建：读 `tsconfig.json` + 跟随 import 图，
逐文件 lower，合并输出一个可 `sa build` 的 `.sai` 工程。

## 对照参考

- `sa_plugin_sla/src/plugin_imports.zig`（import 解析/展开）
- `sa_plugin_sla/src/plugin_module_table.zig`（模块表）
- `sa_plugin_sla/src/plugin_reachability.zig`（可达剪枝）
- `sa_plugin_sla/src/workspace.zig`（工作区）
- tsgo 侧优势：binder/checker 自带模块解析，**不用手写**，只换数据源。

## 任务

1. [ ] `LowerProgram(program, entry)` 入口：以 entry 为根，用 tsgo module resolution 跟随 `import`（含 `paths`/alias、`node_modules`）。
2. [ ] 逐文件 `Lower`，符号加包名前缀（防多文件 `@main`/同名函数冲突）。
3. [ ] 循环 import：拒绝（loud）。
4. [ ] 跨文件调用解析：函数/类符号跨文件可见（现在 `funcSigs` 是单文件预扫）。
5. [ ] 项目级 `@import` 去重 + 校验（现在逐文件 emit，需合并）。
6. [ ] `tsgo-sa build [dir]`：读 `package.json` + `tsconfig.json`，复用现有 `Scaffold` 布局输出。
7. [ ] 增量：按文件内容 hash 缓存 `.sai`；`subset-report.txt` 升级为门禁（`refused=true` 即 CI 失败）。

## 交付数字

- 多文件 demo 项目（main + 2~3 个本地模块，含跨文件函数/类/interface 调用）端到端 `sa build` 通过。
- 286 单文件扫测保持 286/286（零回退）。

## 风险

- 跨文件重名符号的前缀方案需与 `sa` 汇编符号规则对齐（先查 sci 文档再定）。
