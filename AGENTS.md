在 Windows PowerShell 环境中处理包含中文的文件时，必须显式使用 UTF-8 编码。

代码与文本文件读取、统计行数、文本搜索优先使用封装增强工具（read_file, read_lines, count_lines, grep_search 等），避免在终端中使用慢速命令阅读文件或搜索代码
写入文本文件优先使用 apply_patch；如必须用 PowerShell 写文件，使用 Set-Content -Encoding UTF8 或 Add-Content -Encoding UTF8
不要用未指定编码的 Set-Content 或 Out-File 处理中文、Markdown、TOML、JSON 等文本文件
终端输出出现乱码时，先用 UTF-8 重新读取确认，不要直接认定文件内容损坏

---

## saemit 进展（每完成一个特性即更新本节，规则：完成→写 AGENTS.md→commit→push）

- 2026-09-29：单文件 286/286 demo 全过（`56ad4ec8d`）；todo/ 四阶段计划已定。
- 远端：`layola13/satsgo`，`main` 分支；认证经 `$GIT_TOKEN` + credential helper，禁止落盘。
- 约束：扫测门禁、拒则大声、复用 `sci/sa_std`；新符号进 `StdProjectionTable` + `check_sa_std_projection.sh`。
- 分模块开发：新特性独立成文件（如 `link_nsobject.go`、`link_erasure.go`），
  `saemit.go`/`program.go` 只留 thin hook；单文件超约 500 行或职责超两项即拆分。
  实现与测试同模块迁移（`link_erasure_test.go`）；旧文件历史格式问题不顺手重排。
- 复用面已扩展：`sa_plugin_node`（`node.sai`/`node.sal` + `all_exported_symbols.txt` 共 408 符号，
  fs/buffer/crypto/net/dns/http/os/path/process… 原生后端）优先于自造；
  `deno.sai`/`bun.sai` 同理（`Deno.*`/`Bun.*` 命名空间）。投影表需加 Backend 维度
  （sa_std vs node/deno/bun），`@import` 指向插件 `.sai`，u32 状态码 + slot-alloc/load
  调用形状与现有 fallible-trio 一致。deno/bun 暂无 exported-symbols 清单，补投影时需先索取。

### Phase 1：Program 多文件链接（进行中，见 todo/01_program_link.md）

- ✅ `LowerProgram`：import 图 + 可达集 + 环拒绝（带链）+ 命名/命名空间导入 + 未导入调用精准拒
  （`defined in X; import it first`）+ `export *`/default 拒；非入口文件符号前缀；
  跨文件类型环境共享；header 合并去重 + `@const` 冲突拒（`program.go` + 单测 5 项）。
  链接产物过真机 `sa check` + `build-exe` + 运行（多文件 42+12=54 对数）。
- ✅ `tsgo-sa build <dir>`：package.json/tsconfig 入口探测 + `--out/--entry/--mod/--no-cache`
  （前后位置皆可）+ `ScaffoldProgram`（per-file 调试 `.sai` + 合并 `src/main.sai` +
  按包聚合 gate 报告）+ 内容 hash 增量缓存（`.tsgo-sa-cache.json`）。
- 286 单文件扫测零回退（prefix "" 输出不变）。
- 复用约束已扩展 node/deno/bun（见上）。

### Phase 2：checker 接入（进行中，见 todo/02_checker.md）

- ✅ `typeCtx`（`typecheck.go`）：NoLib 内存 Program + checker，单文件/Program 共享，
  lower 改走 checker 的 SourceFile（节点身份对齐）；失败/异常一律回退语法 lowering。
  spike 实测：建 Program+bind 约 400µs，可忽略。
- ✅ `?.` 真守卫：checker 可空（union 含 null/undefined）→ null-join（`a?.b`/`f?.()`）；
  非空保持直调；effectful 基座大声拒。联合注解（`Box|null`）贡献首个已知布局。
- 286 单文件扫测零回退 + 形状全过；单测 2 项（可空守卫/非空直调）。
- ✅ 解构声明：`const [a,b] = arr` / `const {x,y} = obj`（init 单次求值 + 安全索引/布局偏移）+
  `for (const [a,b] of pairs)`；for-of 对象模式缺元素布局仍大声拒。286 零回退；单测 1 项。
- ✅ Map/Set 方法直连 `sa_std` btree 后端（`new Map/Set` 句柄标记 + 方法路由优先于数组面）：
  set/get/has/delete（presence 探针保真）/clear/size/getSize/keys/values/entries；
  add/has/delete/clear/size；key 经 mapKeySlice（string 直通/int 装箱）。286 零回退；单测 1 项。
- ✅ 调用元数检查：多传拒、无默认少传拒、有默认短调放行（replay 仍为已知缺口，注释标明）；
  箭头精确元数；importEnv/跨文件同规则。286 零回退；单测 1 项。
- ✅ `Math.sqrt/log10/random`（`@inline` 惯例 + 投影表）：整数二分 sqrt / 位数 log10 / 确定性 LCG；
  f64 追踪（字面量 + 拷贝传播）支撑 float-sqrt 大声拒。286 零回退；单测 1 项。
- ✅ `findLast/findLastIndex`（无早退覆写 slot）+ `Array.from` mapper（clone 后走 map 内联，
  `{length}` 按 JS mapper 协议传 index）。286 零回退；单测 1 项。
- ✅ node 插件后端试点（`os.platform/arch`）：投影表 Backend 维度 + `node.sai` `@import` +
  u32 状态检查（非零 panic，大声）+ string out-param wrap；`node:os` 前缀等价；
  `check_sa_std_projection.sh` 加 node 区（`all_exported_symbols.txt` 精确匹配）。
  67 sa_std + 2 node 契约全过；单测 1 项。btree 符号一并补进 sa_std 名单。
- ✅ node `os.*` 同形状扩展（homedir/tmpdir/hostname，零参 string out-param 通用路径）；
  node 契约 5/5 全过。
- ✅ node `os.*` Batch2（release/type/endianness/machine/cpus，同 `(&out_ptr,&out_len)->u32`
  形状，免发射器改动，纯投影表+契约+单测）；node 契约 12/12 全过（Go 1.27.1
  `go test ./internal/saemit/` 全过；sci/sa_std 侧 31 缺失为基线预存，与本批无关）。
- ✅ node `os.*` Batch3（version/userInfo/networkInterfaces，同 `(&out_ptr,&out_len)->u32`
  形状；TS 键从 Node 驼峰命名）；node 契约 15/15 全过，全量单测过。
- ✅ `check_sa_std_projection.sh` 纠偏：删除 18 个从未被发射的 `sa_math_*`
  三角/指数/对数符号（两前端一律大声拒，见 `stdlib.go` 注释；脚本必须镜像投影表，
  不得超前）。剩余 13 个 string 符号缺失为 **sci 侧真实缺口**
  （`sa_string_index_of` 等两前端实发、`origin/main` 亦无，`sa` 二进制无内建），
  按“禁 tsgo 原创”立项到 sci（Zig runtime + `string.sai` extern，另起 commit）。
- ✅ sci 缺口关闭（13/13，`cross-platform` 连续 4 个 commit，均 C 实测全过）：
  谓词 4（index/lastIndex/starts/ends，22 用例）→ 大小写+parseFloat 3（21 用例）
  → repeat/pad/replace 4（17 用例，node 交叉核对）→ codePointAt/fromCodePoint 2
  （11 用例；OOB=-1 哨兵，非法码位=U+FFFD；windows 交叉编译干净）。
  `check_sa_std_projection.sh` 现 **49 sa_std + 15 node 全过**（此前 31 缺失归零）。
  注：286 demos 无一覆盖这些 string 方法（链接前零 e2e），本批是首次可链接。
- ✅ node `path.*` 单 slice 批（normalize/dirname/extname）：投影表 Backend 维度加
  `NodeOut "string1"`（一进一出：`&ptr+len` 进参 + u32 状态检查 + slice 包装；
  `join/resolve` 的 argv 桥另立项）。node 契约 18/18 全过；单测 2 项
  （成功形状 + 元数拒绝）；`tsgo-sa` 实测发射形与 `node.sai` 逐位对齐。
- ✅ node `path.*` argv 桥（join/resolve）：`NodeOut "argv"` 变参打包为 16 字节
  `{ptr,len}` 数组（SA slice 布局 = 插件 `SaSlice`；零参仍 alloc 一槽，
  插件按 argc==0 短路，无 null 发明；argv 用后 `!` 释放）。
  node 契约 20/20 全过；形状校验器 `check_sai_shape.py` 通过；全量单测过。
- ✅ node `crypto.randomBytes`（`NodeOut "sized"`：u64 size 进 + 裸 `&ptr` 出，
  长回显请求值；`crypto` 进 import 白名单含 `node:crypto` 前缀）。
  createHash/update/digest 链需句柄追踪（Map/Set 先例），保持大声拒，另立项。
  node 契约 21/21 全过；形状校验通过；全量单测过。
