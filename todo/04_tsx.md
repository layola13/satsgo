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
3. [ ] 复用整条 `sa react build`（待：插件 .so 未构建，先结构单测）（airlock、事件桥、lifecycle）。
4. [ ] 交付：counter 级组件 `sa react build` 跑通（含 Chromium verifier，若有）。

### 路二：hooks / DOM（子集推进）

5. [x] `useState` → SAX state slot（`const [x, setX] = useState(数字/布尔字面量)`，模板 `{x}` 插值；字符串初值/setter 使用/计算表达式/杂语句大声拒；单测 1 项；286 零回退）。
6. [x] 挂载期 `useEffect` → `@onMount`；其余 hooks（deps/cleanup）gate 拒绝。
   （`useEffect(fn, [])`：fn 无参无返回，体仅 `setX(整数/布尔字面量)`，
   直存 state 槽 + 一次 render；空体出裸块；非空 deps/缺 deps/cleanup/
   其他 hooks/计算参数大声拒；单测 1 项；286 零回退。）
7. [x] DOM 投影表（`createElement/appendChild/setAttribute` → airlock extern），相当于给浏览器环境再做一套投影。
   （p1：三件套 + i64 句柄追踪；query/text/attrs 系与 p3 解构另立项；extern 由构建侧提供；单测 1 项；286 零回退。）
8. [ ] JSX → 直接 SA 调用（`createElement` 内联），绕过 `.sax` 中间态（可选优化，不阻塞）。

## 交付数字

- 路一：无 hooks 组件端到端跑通。
- 路二：`useState` + 挂载 `useEffect` demo 通过；超子集用例明确拒绝。

## 风险

- 调度语义（重渲染时机、effect 时序）是深坑，严格限制子集，不做“看起来能跑”的半吊子。
- 路二工作量不小于当初的 std 投影表，单独排期。
