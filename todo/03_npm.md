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
6. [x] 收缩判定（2026-10-02 落地，program + d.ts 配对口径）：标杆 `lodash-es@4.17.21`
    与 `date-fns@2.30.0`（`npm pack` 实测，`LowerProgram` 跟随 import 图）：
    chunk 子树 23 链接 0 通过 / 23 拒绝（0 未决），addDays 子树 2 链接 0 通过 /
    2 拒绝（+11 未决）；残留皆转译体习语（顶层状态、无注解 JS 体、CJS 互操作、
    `arguments`/`isNaN`/方法调用），远低于 30% 收缩线 → **收缩生效**：
    不再投 B 通道转译特性，只保留聚合报错机制（`Unresolved` 按包聚合，本次
    8/11 未决即现网验证）；A/C 通道不受影响。

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

## 附：planck.js 实测（2026-09-30，纯 TS 物理引擎，66 文件，`/content/planck.js`）

单文件口径（`tsgo-sa` 逐文件，`__test__` 除外 60 文件）：**7 通过 / 53 拒绝 /
0 崩溃**（鲁棒性门禁保持：hostile 级真实代码零 panic）。

拒绝聚类（首因计数）：

| # | 首因 | 含义 |
|---|---|---|
| 242 | 顶层变量 | 模块级 `export const`，program 口径 + 顶层折叠可收 |
| 83 | 一阶函数值 | 回调存/传（contact listener 类），Phase 2 已知缺口 |
| 75+14 | 不可赋值目标（含复合） | 待逐个看，多为跨文件类型缺失的连带 |
| 66+66 | `.x`/`.y` 不可访问 | **单文件假象**：`Vec2Value` 是普通 interface，跨文件布局缺失所致，program 口径应消除 |
| 29 | Getter | `get length()` 类，class 子集缺口 |
| 28 | `.TYPE` | 静态枚举式访问，待定 |
| 22 | ModuleDeclaration | `namespace` 块，未立项 |
| 其它 | typeof min/max 别名、`testbed` 重载签名、无布局字面量 | 零散 |

program 口径现状（2026-09-30 更新）：显式 `import type` / `export type`
边已擦除（链接图 + 降级双侧，单测锁定），`Fixture ↔ Body` 值循环告警消除；
现报 `Shape ↔ Distance`——双向皆为 value import 但仅类型位使用
（`set(shape: Shape)` / `proxy: DistanceProxy`，esbuild 口径可擦除）。
backlog：基于用法的擦除（binder 查值位引用，无则消边），独立特性另立项；
真值循环仍保守拒。
- ✅ 基于用法的擦除已落地（2026-09-30）：`import type` 显式擦除 + 值位引用
  分析（纯类型子树不进/绑定名跳过/heritage 保留），链接图与降级双侧；
  planck program 口径环告警清零，收敛到单点
  `util/Timer.ts: export default { now, diff }`（对象字面量默认导出）。
- 下一瓶颈（另立项）：模块命名空间对象（`export default {...}` /
  `import * as planck` + `planck.now()` 成员路由；`main.ts` 即此形）。
  planck 单文件 7/53/0 基线不变（program 口径以链接成功文件计，需重测）。
- ✅ 命名空间对象 p1（2026-09-30）：`export default {a, b: c}` 记录成员→本地
  （方法/spread/空对象大声拒），`import D from` 按成员绑定 qualified，
  `D.m()` 经 `defNSImports` 路由（裸 `D()` 保持大声）；planck program 口径
  从 0 文件推进到 **56 文件链接**，残留顶层变量/typeof 等单文件已知聚类。
  单测 1 项（成功/改名/未知成员拒/方法值拒）；全量单测过。p2（`export default ns`
  透传）与 p3（解构/展开/动态键）另立项。
- ✅ 类型-only 具名符擦除（2026-09-30，`lowerImport` 逐符擦除）：
  `import { Vec2, Vec2Value }` 中零值使用的接口符跳过绑定（显式
  `import { type X }` 恒跳过；`e.link.valueUsed` 为逐文件预扫集；
  误跳过恒以大声 `import it first` 暴露，永不错译）。
  planck program 口径 692→620（-72，全为类型-only 误拒清零，
  零新增；残留 25 经核全为真值缺口：跨文件 const/enum 值导入另立项）。
  单测 1 项（擦除 + 显式 type + 值误绑仍大声）；286 零 diff；
  链接产物真机 check + run 差分一致（7==7）。
- ✅ 方法分派注解接收（2026-09-30，`checkerClassOf` + `reboundNames` +
  实例表静态隔离）：注解形参方法调用由拒转过（`Fixture`/`DistanceInput`
  类形参，跨文件亦可；零映射写入 + 四重回退守卫）。
  planck program 口径 567→538（first-class 135→97；新增皆大声，
  内联体成员真缺口为主）；单测 1 项（7 子项）；286 零 diff；
  真机 check + run 差分一致（40==40）。
- ✅ 跨文件 const 值导入（2026-09-30，`globalConsts`/`modResolution.consts`/
  进口商 const 优先）：`export const` 字标量直折进口商 constVals
  （`let`/模板/对象/桶链恒拒；旧三连拒一次全消）。
  planck program 口径 538→522（-16 全为 EPSILON 簇，零新增）；
  单测 1 项（折叠 + 对象/桶/私有恒拒）；286 零 diff；
  真机 check + run 差分一致（7==7）。
- ✅ 死分支消除（2026-09-30，`isFalseConst` + 四位消除）：
  常量假条件死臂永不 lowering（断言簇整蒸发）。
  planck program 口径 522→477（first-class 97→60，零新增）；
  单测扩展 1 项；286 零 diff；真机 check + run 差分一致（11==11）。
- ✅ 跨文件 enum 值导入（2026-09-30，`enumMemberTable`/`globalEnums`/
  进口商枚举分支）：全整数枚举直填既有表（`const enum` 同形；
  串/计算成员、桶外链恒拒；桶内经共享预播种可达）。
  附带修出单文件负枚举静默错号（`= -1` 按自动计，改编号核共享）
  与 switch 臂释放落终结符后（`releaseScope` 终结守卫）。
  planck program 口径 477→465（-12 全为枚举，零新增）；
  单测 1 项（折叠 + 负号 + 串/桶/私有恒拒）；286 零 diff；
  真机 check + run 差分一致（20==20）。
- ✅ 类型边布局共享（2026-09-30，`typeUsedNames`/`typeSpecOf`/类型闭包/
  共享预扫）：注解位导入不再整文件失联（预扫映射 + checker 集；
  永不融合环/链接/绑定，环单测全绿）。附带修出预扫逐文件
  scratch 致跨文件继承基永不可见（改共享映射）。
  planck 465→465（零新增；注解位点仍被下游真缺口挡住）；
  单测 2 项（布局解析 + 继承 + 环不融合 + 显式边界）；
  286 零 diff；真机 check + run 差分一致（7==7）。
- ⏸ 数组内存模型与索引结构体（调查结论，不 ship 代码，见 AGENTS）：
  `!buf` 构造释放与事后释放皆陷阱（线性释放无力表达 header 拥有
  buffer）；结构体元素指针进 i32 槽读回恒陷阱。需 sci 侧先定模型。