- ✅ node `Hash` 链（createHash/update/digest）：`hashAcc` 按名追踪，update 经
  `sa_string_concat` 折叠并 `assign` 重绑定，digest 走 `NodeOut "string2"`
  （algo+data 双 slice，hex 原生）；非 hex 编码、 finalized 后使用、未知方法、
  非字面编码一律大声拒（`ERR_CRYPTO_HASH_FINALIZED` 语义）。
  node 契约 22/22 全过；形状校验通过；单测 4 项；发射形与契约逐位对齐。
- ✅ node `Hmac` 链（createHmac/update/digest）：`hashState` 加 `kind/key`，
  digest 按 kind 路由 `crypto.hash/hash`（`NodeOut "string3"` 三 slice）。
  诊断按 kind 区分；node 契约 23/23 全过；形状校验通过；单测 2 项。
- ✅ node `querystring`/`url` 批（escape/unescape/parse/stringify + parse/format/
  resolve，复用 string1/string2；`querystring`/`url` 进 import 白名单）；
  JSON 边界以文本形式穿越（parse/format 来回），文档注明。
- ✅ import 别名直达（`import { parse as uparse }`/`createHash as ch`）：
  `importedRemote` 记远端导出名，调用点与 `hashAcc` adoption 双侧解析，
  覆盖 crypto/path/url；此前别名一律误拒。单测 2 项。
- 说明（回应“node 是否与 sa_plugin_node 重复”）：不重复——`sa_plugin_node`
  是纯 Zig 原生运行时（无 TS 解析/降级逻辑），satsgo 投影是其 TS 前端桥，
  零运行时模拟；`sa_plugin_deno` 有 57 externs 但无 exported-symbols 清单、
  无 bun 插件，Deno/Bun 投影待后端先行（本轮不动）。
- ✅ node `util.stripVTControlCharacters`（纯 string→string，复用 string1；
  `util` 进 import 白名单）。`format`（缺 args→JSON 编码器）、`inspect`
  （入参为任意值而 `expandSlice` 无类型守卫）、`isDeepStrictEqual`
  （缺 bool-out 分支）、`formatWithOptions`（插件符号表无）四项保持大声拒，
  单测锁定 3 项拒绝。node 契约 31/31 全过；全量单测过。
- ✅ Date MVP（`Date.now()` + 无参 `new Date()` + `getTime` 恒等）：直调既有
  `sa_time_unix_ms`（sci time.sai 早于参考 Date 支持，属补齐非分叉）；
  Date 对象窄化为 i64 millis（文档注明）；`new Date(x)`/parse/toISOString
  大声拒。sa_std 契约 50/50；单测 4 项（1 成功 + 3 拒绝）。
- ✅ `toISOString`（sci `sa_time_iso_from_unix_ms` + satsgo `dateVars` 路由，
  i64 值传递、零新发射分支）：UTC millis 精度，Hinnant 负值折叠；
  途中修了 Zig 0.14 `{d:04}` 必带 `+` 号的 latent bug（deno 同款顺手修，
  改手工补零）。  C 实测 8 用例全过（含闰日，与 node 逐字对）、windows
  交叉编译干净。sa_std 契约 51/51；单测 2 项（成功 + 元数拒绝）。
- ✅ `Date.parse`（sci `sa_time_parse_iso` 严格 ISO + satsgo
  `emitStatusCheckedI64` 状态检查分支，非法输入 panic——NaN 不可表示）：
  年月日/时分秒/毫秒/时区偏移全覆盖，闰日/越界拒绝；C 实测 16 用例
  （与 node oracle 逐字对，含 8 拒绝）；windows 干净。
  sa_std 契约 52/52；单测 2 项（成功形状 + 元数拒绝）。
- ✅ Date get 系（sci 8 原语共享 Hinnant civil helper + satsgo 零新分支路由，
  `getTimezoneOffset` 恒 0 UTC，setter 大声拒）：C 实测 24 用例与 node
  逐字对（含 1969 负值；另验证 NY 时区下实现为 UTC 固定）；
  sa_std 契约 60/60；单测 2 项（8 调用形状 + setter 拒绝）。
- ✅ Date setter 系（sci `sa_time_set_field` 单原语 + satsgo 分支显式组装
  `(ms,field,value)` 并 `assign` 重绑定）：JS 翻转语义全归一（month 13/
  date 0/hours 25/负值）；途中抓到 Extra 追加导致的实参错位，改显式 splicing。
  C 实测 11 用例与 node 逐字对；sa_std 契约 61/61；
  单测 2 项（7 调用形状 + 元数拒绝）。
- ✅ Date toString 系（sci `sa_time_format_utc` 单原语 + format-id，
  satsgo 4 表行经 Extra 末位对齐 + `valueOf` 恒等；`toLocale*` 大声拒）：
  与 node oracle 逐字对 8 用例（含闰日/负值/非法 fmt）；windows 干净。
  sa_std 契约 62/62；单测 2 项（4 调用计数 + locale 拒绝）。
- ✅ Date 未知方法显式拒（`dateVars` 尾部不再掉入泛尾，`toLocale*` 等报
  Date 专属诊断）：单测扩展 1 项（3 方法 × 专属诊断断言）。
- ✅ 链接图擦除显式类型边（`import type` / `export type`，链接图 + 降级双侧；
  裸副作用导入保留）：planck `Shape → Body` 边消除，值循环告警收敛到
  `Shape ↔ Distance`（value import 但仅类型位使用，待基于用法的擦除另立项）。
  单测 1 项（import-type/export-type 双形状）；全量单测过。
- ✅ 基于用法的擦除（`valueUsedNames` + `importDeclValueEdge`，链接图与降级
  双侧；heritage/装饰器/JSX 保留为值，误伤方向恒为“多留边”）：
  planck program 口径环告警清零，收敛到单点 `export default {...}`
  （命名空间对象，另立项）；真值循环单测同步修正（b 必须真实调用 main）。
  单测 1 项（擦除成功 + 值环仍拒）；全量单测过。
- ✅ 命名空间对象 p1（`export default {a, b: c}` + `D.m()` 路由）：
  `fileExports.defNS`/`modResolution.defNS`/`defNSImports` 三件套，
  复用 nsImports 绑定形状；planck program 口径 **56 文件链接**（此前 0）。
  单测 1 项；全量单测过。
- ✅ 命名空间对象 p2（`export default ns` 透传，`link_nsobject.go` 模块）：
  图循环记录 ns 别名 → 导出扫描认领 → links 期按 reexpQualified 展开播种；
  planck `main.ts` 形环/默认告警清零。单测 1 项；全量单测过。
- ✅ 用法擦除模块化（`link_erasure.go` + `link_erasure_test.go`，实现随测试
  同迁；旧文件历史 gofmt 问题不碰）：零行为变更，全量单测过。
- ✅ package.json→sa.mod（`npm_deps.go` 模块，JEV 降级方案）：`dependencies`
  排序读出（dev 排除、版本原文），`sa.mod` 注释 require 行（`#` 为合法注释，
  hash 缺位故不输出有效行）+ subset-report 独立 section；CLI build 链透传，
  单文件口径不涉及。单测 2 项；全量单测过。
- ✅ checker 布局 v1（`checker_layout.go` 模块，todo/02#2 首刀）：
  `layoutOfNode` = 记录表优先 → checker 接口/类/别名名 → 匿名形字段集
  `matchLayout`；`lowerMemberChain` 基址改走它（嵌套下沉不动）。
  工厂返回等推断布局首次可达；planck program 口径 diag 686→649。
  单测 1 项（具名推断 + 匿名形 + 未知仍拒）；全量单测过。
  已探明 v2：`a.q.c` 嵌套别名（`type TransformValue={...}` 未建布局）另立项。
- ✅ checker 布局 v2（`recordTypeAlias` 进 `checker_layout.go`，`lowerTypeDecl`
  挂钩）：`type X={...}` 对象别名建布局，嵌套经既有 ftypes 下沉；
  planck program 口径 diag 649→617（累计 686→617）。单测扩展 1 项；
  全量单测过。
- ✅ checker#5 捕获分析换共享 walk（删 `collectValueIdents` 手写 walk，
  arrow 捕获走 `valueUsedNames`）：286 输出逐字节一致（ Lower 口径 286/0
  前后相同）；单测 1 项（类型名不进捕获）。
- ✅ checker#6 首刀 typeof 守卫（`typeof_guard.go` 模块）：`typeof v === "undefined"`
  （双向、否定式）降为 `eq/ne v, 0`（子集 null/undefined→0，故精确）；
  非标识操作数大声拒；planck `typeof min/max` 4 项消除（diag 617→613）。
  单测 1 项（肯定/否定/非标识拒）；全量单测过。
