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
3. [ ] 泛型单态化类型驱动（deferred：句柄模型下调用点继承已够用）：`Box<T>` 不再靠调用点布局继承。
4. [x] `?.` 真守卫：`strictNullChecks` 可空信息决定是否加 join-slot（非空保持直调）。
5. [x] 捕获分析换 binder locals（`collectValueIdents` 手写 walk 已删，
   改走共享 `valueUsedNames`；286 输出逐字节一致 + 新单测锁定类型名不进捕获）。
6. [~] 拒绝条件从“语法 Kind”升级为“checker 类型”（精准杀，提升白名单通过率）。
   首刀：`typeof v === "undefined"` 比较级守卫（`eq/ne v, 0`，planck 617→613）；
   其余形态（已知 kind 常量折叠等）另立项。元数/类型拒绝已按需加（既有）。
7. [ ] 自建 scope 逐步删除（deferred），作用域以 binder 为准。

## 交付数字

- 286 扫测保持 286/286（零回退是硬门槛，任何一退即停）。
- `?.` 可空用例新增 demo（空/非空各一）并通过。

## 风险

- Program 搭建成本（host、lib.d.ts、diagnostics 过滤）是前期一次性投入，
  先用最小 host 跑通再补全。
