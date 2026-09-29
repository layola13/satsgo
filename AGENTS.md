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
