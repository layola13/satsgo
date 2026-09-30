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
- ✅ tsx `useState`（路二任务 5，`tsx.go` 内）：前导语句仅限
  `const [x, setX] = useState(数字/布尔)`，state 块发射字面量，
  模板 `{x}` 插值；字符串初值/setter 引用/计算表达式/杂语句大声拒
  （handler 仍拒，旧单测保持）。单测 1 项（成功 + 4 拒绝）；全量单测过。
- ✅ tsx 挂载 `useEffect`（路二任务 6，`@onMount` 块）：空体出裸块，
  setter 常量体直存 state 槽 + 一次 render；非空/缺 deps、cleanup 返回、
  其他 hooks、计算参数大声拒。途中抓到 setterOf 键值反转 bug（调试打印定位）。
  单测 1 项（成功 + 空体 + 4 拒绝）；全量单测过。
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