- ✅ checker#6 已知 kind 折叠（`typeCtx.typeofKind`）：number/string/bigint/
  boolean/symbol/void/undefined/null→object/function/object 联合一致才折叠，
  any/unknown/exotic 大声拒；方言原语（`i32` 注解 NoLib 下本就是 Any，
  只认真实 TS 类型）。单测扩展 2 项（number 折叠 + any 拒绝）；planck 持平。
- ✅ checker#6 比较级常量折叠（`lowerTypeofConstFold`，`typeof_guard.go` 内 +
  `lowerBinary` 薄钩 1 处，todo/02#6 第二刀）：`typeof X === "<kind>"`
  （双向、==/===/!=/!==）静态 kind 已知即折叠为 `eq/ne 1, 1` 常量比较；
  标识符走 checker（与 `lowerTypeof` 同源，不可能分歧）、字面量按语法表
  （含 null→"undefined" 方言映射，与 `lowerTypeof` 逐项对齐）；
  未知 kind（any/方言原语）回落既有大声路径；"undefined" 对仍走空检查。
  此前同形需两串 alloc + `index_of` 调用，现 1 条指令。
  途中抓到 `br` 只吃寄存器（`br 1` 报 UnknownRegister，真机 `sa check`
  定位；`if (1)` 旧发射同病但 286 无覆盖故潜伏，本批不碰旧形）。
  单测 1 项（7 形状 + 值位 + any 拒 + 非标识 undefined 拒 + 空检查保持）；
  全套件绿、286 sweep 零 diff；真机 `sa check` 过 + `sa run` exit 13
  与 node 差分一致。
- ✅ checker#6 注解兜底（`scalarTypeofKind` + `kindVars`，`typeof_guard.go` 内 +
  `trackBinding` 末位记录 + `lowerTypeof`/`lowerTypeofConstFold` 双侧 thin hook，
  todo/02#6 第三刀）：方言标量注解（`i32/i64/u64/f64/boolean/bigint`，
  checker NoLib 下盲为 Any）按声明真相折叠，`typeof y === "number"`（y: i32）
  由拒转过；checker/字面量优先，串/数组/布局/float 既有认领恒胜出，
  用户类型名永不标记，无注解/any 仍大声。
  单测 1 项（翻转 + 5 注解形状 + 裸 typeof + 无注解拒）；全套件绿、
  286 sweep 零 diff；真机 `sa check` 过 + `sa run` exit 10 与 node 差分一致。
- ✅ checker 布局 v3（`layoutOfCheckerName` 抽取 + spread 源接 checker，
  `checker_layout.go` 内 + `saemit.go` 薄钩 2 处，todo/02#2 收尾）：
  精确名恒胜字段集猜测（`layoutOfNode`/`layoutOfLiteral` 共享；
  同名字段异类型布局此前按 map 序误配，现按 checker 真名）；
  spread 源走 `layoutOfNode`（工厂返回/推断 const 此前无记录即拒，
  现经 checker 命名；`any` 源仍大声；外层目标键集匹配与错配拒文不动）。
  单测 1 项（双翻转 + 速记注解 + any 拒）；全套件绿、286 sweep 零 diff；
  真机 `sa check` 过 + `sa run` exit 120 与 node 差分一致。
- ✅ checker#7 评估收尾（todo/02#7 关闭为“删无可删”，零产品代码变更）：
  全站 26 处 `lookupBinding` + push/pop/ownership 约 150 触点逐项分类——
  所有权/别名（binder 盲）与本地遮蔽路由（emitter 答 lowering-time 在场，
  binder 答源码在场，迁移即错）永久保留；全局可见性早已 binder 化
  （`declaredAt` + `linkRoute`），无残留。
  边界锁 `TestLowerProgramLocalShadowsImport`（局部参遮蔽同名导入，
  干净胜出零诊断）；全套件绿、286 sweep 零 diff。
- ✅ env-probe 折叠 + 常量条件具化（`envProbeArm`/`materializeCond`/
  `splitTypeofCompare`，`typeof_guard.go` 内 + 薄钩 8 处，todo/02#6 第四刀，
  planck 顶层变量簇收敛）：`typeof G === "undefined" ? A : B`（G binder 不可见，
  syntax-only 回退除外）取当臂，未取臂永不 lowering（planck `typeof ASSERT`
  惯用法；模块 const 经 `tryTopLevelConst`/`registerModDeclarator` 预折，
  余位经 `lowerTernary` 钩）；整数立即条件经 `ne 0` 进暂存
  （`br` 只吃寄存器，`if (1)`/const-bool-if 家族潜伏陷阱一并修复，
  `if/while/for-test/do/ternary` 五位）。
  途中抓到真 panic 回归（`splitTypeofCompare` 首版对三元非二元条件无守卫，
  281 demo 扫测拦截；单测锁定非二元三元）。
  单测 1 项（模块/函数探针 + 声明名不折 + 常量分支 + 防 panic 回归）；
  全套件绿、286 sweep 零 diff；planck 620→567（top-level-variable 240→186，
  零新增-kind）；真机 `sa check` 过 + `sa run` exit 6 与 node 差分一致。
- ✅ Math 别名值位（`lowerExpr` 标识尾 + 单测 1 项，planck math_ 簇）：
  `2 * math_PI` 此前直落裸名（`mul 2, math_PI`，未声明寄存器静默陷阱；
  286/sweep 全无覆盖故潜伏）。现 const 投影（PI/E）经 `mathConstFold`
  与直用同表折叠（单测锁别名与直用逐字节一致），函数投影值位大声拒
  （调拨仍走 `lowerMathCall`）。sin/cos/atan2 无后端（sci 政策：禁 tsgo
  原创运行时），声明与调用双位大声如旧，待 sci `sa_math_*` 另立项。
  全套件绿、286 sweep 零 diff；真机 `sa check` 过 + `sa run` exit 22
  （整数子集 PI=3，与 node 22.84… 截断一致）。
- ✅ 方法分派注解接收（`checkerClassOf` + `reboundNames` + 静态隔离，
  planck 一阶函数值簇破局）：`varClass` 仅 `new` 初始化写入，注解形参
  （`fA: Fixture`、`input: DistanceInput` 类）调方法恒拒。现 checker
  声明类型直命名类（跨文件亦可），零映射写入故无跨函数陈旧；
  回退守卫四重：重绑定（`reboundNames` 预扫，成员存储除外）、可空、
  静态位（类名本身）、未声明/非类；静态方法不再进实例表
  （同名静态曾按源序遮蔽实例项，`v.normalize()` 误报 arity）。
  单测 1 项（形参/局部翻转 + 字段存储不毒化 + 重绑定/可空/静态/
  未注解兄弟大声）；全套件绿、286 sweep 零 diff；
  planck 567→538（first-class 135→97；新增皆大声：内联体成员真缺口
  + 跨文件内联环境提示，失配静态提示为既有文案问题另记）；
  真机 `sa check` 过 + `sa run` exit 40 与 node 差分一致。
- ✅ 跨文件 const 值导入（`globalConsts` + `modResolution.consts` +
  进口商 const 优先分支，planck EPSILON 簇；单测 1 项）。
  导出扫描对 `export const` 字标量记录文本（模板/对象恒拒；`let` 恒禁：
  重赋值会污染进口商折叠，而进口商 const 重赋卫恒设不可变）；
  进口商直接折进既有 constVals/constIsStr（f64 经 isFloatLiteral 同形），
  桶转口与重导出链 v1 恒拒（不进 star/reexp 枚举）。
  附带修出：`export const` 此前三连拒（not-exported + not-known +
  value-gap），现一次折叠全消；旧 typeof 守卫单测期望同步翻转
  （另加 `export let` 无值对照锁）。
  全套件绿、286 sweep 零 diff；planck 538→522（-16 全为 EPSILON，
  零新增-kind）；真机 check + run 差分一致（7==7）。
- ✅ 死分支消除（`isFalseConst` + if/while/for/ternary 四位，
  planck 断言簇；单测扩展 1 项）。
  常量假条件直接消死臂且永不 lowering（`_ASSERT` 折叠后
  `if (_ASSERT) console.assert(...)` 整臂蒸发，内层 getType 同消；
  真条件恒走物化旧路；for 假仅留 init 并平衡 scope；do/switch 不动）。
  全套件绿、286 sweep 零 diff；planck 522→477
  （first-class 97→60，零新增-kind）；真机 check + run 差分一致（11==11）。
