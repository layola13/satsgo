# 阶段 4：tsx / React

## 前提（硬依赖）

阶段 1（Program 链接）必须先完成——tsx 多文件不进来，一切免谈。

## 现状

`sa react` 只吃 `.sax`（产出 `app.wasm + airlock.js + index.html`），不认识 tsx。
目标端已有 state 槽 + `@onMount/@onUpdate/@onUnmount`，这是 hooks 落点的基础。

## 任务

### 路一：tsx → SAX 资源（先行）

1. [x] tsgo 解析 tsx（ScriptKindTSX，JSX AST 直读）（JSX 节点现成），新 emitter 把 JSX 脱糖成 `.sax` 组件源。
2. [x] 语义子集：静态模板切片落地（`tsx.go` + `LowerTSX`）；动态（hooks/处理器/表达式/组合/spread）大声拒props/state 初始化、条件渲染、列表渲染；其余（spread props、复杂 children 透传）逐个关或拒。
3. [x] 复用整条 `sa react build`（sci 源码编 + `libreact.so` 自建，真机闭环）：
   satsgo counter（`useState(0)` + `{count}` + `setCount(count±1)` 双钮）→
   `react check` 通过 → `react build` 出 `app.wasm + airlock.js + index.html + app.sa`。
   途中修真缺口：有 state 组件须 `!var` 释放行（消费者 SaxStateLeak 规则，
   fixture `!count !last` 即范式），发射器已补、契约已锁（前注记“释放行缺席”系
   无真机时的误判，已勘误）。fixture `react_counter.sax` 同链复建通过；
   Chromium verifier 容器无浏览器，条件不触发。
4. [x] 交付：counter 级组件 `sa react build` 跑通（check + build + 全产物；见上）。

### 路二：hooks / DOM（子集推进）

5. [x] `useState` → SAX state slot（`const [x, setX] = useState(数字/布尔字面量)`，模板 `{x}` 插值；字符串初值/setter 使用/计算表达式/杂语句大声拒；单测 1 项；286 零回退）。
6. [x] 挂载期 `useEffect` → `@onMount`；其余 hooks（deps/cleanup）gate 拒绝。
   （`useEffect(fn, [])`：fn 无参无返回，体仅 `setX(整数/布尔字面量)`，
   直存 state 槽 + 一次 render；空体出裸块；非空 deps/缺 deps/cleanup/
   其他 hooks/计算参数大声拒；单测 1 项；286 零回退。）
7. [x] DOM 投影表（`createElement/appendChild/setAttribute` → airlock extern），相当于给浏览器环境再做一套投影。
   （p1：三件套 + i64 句柄追踪；query/text/attrs 系与 p3 解构另立项；extern 由构建侧提供；单测 1 项；286 零回退。）
   （p2：createTextNode + textContent/innerHTML 写；读另立项；单测 1 项。）
8. [ ] JSX → 直接 SA 调用（`createElement` 内联），绕过 `.sax` 中间态（可选优化，不阻塞）。

## 交付数字

- 路一：无 hooks 组件端到端跑通。
- 路二：`useState` + 挂载 `useEffect` demo 通过；超子集用例明确拒绝。

## 风险

- 调度语义（重渲染时机、effect 时序）是深坑，严格限制子集，不做“看起来能跑”的半吊子。
- 路二工作量不小于当初的 std 投影表，单独排期。
