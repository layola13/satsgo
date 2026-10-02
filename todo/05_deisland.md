# 去孤岛台账（satsgo 立足 tsgo，SA 从 JS 管线同一位置发出）

> 方法论见 AGENTS.md「上游优先方法论」「SA 发射位」。规则：先读上游实现，
> 再结合 sci 实际改进；结构沿上游，SA 运行时边自己搭；禁发明调用惯例。

## 已落地

- p1（`1852a57ea` 后续）：`hasModifier` 手写循环（8 处）→
  `ast.HasModifier` 位掩码（`internal/ast/utilities.go:4147`，
  `modifierflags.go:6-15`，declare→Ambient）；`staticLiteralText` 手写解包链 →
  `ast.SkipOuterExpressions` + `OEKAssertions`（`utilities.go:772-828`，
  四 kind 精确对齐，括号保持不透明）。286 sweep 零回退 + 286 `sa check` 全过。

## 审计（待迁，按 ROI 排序）

1. transpile 对齐：单文件 `Lower` 入口形态对 `TranspileModule`
   （`internal/transpile/transpile.go:93`）。单测锁定同构行为。
2. es 变换逐构件采用：`optionalchain` / `nullishcoalescing` /
   `logicalassignment`（与已落 `&&=` 比对语义）/ `objectrestspread` /
   `exponentiation` —— 每个先读实现、单构件接入、286 门禁。
3. async 路线：先读 `estransforms/async.go` 状态机变换 + `forawait.go`，
   再定 SA future 桥（`sci/sa_std/async.sla` 执行器），禁复刻前立项。
4. const 求值：对齐 checker 字面量类型；`evaluator.NewEvaluator`
   由 checker 持有（`checker.go:937`），直接复用需实体解析器，另立项。
5. program 模块解析：对 `internal/module` + `modulespecifiers` 做最小对齐，
   前缀合并/可达集/环检测保持 SA 侧（sci 语义，无上游对应）。
6. 已知语义差（先记不改）：子集字符串真值（切片非空恒真）vs
   上游 `evaluator.IsTruthy`（`""` 为假）；块作用域同名遮蔽扁平寄存器
   （全站既有包络，非 try 切片引入）。

## 不迁（sci 固有）

- 线性所有权 scopes/owned/consumed/released（JS 无此概念）。
- 跨文件前缀链接与命名空间拍扁（SA 模块语义）。
- 大声拒诊（子集诚实性门禁）。