- ✅ 跨文件 enum 值导入（`enumMemberTable` 抽取 + `globalEnums` /
  `modResolution.enums` / 进口商枚举分支；单测 1 项）。
  导出扫描对全整数枚举记成员表（`const enum` 同形；串/计算成员恒拒）；
  进口商直填既有 `e.enums`（比较/switch/赋值零改动；同名单纯值绑定，
  杜绝函数误绑）。编号核与单文件同源（含 `= -1` 负号支持，
  顺手修出单文件负枚举静默错号）。
  桶转口经共享预播种天然可达（单测锁定 `E.B` 折叠）。
  全套件绿、286 sweep 零 diff；planck 477→465（-12 全为枚举，
  零新增-kind，残留 not-exported 仅剩 stats 对象×3）；
  真机 check + run 差分一致（20==20）。
- ✅ 类型边布局共享（`typeUsedNames`/`importDeclTypeEdge`/`typeSpecOf`/
  typeReached 闭包 + 类型预扫不动点 + typeCtx 扩集 + 共享预扫；
  `link_erasure.go` + `program.go`，单测 2 项）。
  注解位导入（`import { P }` 仅用于 `v: P`）此前整文件不可达，
  布局/类/枚举全丢；现目标文件进类型闭包（预扫映射 + checker 集，
  永不融合环/链接/绑定），值边语义不动（环单测全绿）。
  途中修出预扫共享深坑：逐文件 scratch 使跨文件继承基永远不可见，
  改共享映射（post-order 天然 base-first；类型闭包不动点收敛）。
  全套件绿、286 sweep 零 diff；planck 465→465（零新增-kind；
  注解位点仍被下游真缺口挡住，见下）；真机 check + run 差分一致（7==7）。
- ⏸ 数组内存模型与索引结构体（JEV-(a) 调查结论，不 ship 代码）：
  实测证伪两条：① 数组 `!buf` 在构造处释放属 use-after-free，
  但挪后则泄漏/越界陷阱——线性释放无法表达 header 拥有 buffer
  （逃逸/增长数组无解，需 sci 侧 arena/GC；`sa` 现二进制裸数组读
  全挂，demo052 亦然，与本片无关）；② 结构体数组创建把元素指针
  存进 i32 槽，64 位读回恒陷阱（手写 SAI 证伪）。
  已实现又回滚：`lowerElemAddress` 地址 join（设计存档待创建模型修复）、
  return-消费标记（跨臂污染 bindings，被 5 个 sweep diff 抓获 revert）、
  buffer-declareOwned（循环缓冲 scope 陷阱 revert）。
  保留 4 行无害元数据：类 `fdefs` 记录（286 零 diff 实证）。
  后续：sci 侧先定数组内存模型，再做索引读写。
  附带修出：switch 臂 return-call 结果 `!` 落终结符后
  （`releaseScope` 终结守卫，一处改全臂；286 零 diff 说明旧语料无覆盖，
  新形状由 enum e2e 真机锁定）。
- ✅ 类型-only 具名符擦除（`lowerImport` 逐符擦除 + 单测 1 项）：
  整声明擦除（`importDeclValueEdge`）早已落地，残留的是同声明内混合
  `import { Vec2, Vec2Value }`——值兄弟存活而接口符报 `not exported`。
  现零值使用符/显式 `type` 符跳过绑定（逐文件 `valueUsed` 预扫集已就绪，
  纯加法；误跳过恒大声）。planck program 口径 692→620（-72 全为误拒清零，
  零新增；残留 25 核为真值缺口：跨文件 const/enum 值导入另立项，
  枚举值用例证伪了过度擦除）。
  JEV-(a)字面语义（命名空间 spread/动态键）经实测证伪：planck src 零用例
  且 p2/p3 早落地，故不做推测实现，改收敛实测最大项（JEV s1 79%）。
  全套件绿、286 sweep 零 diff；链接产物真机 check + run 差分一致（7==7）。
- ✅ 访问器记录 + 精确拒（`classDef.getters/setters`，读经
  `lowerPropertyAccessInner`，写经 `lowerFieldStore`，`classDefOf` 三路解析）：
  未读 getter 的类不再整文件拒；读写报专属诊断（内联含 `this`/副作用，
  另立项）。planck diag 613→584（正好是 getter 簇）；单测 1 项
  （放行 + 读写专属诊断）。
- ✅ 类静态折叠（`static X = 字面量`，`as const` 链解包；实例布局排除静态）：
  heritage 类早退前先收割静态进隔离 `staticDefs`（实例路径不可见），
  读经类名/实例双路折叠；写保持原拒。途中抓到 heritage 部分 def 泄漏致
  `lowerNewClass` 空指针 panic，已隔离 + 加哨兵拒（hostile 输入零 panic 门禁）。
  planck `.TYPE` 诊断 28→0（extends 文件仍整拒，折叠生效待 extends 支持；
  跨文件非 heritage 已单测锁定）；diag 584→564；286 逐字节一致；
  单测 1 项（折叠 + 跨文件 + heritage 拒文）。
- ✅ checker#7 首刀 binder 可见性（`typeCtx.declaredAt`，GetSymbolAtLocation）：
  typeof 尾部分支改走“scope 无 → binder 不可见才 unknown global”；
  所有权/别名仍归自建 scope（binder 无所有权概念，边界注明）。
  单测扩展 2 项（单文件 unknown 锁定 + program 导入可见性翻转）；全量单测过。
- ✅ checker#3 泛型单态化（`checker_layout.go`：模板记参 + `monoKey` 规范键 +
  `instantiateLayout` 缓存 + `layoutOfAnnotation` 接入注解/联合）：
  `Box<i32>` 与 `Box<string>` 布局分宽；未知模板回退裸布局；
  递归泛型嵌套保持 raw 键（懒实例化另立项）。286 输出逐字节一致
  （纯加法）；单测 1 项（分宽断言）；planck 持平 613。
- ✅ checker#3 递归懒实例化（shell 预占 + 实参代入递归，互递归经缓存收敛；
  含参泛型实参/深层嵌套仍 raw）：`List<T>` 自递归一次通过；
  286 逐字节一致；planck 持平；单测扩展 1 项。
- ✅ tsx `useState`（路二任务 5，`tsx.go` 内）：前导语句仅限
  `const [x, setX] = useState(数字/布尔)`，state 块发射字面量，
  模板 `{x}` 插值；字符串初值/setter 引用/计算表达式/杂语句大声拒
  （handler 仍拒，旧单测保持）。单测 1 项（成功 + 4 拒绝）；全量单测过。
- ✅ tsx 挂载 `useEffect`（路二任务 6，`@onMount` 块）：空体出裸块，
  setter 常量体直存 state 槽 + 一次 render；非空/缺 deps、cleanup 返回、
  其他 hooks、计算参数大声拒。途中抓到 setterOf 键值反转 bug（调试打印定位）。
  单测 1 项（成功 + 空体 + 4 拒绝）；全量单测过。
- ✅ useEffect console 体判定（`.sax` 无 print 机制，盲目发射即错方言，
  故维持大声拒并单测锁定）：单测扩展 1 项。
- ✅ DOM 投影 p1（路二任务 7，`dom_proj.go` 模块）：`document.createElement`/
  `appendChild`/`setAttribute` 对 airlock `sax_dom_*`（i64 句柄经
  `domVars`/`domTemps` 追踪，非句柄/非 string/未知方法大声拒；
  extern 由 `sa react build` 侧提供如 `sax_get_time`）。
  形状与 airlock 契约逐位对齐；单测 1 项（3 形状 + 嵌套 + 3 拒绝）。
  注：vm edit 在个别大文件出现幻写成功，已改“写后必验”（单测即验）。
- ✅ DOM 投影 p2 文本系（`createTextNode` + `textContent`/`innerHTML` 写）：
  工厂经 `domCreateSym` 表（含 `trackDomBinding` 同步），写经赋值路径前置钩
  （仅单段 + string RHS，读与未知属性大声拒）；读（get_text 需调用方 buf）
  另立项。形状对齐；单测 1 项（3 形状 + 2 拒绝）。
- ✅ DOM 读（`el.textContent` 经调用方 4096 scratch + 满缓冲 panic 哨兵；
  slice 别名 scratch，双双持有到出口）：顺手堵了 `.length` 读句柄的
  静默垃圾（改大声拒）；`innerHTML` 读无契约拒。单测 1 项（形状 + 2 拒绝）。
- ✅ DOM attrs 系（`getAttribute`/`removeAttribute` 方法 + `className`/`id`
  糖读经共享 `domRead` helper；set 沿用）：单测 1 项（3 形状 + 1 拒绝）。
- ✅ node console 批（`node_console.go` 模块，log 不动 sa_std）：error 多参同
  log 折叠后单 slice 穿越，time/timeEnd 原生配对（缺省 label `default`，
  缺失 timer 经状态 panic），clear 零参；新增 `NodeOut "fire"/"fireF64"`
  （值传 slice + 状态检查，前者无出参后者 f64 出参）；timers 保持大声拒
  （async，Phase 2）。node 契约 35/35；形状校验通过；单测 1 项
  （4 调用形状 + 3 拒绝：timeEnd/clear 元数 + 未知方法）。
