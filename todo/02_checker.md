# 阶段 2：接入 binder/checker（把前端杠杆吃满）

## 背景

demo 阶段为速度手写了 scope/类型猜测（`scopes` map、`annotationType`、
`matchLayout`、`?.` 降级、`Box<T>` 调用点布局继承 hack）。到 npm 阶段这些会反噬，
必须换成 tsgo 真类型。

## 任务

1. [x] 入口换 `LowerProgram`（typeCtx：NoLib 内存 Program，单文件/Program 共享）：Program 建一次，binder/checker 全程在线。
2. [~] `layoutOfVar`/`matchLayout` 改查 `checker.getTypeAtLocation`，删除注解猜测。
   v1（`checker_layout.go`）：基址经 `layoutOfNode`（记录表 → checker 名 →
   匿名形字段集）；注解猜测保留；planck program 口径 diag 686→649。
   v2：`type X={...}` 建布局 + 嵌套 ftypes 下沉；diag 649→617。
3. [x] 泛型单态化类型驱动：`Box<T>` 不再靠调用点布局继承（`monoKey` +
   `instantiateLayout` + 注解/联合接入；未知模板回退；递归经 shell 预占收敛，
   含参泛事实参/深层嵌套仍 raw）。
   286 逐字节一致；单测 2 项。
4. [x] `?.` 真守卫：`strictNullChecks` 可空信息决定是否加 join-slot（非空保持直调）。
5. [x] 捕获分析换 binder locals（`collectValueIdents` 手写 walk 已删，
   改走共享 `valueUsedNames`；286 输出逐字节一致 + 新单测锁定类型名不进捕获）。
6. [~] 拒绝条件从“语法 Kind”升级为“checker 类型”（精准杀，提升白名单通过率）。
   首刀：`typeof v === "undefined"` 比较级守卫（`eq/ne v, 0`，planck 617→613）；
   第二刀（2026-09-30 落地）：`typeof X === "<kind>"` 比较级常量折叠
   （`lowerTypeofConstFold`：双向/否定/字面量，静态已知即 `eq/ne 1, 1`，
   未知回落既有大声；单测 1 项；286 零 diff；真机 check + run 差分一致）。
   第三刀（2026-09-30 落地）：注解兜底 `scalarTypeofKind` + `kindVars`
   （`trackBinding` 末位记录，`lowerTypeof`/`lowerTypeofConstFold` 双侧；
   方言标量 `i32/i64/u64/f64/boolean/bigint` 即使 checker 盲也折叠，
   无注解/any 仍大声；单测 1 项；286 零 diff；真机 check + run 差分一致）。
   v3（2026-09-30 落地）：`layoutOfCheckerName` 抽取（`layoutOfNode`/
   `layoutOfLiteral` 共享，精确名恒胜字段集猜测）+ spread 源走
   `layoutOfNode`（工厂返回/推断 const 此前无记录即拒，现经 checker 命名；
   `any` 源仍大声；外层目标键集匹配不动，错配仍拒）。
   单测 1 项（双翻转 + 速记注解 + any 拒）；286 零 diff；
   真机 check + run 差分一致（120==120）。
   元数/类型拒绝已按需加（既有）。
7. [x] 自建 scope 评估收尾（2026-09-30 全站审计结论：删无可删，永久保留）。
   首刀：`declaredAt` 可见性 helper + typeof 尾部分支；单测 2 项。
   全站 26 处 `lookupBinding` + push/pop/ownership 约 150 触点逐项分类：
   - 所有权/别名（assign/release/alias/heap/consumed/released、箭头捕获、
     `thisSelf`、temps）：binder 无所有权概念，不可删。
   - 本地遮蔽路由（模块槽/命名空间/布局/自增与复合赋值的 `lookupBinding == nil`
     守卫）：emitter 栈答的是 lowering-time 在场（含合成绑定），binder 答
     的是源码在场，两者是不同问题，迁移即错，不可删。
   - 全局可见性：早已 binder 化（`declaredAt` + `linkRoute` 識别跨文件成员），
     无残留。
   边界锁：`TestLowerProgramLocalShadowsImport`（局部参遮蔽同名导入，
   binder 见导入而栈见局部，局部必须干净胜出）；286 零 diff。

## 交付数字

- 286 扫测保持 286/286（零回退是硬门槛，任何一退即停）。
- `?.` 可空用例新增 demo（空/非空各一）并通过。

## 风险

- Program 搭建成本（host、lib.d.ts、diagnostics 过滤）是前期一次性投入，
  先用最小 host 跑通再补全。
