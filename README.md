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
- [x] 审批点（风险分级 L0-L3 + 参数感知 + 模型自动判断风险 LLMRiskEvaluator（只升不降）+ fail-closed 默认拒绝 + 拒绝回填 + 连续拒绝升级中止）— `internal/agent/approval.go`、`internal/agent/risk.go`
- [x] 真实模型接入（OpenAI 兼容 Provider：智谱 BigModel / DeepSeek / 火山 Ark 通用，tool_call id 关联 + 结构化 tool_calls 协议适配）— `internal/agent/provider.go`
- [ ] 评测门（任务成功率、结构化判断）

## 当前进度（2026-09-30）

第一版已跑通：`go run . -demo normal | loop | soft | compact | approval | real | real-approval` 七个场景分别演示
「正常链路 / 循环检测中止 / 步数软停止 / 摘要压缩 / 审批点 / 真实模型 / 真实审批」，
前五个用 `MockLLM`（脚本化假模型）不依赖 API Key，后两个走真实智谱 GLM
（OpenAI 兼容，需 `ZHIPU_API_KEY`，写入项目根 `.env`，已被 `.gitignore` 排除）。
撞上限与循环命中均为"软停止"（返回带原因的 RunResult），
不是 error——对齐生产 Agent 的"交还用户"语义。审批点采用风险分级：工具声明基础风险
（`BaseRisk` L0-L3），风险由 `RiskEvaluator` 判定——静态/函数（参数感知）/模型
（`LLMRiskEvaluator` 用独立 LLM 判断，只升不降）；低于审批阈值自动放行、达到阈值
进闸门；未配置 approver 时默认拒绝（fail-closed）；连续拒绝达上限升级中止。

**真实接入踩坑记录（重要）**：
- `glm-4.7-flash` 是混合思考模型：reasoning 会先吃掉输出预算，`max_tokens` 太小导致
  `content` 为空——风险判断这类"只输出级别名"的任务会解析失败，**静默退回基础风险
  造成高风险动作漏审**。修复两层：① 风险判断 Provider 给足 `MaxTokens`（2048）；
  ② `LLMRiskEvaluator` 解析失败不再退回基础风险，改为保守升级
  `maxRisk(base, MEDIUM)`（宁多送审、勿漏审——不对称风险论）。
- 真实模型判断存在输出方差：同一请求在不同运行可能给出不同级别，生产应换
  专用小模型/分类器 + 确定性输出约束（JSON mode / 低温度 / 多次采样）。

token 统计为**估算口径**（CJK 1 字 ≈ 1 token、其余 4 字符 ≈ 1 token，含局限说明见
`internal/agent/context.go` 注释），单元测试见 `internal/agent/context_test.go`、
`internal/agent/approval_test.go`、`internal/agent/provider_test.go`（`go test ./...` 可跑）。

## 约定

- 不自动提交、不自动推送，按明确指示执行 git 操作。
- 代码注释统一标注 JS/TS ↔ Go 差异点（如错误处理、并发模型、类型系统）。