- ✅ deno 后端试点（`Deno.hostname/osRelease`，`isPluginBackend` 泛化
  node/deno 同 u32-status 形状；无线束清单，以 `deno.sai` + Zig 实现双验
  为契约，check 脚本加 deno 区）：`Deno.*` 全局直达，形状对齐；
  deno 契约 2/2；单测 1 项（成功 + 未知成员拒）。bun 仍无插件，不动。
- ✅ deno 文件批（`readTextFile` 经 string1，`writeTextFile` 经 fire 双 slice；
  `Deno.env.*` 需两级命名空间路由，单测锁定拒绝）：deno 契约 4/4；
  形状校验通过；单测扩展 2 项。
- ✅ deno env 两级路由（`node_deno.go` 模块 + `NodeOut "nullable"`）：
  `get/set/delete` 经 `Deno.env` 链分发（元数 + string 类型双检）；
  缺失（status 1）映射 null `0`（子集一致），其余非零 panic，
  双臂在单 temp 上 join；其他 `Deno.x` 链大声拒。deno 契约 7/7；
  形状校验通过；单测扩展 2 项（成功三调用 + 未知链拒）。
- ✅ deno fs/编码批（`cwd/chdir/mkdir/remove` + 裸 `btoa/atob`，`node_deno.go`）：
  mkdir/remove 单路径（Extra 末位补 recursive=0，fire 分支首支持 Extra）；
  btoa/atob 用户影子优先；deno 契约 13/13；形状校验通过；
  单测扩展 2 项（6 形状 + 元数拒）。
- ✅ deno version 面判定维持拒绝（`version_json` 等无直接 TS 拼写，
  对象形态需物化，单测锁定 3 项）：不发明对象形状。
- ✅ 命名空间对象 p3（`const {a, b: c} = NS` 解构，直达 qualified）：
  缺省/嵌套/spread/计算键大声拒；spread/动态键仍另立项。单测 1 项
  （成功 + 改名 + 未知成员拒 + 缺省拒）；全量单测过。
- ✅ timers 拒绝锁定（`node_timers.go` 模块）：6 个异步定时器全局以 Phase-2
  事件循环为由大声拒（置于未知函数之前，用户自定义同名函数仍优先）；
  `timers_sleep` 无同步 JS 语义故不投影（SA 原生侧用 `sa_time_sleep_ms`）。
  单测 1 项（3 拒绝 + 影子优先）。
- ✅ node Buffer 批（`node_buffer.go` 模块）：`byteLength` 经新 `NodeOut "u64out"`
  （1 slice + u64 出槽 + 状态检查）；`concat` 仅接字面量元素（标识符/字符串，
  绕过存不下 slice 的数组模型，直接打包静态 argv，动态数组/非串元素大声拒）。
  node 契约 37/37；形状校验通过；单测 1 项（2 形状 + 3 拒绝）。
  注：本轮 vm 写出现幻写成功与 NUL 注入，已逐项写后核验并修复。
- ✅ node 全局命名空间（`process.cwd`/`crypto.randomUUID`，免 import，方法路径同形状）；
  node 契约 7/7 全过。注：node 后端输出需插件环境才可 `sa check`/运行，
  门禁为符号契约 + 形状；sa_std 面仍全量真机。
- ✅ 未决第三方依赖聚合：`LowerProgram.Unresolved` + `subset-report.txt` 按包 section
  （`package X: no SA backend yet`），裸 import 仍大声拒；单测 1 项。286 零回退。
- ✅ `export default` + 默认导入（`export default f;`/`export {x as default}`/声明位）：
  lodash program 实测消灭 25 项默认导入拒 + 级联 `import it first`；匿名/`export =` 大声拒。
  单测 1 项（成功/赋值式/无默认）；286 零回退。
- ✅ 顶层纯量折叠：字面量内联（`var K=42`/`S="hi"`，f64 保类型）+ `var f=Math.g` 别名调度；
  重赋值自动摘表；effectful（require/typeof 链）仍拒。lodash chunk 子树 top-level 拒 26→20；
  单测 1 项。286 零回退。
- ✅ `null/undefined/void 0` 按子集映射为 0（`== null` 守卫、`f(null)` 直传）+
  `Array(n)/Array(a,b)` 构造调用（等价 `new Array`/字面量）。lodash 实测 null/Array 类拒清零；
  单测 1 项。286 零回退。
- ✅ `.d.ts` 配对：同目录 `x.d.ts`↔`x.js` 签名覆写（返回值/元数/可选即默认）+
  顶层无注解体回退；date-fns esm 子树默认链无级联（其 d.ts 为空 re-export 印证需真类型源）。
  单测 1 项；286 零回退。

### 真机门禁（`sa check` + Node 差分，`tools/verify_demos_diff.sh`）

- 286/286 过 `sa check`；差分 259 机器验证 + 9 手工验证（073/154/156/221/274-278，
  oracle 侧 strip 不支持 tuple/泛型，手工对数全对）+ 18 fs/net 系环境项
  （真 IO，需 fixtures；汇编+运行正常）。
- 差分修出的真 bug 族（单测已锁关键两项）：
  - 所有权：call 结果一律 own（出口释放）；`assign` 重绑定复位 released；
    rest 调用恒打包；fallible scratch 用后释放；`releaseAllOwnedExcept` 不再清空 scope
    （early return 后名字丢失，045/222）+ `kept` 别名迭代修（曾 277→223 回退）。
  - 作用域：零分配分支改单路径过分配（`alloc 0` 改 `(n+1)` 槽）；`!raw` 后循环重载 header；
    跨 arm 重绑定改无分支算术（at/with/toSpliced-d）；`out=add dh,0` 删（alloc 头自 own）。
  - 槽模型：数组槽统一 4 字节 + i32 流量（字面量/push/check-index 一致；嵌套句柄 round-trip），
    结构体布局仍 widthOf。
  - 协议：from_char_code 经 buffer_data/len 解包（直接读 u64 会 segfault）；
    console.log 按参考重写（render+空格+换行，补 fmt/string import，`@const` 转义 \n\r\t）；
    `.length` 结构体同名字段优先；clone 恢复句柄性（Identifier 恒 tI32 坑）；
    Identifier 上报静态类型（修字符串变量被 sext 格式化）。
  - 复制：`copyRange` 目标偏移用 di（曾用循环变量 i，toSpliced 错 4）。
- ✅ `typeof` 窄子集 + 字符串内容相等：已知静态 kind（字面量/string/array/布局/箭头/函数/
  常量；未知全局/动态值大声拒）折叠为 slice；`==`/`!=` 双 string 走
  `index_of==0 && 等长`（地址比曾静默错）。真机 63 对数；单测 1 项。286 零回退。
- ✅ `fs.readFile` buffer 协议：u64! 按 `{status:i32,payload:u64}` 取 +8 payload 后
  经 data/len 解包（直接当 slice 读长度错 4 vs 8）；write→read roundtrip 真机 8 对数。
  单测 1 项。286 零回退。
- ✅ fs 真机验证（fixtures）：write/read/mkdir/remove 全部真实生效
  （`b.txt` 7 字节落盘、`gone.txt` 删除、`newdir` 创建、read 长度对）；
  `create`（`sa_fs_file_create`）裸调亦返回 1 且不建文件——stdlib 侧行为，
  与参考一致，非 lowering 问题。net（live listener 无连接到达）归环境桶，
  与参考 `upstream` 口径一致。单测 1 项。286 零回退。
- ✅ `in` 静态折叠（布局固定→编译期 1/0，进寄存器）+ `delete` 大声拒
  （静态布局不可删字段；Map/Set 用方法）。真机 1 对数；单测 1 项。286 零回退。
- ✅ `f.call(t, ...args)` 脱糖（thisArg 作首参，契合静态方法惯例；别名/导入/函数三路；
  类实例自有 `call` 优先）。真机 42 对数；单测 1 项。286 零回退。
- ✅ 透传导出：`export {x} from` / `export {x as y} from` / `export * from`
  （qualified 直达定义、rets/arity/默认透传、环带链拒；`export *` 不含 default；
  default 透传经 defQualified）。单测 1 项（命名/star/环）。286 扫测 + `sa check` 全过。
  透传 + 默认导入混合程序真机 23 对数。

### Phase 4：tsx→SAX（进行中，见 todo/04_tsx.md）

- ✅ 静态模板切片（`tsx.go` + `LowerTSX`）：纯静态 JSX（标签/文本/string 属性/self-closing/
  fragment）→ `.sax` Component + 空 state；hooks/事件处理器/表达式子节点/自定义组件/
  spread 全部大声拒。单测 2 项（静态形状/三类动态拒绝）。286 零回退（独立入口）。

### Phase 3：npm 通道（进行中，见 todo/03_npm.md）

