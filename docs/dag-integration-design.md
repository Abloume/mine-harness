# DAG 执行器 × mine-harness 内核集成设计

## 一、定位：不是替代 ReAct，是第二种 Agent 模式

| 模式 | 适合任务 | 特点 |
|---|---|---|
| **ReAct**（现有 loop.go） | 短链路、不确定、需要动态调整 | 走一步看一步，每步都要模型推理 |
| **Plan-and-Execute**（dag 包） | 长链路、步骤清晰、可并行 | 先规划完整 DAG，再按层并行执行 |

两种模式并列，不是谁替代谁。Agent 构造时选模式，或者根据 Skill 类型自动选。

---

## 二、四层集成路径

### 第 1 层：DAG 执行器（已完成）

```
internal/dag/
  types.go  — Graph / Node / Edge / Executor
  - 拓扑排序（Kahn 算法）
  - 按层并行调度（goroutine + 信号量限流）
  - 节点状态机（pending → running → success/failed/skipped）
  - 失败隔离 + 级联传播
```

**现状**：能跑、能并行、能隔离失败，但没有接 LLM 做规划——图是手写的。

---

### 第 2 层：Planner——让 LLM 输出结构化 DAG

**目标**：从"手写 DAG"进化到"模型自动出计划"。

```
用户任务
    ↓
Planner（LLM）：读任务 + 可用工具列表
    ↓
输出 JSON：{ nodes: [...], edges: [...] }
    ↓
Executor：拓扑排序 → 按层执行
```

**关键设计**：
- Prompt 里要求模型输出严格 JSON（nodes + edges）
- 执行器先 Validate（环检测），有环就让模型重规划
- 节点粒度：一个节点 = 一次工具调用

**跟现有代码的关系**：
- 复用 `agent.LLM` 接口（Chat 方法）
- 复用 `agent.Tool` 注册表（工具名 + 参数）
- Planner 本身就是一个特殊的"工具"——输入任务，输出 DAG JSON

---

### 第 3 层：Agent 模式选择

**目标**：Agent 构造时可以选 ReAct 还是 Plan-and-Execute。

```go
// 现有：ReAct 模式（默认）
a := agent.NewAgent(llm, reg)

// 新增：Plan-and-Execute 模式
a := agent.NewAgent(llm, reg, agent.WithPlanExecuteMode())
```

**内部实现**：
- Agent 结构体加一个 `mode` 字段（react / planExecute）
- `Run()` 方法根据 mode 分发：
  - react → 现有 loop
  - planExecute → 调 dag.Executor.Run()

**跟 Skill 系统的集成**：
- Skill 可以声明 `execution_mode: dag` 或 `execution_mode: react`
- Agent 加载 Skill 时自动选模式
- 比如 `fix-bug` Skill 是 ReAct 型（需要动态定位），`self-evolve` 是 DAG 型（步骤固定）

---

### 第 4 层：Replan——局部重规划

**目标**：节点失败时，不是全盘重来，而是只改受影响的子图。

```
节点失败（重试无用）
    ↓
锁定已成功的节点（不许改）
    ↓
摘出失败节点 + 它的所有后继
    ↓
Planner：带原始目标 + 已完成结果 + 失败原因
    ↓
输出新的子图
    ↓
合并回原 DAG 继续执行
```

**护栏**：
- Replan 上限 3 次，超限终止
- 已成功的节点不许动（防止回归）

---

## 三、跟现有包的关系

| 现有包 | DAG 集成方式 |
|---|---|
| `internal/agent/loop.go` | ReAct 主循环，跟 DAG 并列，不互相替代 |
| `internal/agent/tools.go` | DAG 节点的执行函数直接复用 agent.Tool.Execute |
| `internal/agent/skill.go` | Skill 可声明执行模式（dag / react） |
| `internal/evolve` | 自进化闭环本身就是 DAG 型任务（分析→诊断→改写） |
| `internal/eval` | 评测门可以用 DAG 并行跑多个 Case |

---

## 四、下一步迭代优先级

1. **Planner**：让 LLM 输出结构化 DAG JSON（最有价值）
2. **Agent 模式选项**：WithPlanExecuteMode() 构造函数
3. **Replan 机制**：失败后局部重规划
4. **跟 Skill 集成**：Skill 声明执行模式

---

## 五、为什么不是现在就全做

这是学习项目，不是生产框架。每一层都要配套：
- 单元测试（环检测、拓扑排序、失败传播）
- demo 场景（`go run . -demo dag`）
- JS/TS ↔ Go 差异注释

一次做一层，每层验证通过再往下走——跟自进化闭环的"最小修改原则"一个道理。
