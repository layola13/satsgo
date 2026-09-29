# tsgo → SA 设计文档 (tsgo → ts → sa)

## 1. 背景:三件套定位

| 组件 | 位置 | 角色 |
|---|---|---|
| sala | `/content/sa_all/sala` (sahelp) | SA/SLA 帮助文档站 (CHM 风格 HTML):语言参考、SA 汇编、CLI、插件、标准库.知识库,不参与工具链 |
| sci | `/content/sa_all/sci` | SA 核心编译器 (Zig):`.sa/.sai/.sal → Flattener → Referee → LLVM-C/WASM`,自带 `sa_std` 标准库与 `sa` CLI.后端消费者 |
| sa_plugin_ts | `/content/sa_all/sa_plugin_ts` | 既有 TS 前端插件 (Zig `.so`):手写 SIMD lexer + Pratt parser + 线性 Lowerer,输出 SA-ASM.279 个 demos 定义其已验证子集 |

问题:`sa_plugin_ts` 的手写 parser 只覆盖 TS 子集,无类型检查;而
`typescript-go` (TS 7 原生 Go 实现)拥有生产级 parser/binder/checker.
改造目标:以 tsgo 为前端,生成 sci/sa 可直接装配的工程.

## 2. 管线:tsgo → ts → sa

```
.ts (全量 TypeScript)
  │  tsgo parser (生产级, exact errors)
  ▼
┌─────────────┐
│ subset gate │  "ts" 中间件:仅 SA 可降级子集通过;
│ ("ts" step) │  其余按定位拒绝,永不静默错码
└─────────────┘
  │  internal/saemit
  ▼
.sai (SA-ASM) + sa.mod + src/main.ts + subset-report.txt + build.sh
  │  sci `sa build` / `sa build-exe`
  ▼
.exe / .wasm
```

新增代码 (本仓库内,未动上游):

- `internal/saemit/saemit.go` — 解析 + 子集门 + SA-ASM 发射器
- `internal/saemit/stdlib.go` — `StdProjectionTable`:TS 标准库 → `sci/sa_std` 契约映射
- `internal/saemit/project.go` — sci/sa 工程脚手架 (`sa.mod` + `src/` + `build.sh`)
- `cmd/tsgo-sa/main.go` — CLI:`tsgo-sa [--out dir] [--mod name] file.ts...`
- `internal/saemit/saemit_test.go` — 单测 (Lower + Scaffold + 投影表形状)
- `tools/check_sai_shape.py` — .sai 结构校验器 (无 `jz`,双目标 `br`,显式 offset,标签闭合,终结符纪律)

## 3. 发射规则 (对标 `sa_plugin_ts/src/lowerer.zig` + `parser.zig`)

- `br <cond> -> <t>, <f>`:双目标强制;`break/continue` 只是 `jmp`;`throw` → `panic`
- 值返回函数必须 `-> T:`;无注解 = `void`(099/084 对齐)
- `load/store` 必须显式字节偏移 (`+ N as Ty`)
- `terminated` 跟踪:终结符后零死代码 (对标 `block_terminated`)
- switch = test/body  guard 链,无 fallthrough,未终止臂跳 end (对标 `parseSwitch`)
- 三元/`a?.[i]`/abs/pow = join-slot (`alloc 8` + 双臂 store + merge load)
- interface → LayoutTable (对齐 `getTypeSizeAndAlign` + `saTypeOf`:`number/boolean→i32`,其余非标量→`ptr`);字面量零初始化 + 分字段 store;`Enum.Member` 折叠为序号
- 数组 = 16 字节 `{ptr,len}` 头;`push` 增长拷贝;`pop/shift/unshift/fill` 与参考同形;无参 `sort` = 原地插入排序
- `Math.abs/pow/floor/ceil/round/trunc` 内联分支/循环形;`Math.sin…` 投影 `sa_std/math.sai`
- `new Map()` → `call @sa_btree_map_new()`;`new Array(n)` → 头 + `n*4` 零缓冲
- 模板串:整数经 `sext+@sa_fmt_i64_into`,块经 `@sa_string_concat` 连接

## 4. 标准库政策 (硬约束,用户确认)

**所有 std 投影到 `sci/sa_std`,前端零运行时模拟.**

每个 `StdProjection` 条目记录精确契约文件,生成 `.sai` 头部携带对应
`@import "sa_std/..."`,由 sci 编译器自带 `sa_std` 在 `sa build` 时解析,
脚手架不 vendoring 任何文件.符号名逐字核对过 `.sai` 契约
(如 `@sa_net_tcp_listener_bind`,非臆测名).无对应符号的构造一律定位拒绝
(同 `.wit` 拒绝哲学).`tools/check_sa_std_projection.sh` 在 CI 对
`$SCI_ROOT/sa_std` 逐符号核对.

已投影:console.log,模板内插/连接,String 全系方法,Number.parseFloat,
Math 三角/指数/舍入全系,Map,fs 全系 (含 fd 级 read/write + scratch 缓冲),
net 全系,async 预留 (`sa_std/async.sla`,Phase 2).

## 5. 验证现状 (2026-09-29, Go 1.27.1)

- `go vet` + `go test ./internal/saemit/` 全过
- sa_plugin_ts 全 demos 扫测:`tsgo-sa` **265 通过 / 21 诚实拒绝 / 0 崩溃**
- 265 个 `.sai` 经 `tools/check_sai_shape.py` 结构全有效
- 21 个拒绝全属 Phase 2 范畴:箭头闭包/高阶回调、数组回调方法、spread、
  async/await、class、`structuredClone`、新式数组方法
  (`toSorted/with/toSpliced/copyWithin/toReversed`)

## 6. 路线图 (Phase 2)

1. 箭头闭包静态脱函数 (out-of-line callbacks,per-arrow ctx) + 数组回调方法
2. checker 类型接入:完整 `number` 推断、float 定向、`?.`/`??` 全覆盖
3. async/await ready-future (`sa_std/async.sla`) + `async main` 同步驱动
4. spread/rest、多文件链接 (Program 级)、`.wasm` arity-extern
5. `sa build` 真机 e2e (需 sci `sa` 二进制;本容器以结构校验代替)
6. 以本后端逐步替换 `sa_plugin_ts` 手写 parser,checker 报错与 tsc 同源