- ✅ 白名单首轮实测（单文件口径，`npm pack` 实源）：lodash-es 5.4%（35/643）、
  date-fns 12.4%（259/2093）；零 panic。主因：跨文件 import（未走链接）+ `.js` 无注解
  （未配 `.d.ts`）+ 顶层 require。结论：单文件口径系统性低估，不触发收缩线；
  下一步 program 口径 + `.d.ts` 配对后再判定（已记入 todo/03）。
- ✅ 鲁棒性：lodash-es 644 文件 hostile 语料零 panic（checker 异常一律回退语法 lowering；
  拒绝全是定位 diagnostic）。

### 全 TS 特性收敛（进行中，目标：零借口、无残缺）

- ✅ 类单继承（`class_heritage.go` 模块 + `saemit.go` 薄钩子 12 处）：
  `extends` 布局拍扁（父偏移不变、子段追加；重声明同宽复用偏移、异宽大声拒）+
  方法/getter/setter/静态继承（子覆盖）+ 缺省派生构造（继承基构造节点）+
  `super(n)` 构造委托（经外层调用点映射，字面量/形参直通、余者大声拒；无基构造时求值丢弃）+
  `super.m()`/`super.f` 路由（拍扁偏移等价，getter 保持专属拒）+
  `implements` 类型擦除 + 接口 `extends` 布局拍扁 + `abstract` 精确拒 `new`；
  动态基/未知基/环/缺 `super()`/非法 `super` 一律大声拒。
  286 单文件扫测零回退（基线 worktree 逐字节一致，refused 0/0）；
  单测 9 项（7 新 + 2 旧转正：extends 空子类、heritage 静态跨文件）；
  真机 `sa check` 过（103/24 指令），手工对数 5==5（Dog(4,1).total()）。
  缺口政策：新增 std 原语一律立项到 sci/sa_std（禁 tsgo 原创运行时）。
- ✅ 命名空间（`namespace_ts.go` 模块 + 薄钩子 16 处）：
  环境块整体擦除（`declare module`/`declare global`/`declare namespace`/
  `export as namespace`，planck src 66 文件 ModuleDeclaration 诊断 22→0）+
  运行时拍扁（`N.f`→`N_f`，嵌套 `A.B.g` 最长匹配；函数/箭头/纯量 const/类/
  枚举/接口/别名全成员；`NS.f()`/`NS.C`/`new NS.C()`/`NS.E.M` 全路由；
  `export` 私有性 + 值遮蔽优先 + 重定义/合并大声拒）+
  修出内联所有权缺口（`releaseDeeperThan`：方法/回调 join 点清理，顶层
  `lowerReturn` 语义对齐；276 扫测输出提前释放 `!a !b`，check 放行）。
  286 单文件扫测 285 逐字节一致 + 1 改进；单测 8 项；真机 `sa check` 过，
  手工对数 47==47。后续切片：可变命名空间状态（并入模块状态）、合并重开、
  import-equals、跨文件成员。
- ✅ 顶层可变模块状态（`modstate.go` 模块 + 薄钩子 20 处；sci 先行
  `8c07ecd8`：`modstate.sai` + TLS 注册表值语义 `get/set_u64`，Zig 5/5）：
  标量 `let`/`var`（i32/i64/u64/f64，bool 按 i32）经注册表槽共享跨函数状态，
  零值免 init（注册表零填）、非零字面量第二 flag 槽 use-site 懒 init 一次
  （`??` join 形）；赋值全文件预扫，被赋值的名永不进 constVals 折叠——修出
  静默误编译（counter：`sa=1` vs `node=2`，check 曾放行）；命名空间
  `export let` 同解（拍扁名 + `checkNsAccess`，单层/嵌套读写、`+=`/`++` 全路由）；
  串/数组/effectful 初始化/跨文件变量/const 重赋一律大声拒（旧命名空间
  `export let` 拒测转正 1 项）；声明位走 `assignLocal`（遮蔽不再摘全局折叠，
  旧折叠误伤顺手修）；`@const` 可写与指针穿越两条死路已探否决
  （AOT 只读/`Locked_Mut`），`panic(1403)`  沿 14xx 分配失败族。
  286 单文件扫测 286 逐字节一致 + `sa check` 286 全过；单测 10 项（拒测含 5 子项）；
  新形状 `sa check` 全过（计数器/命名空间/宽/循环/分支/program 前缀隔离 4 键）；
  Zig 胶水以真实发射 key 驱动计数器 0→1→2 与懒 init 协议 2/2。
  JEV blast-radius local_only 97%。
  缺口序列：串/对象模块状态（双槽 + 生命周期）、effectful 初始化、
  顶层可执行语句入口、 bare `panic` 8 处潜雷（本二进制拒，286 未触及）。
- ✅ 串模块状态（`modstate.go` 扩展 + 薄钩子 10 处；零 sci 变更，复用既有
  `get/set_u64` 对）：串槽 = 双 u64 槽（ptr+len）+ flag 槽（新 FNV 域，标量键
  不动）；写仅字面量/折叠串 const（`@const` 永生中转、header 用后释放；
  `=` 点分前后路由、计算串经 `modWiden` 大声拒）；读经双 get 物化 header
  （`modStrTmps` 标记供类内联/回调别名，防半拷贝）；方法经 `lowerStringMethod`
  直达（防 `indexOf` 等数组名抢占）；`++/--` 拒；数组字面量/push 仅对标记
  临时量拒（纯字面量数组放行保 `Buffer.concat`）；`typeof` 报 string；
  串恒走 flag 初始化（空串亦显式，防 extern 空指针触碰）；闭包捕获天然直通
  槽（`lookupBinding` 守卫已排除无 scope 名，箭头实测同键）；旧串拒测转正 1 项。
  探否决 `tString == tArray == "ptr"` 类型判等（改节点/名源精确判定）。
  286 单文件扫测 286 逐字节一致 + `sa check` 286 全过；单测 6 新串测 + 4 拒子项；
  新形状 `sa check` 全过（串读写/方法/命名空间串/类参/`typeof`/拒形）；
  JEV consistency 4/4 先行、blast-radius local_only 96%。
  缺口序列：对象模块状态、计算串累加（缺泄漏豁免）、串数组、effectful 初始化、
  顶层可执行语句入口、命名空间合并重开、import-equals、跨文件成员。
- ✅ 裸 panic 带码（8 处 + 命名常量；零 sci 变更）：本二进制拒裸 `panic`
  （`panic_op` 需字面量），既有 8 处发射（`throw`、DOM 满、node/deno/sa_std
  状态检查 6 处）产物恒不可组装。`throw`→`panic(2501)`、DOM 满→`2502`、
  状态检查→`2503`；25xx 为 satsgo 发射块（sci 12xx-22xx 已占，
  1403 注册表 OOM 沿用）。单测 `TestCodedPanics` 4 子项断码 + 无裸 panic；
  旧 panic 子串断言全过；286 sweep 零回退；throw 与 `Date.parse` 产物现过
  `sa check`（此前必挂）。JEV blast-radius safe_to_apply 77%。
- ✅ 顶层入口合成 D1（`entry_top.go` 模块 + 薄钩子 8 处）：运行时调首个
  `@main`，裸顶层语句（`main();`/`console.log`/控制流）此前内联进 body
  恒 fallthrough。两遍 lowering：定义先行，可执行语句按序进合成
  `@main() -> i32`（finish 时前插；无执行文件 entryBuf 空，布局不变）；
  用户 `main` 碰撞改名 `main__user`（预扫签名搬移 + 陈旧键删除 + dtsRet
  拷贝、定义点、两调用点 importEnv 后映射防误伤导入、typeof）；出口
  `return 0`（TS 完成值非 exit 码）；`main__user` 字面碰撞大声拒。
  附带修出键溢出：解释器按 i64 解析立即数，全范围 u64 键半数 Overflow，
  槽键掩到 63 位（注册表收任意 u64；单测锁定）。
  单测 6 新 entry 测 + 1 键范围测；286 sweep 逐字节一致 + check 全过；
  entry 真机 check 过、纯 entry `sa run` exit 0。
  JEV blast-radius safe_to_apply 69%。后续 D2：program 入口同路 +
  非入口顶层可执行拒 + `export main` 碰撞处置。
- ✅ 顶层入口合成 D2（纯测试交付，零产品代码变更）：入口文件走 D1 共享
  `lowerSourceFile`（prefix "" 前缀无关），非入口顶层可执行走 `planEntry`
  拒（link + prefix 判定）。3 个 `LowerProgram` 门禁测：入口合成形状
  （`@main` + `@main__user` + 前缀被调）、非入口拒文、零执行零改名。
  `export main` 处置结论：可链接程序中入口 main 被导入当且仅当成环
  （环检测/可达性/拒三重覆盖，错目标不可达）；相邻发现星号重导出同文件
  调用错位（`mid__add` vs `lib__add` 定义）系既有 linker 缺口——基线同现、
  与改名无关、`sa check` 响亮，记序列缺口另立项（linker 手术，不混入 entry）。
  全套件绿、286 sweep 零回退、程序真机 check 过。
  JEV blast-radius local_only 100%。
