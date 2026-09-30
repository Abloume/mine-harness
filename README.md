# mine-harness

最小 Agent 运行时内核（Go）学习项目。

## 定位

- **目的**：通过手写"最小内核"理解 Agent 运行时机制——agent loop、工具注册、上下文管理、审批点、评测门。
- **边界**：学习/练习项目，不是生产框架。生产场景请使用成熟运行时（Agent Framework / LangGraph / Agent SDK 等）。
- **迁移学习**：代码注释中标注 JS/TS → Go 的差异对比点，服务前端转 Go 的学习过程。

## 目标范围

第一版目标：几百行内跑通一条最小链路

```
调模型 → 解析工具调用 → 执行工具 → 回填结果 → 再调模型（直到结束）
```

规划的能力（按学习顺序）：

- [x] agent loop（ReAct 风格主循环）— `internal/agent/loop.go`
- [x] 工具注册与调用（function calling / 本地工具）— `internal/agent/tools.go`
- [x] 上下文管理（token 预算 + 摘要压缩 + FIFO 兜底）— `internal/agent/context.go`（摘要器可替换为生成式 LLM 摘要）
- [x] 循环检测 + 软停止（精确重复 N=2、滑动窗口 8 步、失败重试预算 3、分级响应）— `internal/agent/loop.go`
- [x] 审批点（human-in-the-loop：Approver 可插拔决策器、fail-closed 默认拒绝、拒绝回填让模型换路径）— `internal/agent/approval.go`
- [ ] 评测门（任务成功率、结构化判断）

## 当前进度（2026-09-30）

第一版已跑通：`go run . -demo normal | loop | soft | compact | approval` 五个场景分别演示
「正常链路 / 循环检测中止 / 步数软停止 / 摘要压缩 / 审批点」，模型层用 `MockLLM`（脚本化假模型），
不依赖真实 API Key。撞上限与循环命中均为"软停止"（返回带原因的 RunResult），
不是 error——对齐生产 Agent 的"交还用户"语义。审批点默认 fail-closed：
高风险工具（`RequiresApproval: true`）未配置 approver 时一律拒绝执行。

token 统计为**估算口径**（CJK 1 字 ≈ 1 token、其余 4 字符 ≈ 1 token，含局限说明见
`internal/agent/context.go` 注释），单元测试见 `internal/agent/context_test.go`、
`internal/agent/approval_test.go`（`go test ./...` 可跑）。

## 约定

- 不自动提交、不自动推送，按明确指示执行 git 操作。
- 代码注释统一标注 JS/TS ↔ Go 差异点（如错误处理、并发模型、类型系统）。
