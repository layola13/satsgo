# 阶段 3：npm 通道

## 立场（已定）

npm 任意库直接转译不现实。分级通道：**A. SA 原生重写（首选）> B. 白名单 TS 转译 > C. 类型-only**。
有 `.d.ts` 无源码的一律按缺实现拒绝，不生成悬空 `@extern` stub。

## 任务

### B 通道：白名单实测（2026-09-29 第一轮，单文件口径）

方法：`npm pack` 取 `lodash-es@4.17.21`（643 非 min `.js`）、`date-fns@2.30.0`
（2093 `index.js`），`Lower` 逐文件扫测（panic 计入拒绝；实测零 panic，
checker 回退在真实 JS 上成立）。

| 库 | pass | refused | 通过率 |
|---|---|---|---|
| lodash-es | 35 | 609 | **5.4%** |
| date-fns | 259 | 1834 | **12.4%** |

拒绝聚类（首 diagnostic）：

- lodash-es：无注解返回值拒（39）、顶层变量（38）、`null` 字面量（15）、
  跨文件 import（数百条，`./_baseXxx.js` 系列为主）。
- date-fns：顶层变量（1020，多为 `require` 互操作）、对象字面量无布局（175）、
  跨文件 import（数百条）。

后续：

4. [x] program 口径重测（LowerProgram 跟随 import 图，拒绝按包聚合）。
   lodash-es `chunk` 子树：22 文件链接、0 未决，默认导入拒清零（含级联）。
   剩余：无注解返回 29（结构性）、顶层 effectful 20、typeof 6、一阶值 5。
   date-fns `addDays` 子树（esm）：4 文件链接，默认链无级联；剩余 `Date`/
   `arguments`/`isNaN`/`instanceof`（无 SA 后端，诚实拒）+ 无注解 9
   （其 d.ts 为空 re-export，配对无签名可用——印证 C 通道需真类型源）。
5. [x] `.d.ts` 配对（签名来自 d.ts，体来自 js）：同目录 `x.d.ts`↔`x.js` 配对，
   返回值/元数/可选即默认覆写 + `lowerFunction` 顶层回退；单测 1 项。
   286 零回退。
6. [ ] 收缩判定：program + 真 d.ts 源（如 date-fns typings 包）后再看通过率；
   当前仍低于收缩线，不触发（见方法论警示）。

**方法论警示（重要）**：以上是单文件口径，系统性低估：

1. 跨文件 import 占拒绝的大头——`LowerProgram` 多文件链接正是为此建的，
   必须重做 **program 口径**实测后再下结论。
2. `.js` 无类型注解触发“返回值需 `-> T`”系统性拒绝——两包都自带 `.d.ts`
   （date-fns 每个函数目录下有 `index.d.ts`），正确设计是 **C+B 配对**：
   `.d.ts` 给签名、`.js` 给体。未做此配对前不判定 B 通道生死。
3. 收缩线（<30% 即收）以 **program + d.ts 配对口径**为准，本轮数字仅为基线。

### package.json → sa.mod 映射

4. [ ] `dependencies` 能对应 SA 包的生成 `require` 行（git ref + sha256 钉死）。
5. [ ] 纯 TS 依赖进 vendor 转译通道；`node:` 内建走 StdProjectionTable。
6. [ ] **Node 内建优先投影到 `sa_plugin_node`**（`node.sai` + 408 符号清单），
   其次 deno（`Deno.*`→`deno.sai`）、bun（`Bun.*`→`bun.sai`），最后才用 `sa_std` 模拟。
   投影表加 Backend 维度；`@import` 指向插件 `.sai`；u32 状态码 + slot-alloc/load 形状
   与现有 fallible-trio 一致；deno/bun 补投影前先索取 exported-symbols 清单。
7. [ ] 转译失败的依赖在 `subset-report.txt` 按包聚合报错（不淹没在文件级 diagnostic 里）。
7. [ ] 拉取/审计不管（`sa pkg` 的事），只生成声明。

### A 通道：SA 原生库（2026-09-29 复用盘点qv）

| 需求 | sa_std | node 插件（408 符号） | 结论 |
|---|---|---|---|
| env/args/cwd | env.sai/process.sai（args/env/cwd/home/tmp） | 同等覆盖 | 直接投影，无需重写 |
| crypto/hash/uuid | 无（仅附带） | hash/hmac/pbkdf2/random_uuid/cipher 全套 | 投影 node 符号，无需重写 |
| http client/server | ws_client/tls_client/http2 碎片 | http client/server/websocket bridge 全套 | 投影 node 符号，无需重写 |
| path/url/querystring | 无 | join/resolve/basename/parse/format 全套 | 投影 node 符号，无需重写 |
| 日期格式化 | time.sai（待查明细） | 无 date 格式化 | **候选重写 #1**（无后端） |
| 校验（zod 子集） | 无 | 无 | 候选重写 #2（排期靠后） |
| lodash 纯函数 | — | — | B 通道转译，不重写 |

立项结论：先把 node 现成面投影完（os/path/crypto/url/http 按 pilot 模式逐批），
`date` 单独立项（SA 原生），校验延后。收缩判定仍待 program + 真 d.ts 口径。

## 交付数字

- 白名单实测通过率（两个标杆库）。
- 映射模块落地 + 聚合报错可用。
- A 通道立项清单（缺口盘点文档）。

## 风险

- B 通道覆盖率若低于 30%，即收缩，只保留聚合报错机制，不再投转译特性。