- ✅ 星号重导出同文件调用错位（`program.go` 3 处）：`export *`/`export-from`
  不引入本地绑定，但星拷贝进 `globalRets` 后被播种成 localDefs，同文件调用
  走本文件前缀（`@mid__add` vs `@lib__add` 定义），产物恒 check-trap。
  修：`fileExports.starProvided` 域 + resolve 星分支成功标记（直定义/命名
  重导出早返故精确）+ 播种 localDefs 排除重导出名；funcSigs/arity/qual 不动，
  错位调用落既有 import-it-first 大声拒（TS 语义：重导出名须先 import）。
  单测 1 项（星 + 命名双拒形）；全套件绿、286 sweep 零回退；
  planck program 口径 56 文件两边一致、诊断 519→491（早拒收敛级联噪声，
  零新增坏点）。JEV consistency 4/4 先行、blast-radius safe_to_apply 86%。
- ✅ 装饰器/`using` 静默吞转大声拒（soundness 修补）：类/成员/参数装饰器
  此前直接丢弃（定义时效应丢失），`using` 按普通声明 lowering（dispose
  丢失）。类位/成员位（`recordClass` 头）/变量声明位（函数体 + 顶层，
  `NodeFlagsUsing` 覆盖 `await using`）三处设防；`modClaim` 排除 using
  防槽认领掩盖诊断。单测 1 项（5 拒形）；全套件绿、286 零回退。
- ✅ 逻辑复合赋值 `&&=`/`||=`/`??=`（真短路 lowering）：RHS 只在 assign
  臂求值（既有 `and`/`or` 值运算是 eager 的，此处不沿用），双臂经 join 槽
  （`??` 形，alloc 在 br 前）；目标仿 `=`（标识符 + 命名空间成员，含模块
  槽与串槽字面量分发）；串测 len（空串 falsy，纠正 header 指针恒真）、
  `??=` 指针测、`f64` 用 `fcmp`；join 值须物化寄存器（命名空间内裸名不定
  寄存器、立即数不可存，`snapImm` 统一快照）；预扫 `assignedNames` 计入
  （否则模块名先折叠后拒）。途中抓到 assign 臂极性反转（`||=` 测真进 skip）
  与发射 fallthrough（alloc 在 br 后），真机对数锁定。
  单测 1 项（三臂形状 + 单次调用 + 串 len + 模块槽 + 坏目标拒）；
  全套件绿、286 sweep 零回退 + check 全过；  真机 `sa run` 六形 31 对数。
  JEV blast-radius local_only 98%。
- ✅ linkRoute 值分支按导出态分流（`program.go` + 注释，`program_test.go` 1 行同步）：
  exported 字面量 const miss 报 `import it first`（值导入已落地，缺的是 import
  不是后端）；未导出/let/模板/对象仍诚实缺口。`TestLinkRouteMisses` const 期望同步；
  全套件绿、286 sweep 零回退 + check 286 全过、差分 259+27 基线吻合。
- ✅ 形状校验器同步带码 panic（`tools/check_sai_shape.py` 1 行，零发射器变更）：
  `panic` 终结符接受 `panic(<code>)`（2501/2502/2503/1403），与裸 panic 带码
  发射对齐；`panic(foo)` 非数字仍拒。python 双向验证；286 sweep 形状全过。
- ✅ linkRoute enum 回归锁（`program_test.go` 纯单测，零产品代码变更）：
  reachable 全整数 enum 成员经 sharedEnums 直接折叠、无需 import（实测证伪
  loop1-kind 设想，按删无可删回滚；JEV 回滚裁决）；unreachable enum miss 走
  loop2 value 分支报 `import it first`（上一轮分流自动覆盖）。单测锁定该形，全套件绿。
- ✅ linkRoute 串枚举诚实缺口（`program.go` loop2 诊断索引 6 行 + `program_test.go` lib/用例）：
  unreachable 诊断索引曾把所有具名 enum 记为 value，串/计算枚举 miss 经上一轮
  分流误报 `import it first`（import 也救不了，违拒则大声）。现 kind 仅当
  `enumMemberTable(st, true)` 成功才记，全整数行为不变；串枚举回落通用子集拒
  （单测锁定 `property access .X is not in the SA-lowerable subset`）。
  全套件绿（JEV 排序 #1 + blast-radius local_only）。
- ✅ 跨文件多级继承 super 链（`program.go` sharedClassParent 4 行 + `program_test.go` 三文件单测）：
  `classParent` 曾是单 emitter 私有映射，进口商侧多级链断裂（mid 的 `C→B` 在
  main 不可见，`new D` 报误导性 super 拒；单文件恒过）。现与 classDefs 同形
  共享（两处 prescan + lowering 接入；单文件 nil 分支不动；环检测顺带跨文件化）。
  探针 base→mid→main 缺省派生构造打通，真机 check 过 + `sa run` 7 与 node 差分一致。
  286 sweep 零回退 + check 286 全过；全套件绿（JEV blast-radius local_only）。
- ✅ 命名空间声明跨文件调用（`link_namespace.go` 新模块 + `program.go` 记录/装配 + `saemit.go` 具名分支拦截）：
  `import { N }` 按成员绑定（`N→f→N_f`，复用 `nsImports` 调用/解构/跨文件根三位
  读者，零新路由代码）；命名空间本身非值，未知成员与非 callable 成员大声拒。
  探针 `N.f(41)` 真机 check 过 + `sa run` 42 与 node 差分一致；单测 3 形；
  286 sweep 零回退 + check 286 全过；全套件绿。
- ✅ 嵌套命名空间跨文件调用（`link_namespace.go` 嵌套键绑定 + `routeNestedNSCall` 两级接收路由）：
  `import { N }` 后 `N.M.g()` 经虚线键（`N.M.g→N_M_g`）与 Deno 链后的并联分支
  路由；根非导入命名空间原样穿透，深路径未知大声拒。探针真机 42 与 node 一致；
  单测贯通 + 未知深成员拒 2 形；286 sweep 零回退 + check 286 全过；全套件绿。
- ✅ 命名空间 const 跨文件折叠（`nsConsts/nsConstStr` 双表 + 绑定虚线键 + 读位折叠）：
  `import { N }` 后 `N.K` 按字面量折叠（数/布/串，照抄同文件读位三臂）；
  let 与计算初值永不进表，大声依旧。途中抓到具名分支 `continue` 跳过 const
  绑定（绑定前移修复）。探针 `N.f(N.K)` 真机 14 与 node 一致；单测数/串 const
  贯通 2 形；286 sweep 零回退 + check 286 全过；全套件绿。
- ✅ 命名空间类跨文件实例化（`lowerNew` 进口商分支 + `varClass` 回退 + 构造位布局记录）：
  `import { N }` 后 `new N.C()` 经共享表布局直通（同文件影子护栏保留）；
  构造位同步记录实例布局，checker 失明的跨文件实例读位不再悬空（同类
  顶层实例行为不变）。探针 `c.v` 真机 41 与 node 一致；单测贯通 + 未知类拒；
  286 sweep 零回退 + check 286 全过；saemit 全套件绿（`fswatch` 系容器无
  fanotify 环境项，零交集）。
- ✅ linkRoute 一阶值诚实文案（`program.go` 顶部 function+已绑定分支 + 单测双断言）：
  已在手（own/localDefs 或 import）的函数名作值用时，不再劝无用的 import，
  直说无一阶值并指路直接调用（措辞对齐 Math 别名拒）。既有 linkRoute 单测
  全用未导入名，零触碰；286 sweep 零回退 + check 286 全过；全套件绿。
- ✅ 尖括号断言擦除（`<T>x` 与 `as` 同形；`satisfies` 早已擦除）：
  `lowerExpr` + `staticLiteralText` 双侧加 `KindTypeAssertionExpression`
  （静态折叠同步，`static K = <number>7` 照折）。
  单测 1 项（值/对象/satisfies 擦除 + 静态折叠）；全套件绿、286 零回退；
  真机 `sa run` exit 41 对数。
- ✅ 对象展开/字面量计算键/简写（`lowerObjectLiteral` 重写）：
  spread 源先求值（布局经 `layoutOfVar`，无布局大声拒），字面量计算键
  折叠为静态名（`{["x"]: 1}`），简写读绑定（`{y}`）；键集合匹配布局
  （覆盖去重，目标失配沿用旧拒文）；按源序重放（spread 逐字段布局引导
  拷贝 + 类型相符检查，赋值覆盖在后者胜）；动态键/类型错位大声拒。
  `layoutOfLiteral` 纯检测位不动（trackBinding/解构零行为变更）。
  单测 1 项（成功 + 字面键 + 简写 + 3 拒形）；全套件绿、286 零回退 +
  check 全过；真机 `sa run` 20/1 双对数（覆盖与被覆盖序）。
  JEV blast-radius local_only 95%。
