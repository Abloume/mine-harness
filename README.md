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
- [x] 上下文管理（token 预算、FIFO 截断）— `internal/agent/context.go`（摘要压缩待升级）
- [ ] 审批点（人工确认/自动放行）
- [ ] 评测门（任务成功率、结构化判断）

## 当前进度（2026-09-30）

第一版已跑通：`go run .` 输出「工具调用 → 回填 → 最终回答」完整链路，
模型层用 `MockLLM`（脚本化假模型），不依赖真实 API Key。

## 约定

- 不自动提交、不自动推送，按明确指示执行 git 操作。
- 代码注释统一标注 JS/TS ↔ Go 差异点（如错误处理、并发模型、类型系统）。