- ✅ 标签 break/continue（`labels.go` 模块 + 薄钩子 8 处）：label 表 +
  pending 栈，循环/switch 经 push helpers 绑定（`a: b: for` 双绑），块标签
  直绑 break-only；标签随语句死亡（顺序复用合法、嵌套同名拒）；函数边界
  隔离（arrow/entry 存换）；未定义/块 continue/标错语句皆大声拒。
  附带修无标签 `continue` 在 C-for 跳过 incrementor 致挂（cont 改指
  incrementor；body 预扫使无 continue 文件零增量；终止体经 contJumps 保
  incrementor；此前 286 无 continue 覆盖故一直潜伏）。
  单测 1 项（3 形状 + 5 拒形 + 顺序复用）；全套件绿、286 sweep 零 diff +
  check 全过；真机 `sa run` 六对数（3/1/7/10/4/2，含无标签 continue 修后值）。
  JEV blast-radius local_only 99%。
- ✅ 类/函数表达式（`recordClassNamed` kind 无关化 + `lowerClassExpression` +
  `tryTopLevelClassExpr` + `lowerArrowBinding` 泛化 + program 导出扫描补函数
  表达式）：函数表达式共享 arrow 别名路径（含顶层）；类表达式匿名记 bound
  名、具名记 own 名 + bound 别名（`new`/静态同解）；表达式 heritage 拒
  （声明形 machinery）；program 侧函数表达式与 arrow 同等出口
  （`import { Distance }` 贯通）。已知缺口如实锁定：实例字段初始化缺失
  （既有，旧测试 `= 0` 掩盖）、内名外泄（值正确，可见性宽）。
  单测 1 项（成功 + 具名 + 顶层双形 + heritage 拒 + 内名锁定）；
  类系 18 项回归全绿；286 sweep 零回退 + check 全过；真机 `sa run` 44 对数；
  planck program 口径拒文件 49→48（`util/Timer` 修好，顶层 let + 展开），
  零 base-干净文件新增诊断（171 条新增全为既有伞拒打开后的真缺口，同属
  既有大声家族；28 处消除）。
  JEV blast-radius safe_to_apply 86%（首审 needs_regression_tests 52%，
  planck 门禁加固后翻转）。
- ✅ 私有字段 `#x`（owner 改名 + 词法归属）：`#x` 按属主改名（`#C#x`，
  遮蔽天然分槽）；`methodOwner` 记录定义类（继承方法保 base，与 `ctorOwner`
  同例；`curMethodClass`/super 语义不动）；读经 memberChain、写经
  fieldStore、构造器经 wiring owner、静态经折叠位、`in` 作品牌检查；
  super 下/方法外/他类/未声明皆大声拒。
  单测 1 项（读写/品牌/私有静态 + 4 拒形）；类系 18 项回归绿；
  全套件绿、286 sweep 零回退 + check 全过；真机 `sa run` 15/33 双对数
  （含继承遮蔽：base 方法读 base 槽、子类方法读子类槽）；planck 零移动。
  JEV blast-radius safe_to_apply 89%。
- ✅ Tagged 模板（`String.raw` 煮无 + 他 tag 大声拒）：raw 段 +
  正常插值渲染（复用 `concatSlices`/`renderInterpValue`）；NoSub 的
  RawText 解析器留空，改源码字节切片（位置字节性以多字节前缀实证，
  取不到则大声拒；熟文本永不可信，每个转义都变长度）；他 tag 大声拒
  （缺 strings 数组）。单测 3 项（形状 + 拒形 + 转义锁定 + 多字节切片）；
  全套件绿、286 sweep 零回退 + check 全过；真机 `sa run` 纯形对数，
  unicode 与子集字节语义一致（子集长度即字节，既有语义）。
  JEV blast-radius safe_to_apply 78%（首审 24%，切片回归加固后翻转）。
- ✅ 命名空间合并重开（延迟 lowering + 全量 prescan 记录）：重开合并
  （scope 注册一次）；成员延迟到 pending 队列（定义遍后排空 + 各顶层语句
  前 eager 排空保源序，发射序无关 @label）；节点身份去重（同节点重预扫
  幂等，不同节点同名拒）；类型空间/const 折叠/let 槽/嵌套名/类/布局/枚举
  全量 prescan 记录（跨 body 前向引用；函数/箭头体 drain 期发射保后见签名）。
  旧 merging 拒测转正。单测 1 项（合并 + 跨 body const/let 读 + 双拒形 +
  嵌套合并）；全套件绿、286 sweep 零回退 + check 全过；真机 `sa run` 41；
  planck 零移动（零 demo 用命名空间，48 文件、零干净新增）。
  JEV blast-radius safe_to_apply 89%。
- ✅ import-equals 别名（`lowerImportEquals` + `qualify` 单 choke +
  调用/读两接收分支）：成员别名经 qualify 全用位路由（调用/读/槽/
  类/enum/typeof/继承基，15 处调用点全核为用位）；命名空间别名改
  调用与读的接收分支（遮蔽优先）；全文件 ns prescan 使别名可在 ns
  之前声明；成员存在性/私有性 import 期校验；跨文件/require/未知/
  私有/重复/碰撞大声拒。旧 import-equals 拒测转正。
  单测 1 项（全品类 + 命名空间 + 前向 + 5 拒形）；全套件绿、
  286 sweep 零回退 + check 全过；真机 `sa check` 过；planck 零移动
  （无 import-equals 用例）。
  JEV blast-radius safe_to_apply 93%。
- ✅ 对象模块状态（标量字段逐槽化，零堆持久化）：每字段独立 u64 槽 +
  对象 flag 槽（FNV 域隔离）；布局按注解优先、字面量匹配兜底；缺字段
  零填；串/句柄/嵌套/spread/简写/计算键大声拒；读按布局物化 header
  （`varLayouts` 标记，裸读免费经 `layoutOfVar`）；写直存字段槽
  （整对象字面量分发，链式禁防静默丢）；`N.obj` 整读写 + `N.obj.x`
  全路由（隐私同成员）；`o.x++`、`typeof object`、解构、`in` 复用既有位。
  途中堵住遮蔽洞（局部同名标量参数曾误读槽布局，三处 scope 优先守卫
  + 回归锁定）。单测 5 项（10 断言）；全套件绿、286 sweep 零回退 +
  check 全过；Zig 胶水真实 key 驱动 14/25；混合程序 382 指令 check；
  planck 零移动（48 文件、零干净新增）。
  JEV blast-radius safe_to_apply 89%（首审 44%，共享路径审计 + 遮蔽
  回归加固后翻转）。
- ✅ 跨文件 miss 路由（`linkRoute` 品类诚实文案 + 全 miss 位接线）：
  修双 prescan 重复诊断（prescan 收归一处）；`linkSeeded` 豁免自碰撞
  （`directTopFuncs` 保真碰撞仍拒——从 ns 文件 import 曾整文件误拒）；
  `linkExports` 扩品类（function/class/namespace/value）+ unreachable
  文件诊断索引（确定性排序，只记名不 lowering）；miss 位全接线
  （调用/读/new/extends/点调用/点读，遮蔽优先，单文件零行为变更）。
  文案按可 import 性分品类（函数/类指 import，不可者明说缺口；未导出
  指补 export）。转正程序按 feature 级验证（`sa check` + `sa run` +
  node 对数：call/new/star/类读全对）。
  单测 2 项（13 子断言：6 miss 品类 + 自碰撞正反）；全套件绿、
  286 sweep 零回退 + check 全过；planck 拒文件 49→48、零干净新增
  （新增诊断全在已拒文件，品类全属既有大声家族）。
   JEV consistency 首审曾 flag 转正风险（66%），按 feature 级验证消化；
   blast-radius safe_to_apply 84%。
- ✅ linkRoute 命名空间诚实文案（`program.go` namespace 分支 + 调用/new 双侧 dottedRoot）：
  `N.f/N.K/new N.C/N.M.g` 在 `import { N }` 后均已打通，裸 miss 不再报后端缺口，
  改指 import。调用侧补嵌套链 dottedRoot（`N.M.g`），new 侧补 dottedRoot 回退
  （`new N.C`）；同文件值遮蔽优先，单文件（link==nil）零行为变更。
  sla 对照：try 为后缀 `?` 传播（无异常边，TS try/catch 保持 panic 语义），
  async 为状态机（satsgo await 保持同步 unwrap，不复刻）。
  单测 `TestLinkRouteNamespaceMembers` 3 形 + 既有期望更新；全套件绿。
