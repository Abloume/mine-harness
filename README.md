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
- [x] 上下文管理（token 预算 + 摘要压缩 + FIFO 兜底 + **生成式 LLM 摘要器**）— `internal/agent/context.go`（`Summarizer` 接口可插拔：默认抽取式 `HeuristicSummarizer`，`LLMSummarizer` 调真实模型写摘要；**收敛护栏**——摘要不比被压批次小就回退 FIFO，防生成式摘要死循环；`-demo llm-compact` 用豆包做真实摘要）
- [x] 循环检测 + 软停止（精确重复 N=2、滑动窗口 8 步、失败重试预算 3、分级响应）— `internal/agent/loop.go`
- [x] 审批点（风险分级 L0-L3 + 参数感知 + 模型自动判断风险 LLMRiskEvaluator（只升不降）+ fail-closed 默认拒绝 + 拒绝回填 + 连续拒绝升级中止）— `internal/agent/approval.go`、`internal/agent/risk.go`
- [x] 真实模型接入（OpenAI 兼容 Provider：智谱 BigModel / DeepSeek / 火山 Ark 通用，tool_call id 关联 + 结构化 tool_calls 协议适配 + **多 tool_call 并行调用支持** + 指数退避重试）— `internal/agent/provider.go`
- [x] 评测门（任务成功率、结构化判断）— `internal/eval/eval.go`（RuleJudge 确定性规则 + LLMJudge 结构化 JSON 判断 + RunSuite 汇总，`go run . -demo eval`）
- [x] Skill 加载（A 方案：Skill 包装为工具——Description 常驻触发 + SKILL.md 正文按需读取，用 function calling 伪装渐进式披露）— `internal/agent/skill.go`、示例 `skills/example/SKILL.md`
- [x] 多 tool_call 并发执行（工具声明 `ConcurrentSafe` 才参与并行；护栏/审批保持顺序、执行失败预算集中回主 goroutine，结果按原始顺序回填；未声明或含特殊通道时整批回退顺序执行）— `internal/agent/loop.go`、`internal/agent/tools.go`、`-demo parallel`
- [x] 真渐进式 Skill（B 方案：`load_skill` 特殊通道 + harness 注入 system 消息，去重注入、协议闭合；**常驻清单**——`SkillProvider.List()` 把 name/description/tags 拼进 `load_skill` 描述，模型先"看目录"再按 name 加载正文；**L3 按需引用**——`read_skill_ref` 特殊通道按需读取 SKILL.md 引用的附属资源（references/xxx.md），必须先加载技能正文才能读引用，宿主做目录穿越防护；机制归内核、目录发现归宿主）— `internal/agent/skill.go`、宿主 `skillhost.go`、`-demo skill-b`
- [x] 豆包 API 接入（火山方舟 Ark，OpenAI 兼容——与智谱/DeepSeek 共用同一个 Provider，零协议改动）— `main.go` 的 `-demo doubao` / `-demo doubao-approval`

## 当前进度（2026-10-01）

第一版已跑通：`go run . -demo normal | loop | soft | compact | approval | real | real-approval | eval | skill | skill-b | parallel | llm-compact | doubao | doubao-approval` 十四个场景分别演示
「正常链路 / 循环检测中止 / 步数软停止 / 摘要压缩 / 生成式 LLM 摘要 / 审批点 / 真实模型 / 真实审批 / 评测门 / Skill 加载(A) / 真渐进式 Skill(B) / 多 tool_call 并发 / 豆包真实链路 / 豆包真实审批」，
前六个用 `MockLLM`（脚本化假模型）不依赖 API Key，真实场景可走智谱 GLM 或豆包（火山方舟）：

- **智谱**：需 `ZHIPU_API_KEY`，写入项目根 `.env`（已被 `.gitignore` 排除）；模型可用 `ZHIPU_MODEL` 切换，默认 `glm-4.7-flash`，限流时可换 `glm-4.5-flash`。
- **豆包（火山方舟 Ark）**：需 `ARK_API_KEY` + Model ID；base_url `https://ark.cn-beijing.volces.com/api/v3`；模型默认 `doubao-seed-2-1-lite-260915`，用 `DOUBAO_MODEL` 切换（如 `doubao-seed-2-1-pro-260915`）。每个模型**独立**赠送 50 万 tokens 免费额度（调用哪个模型只消耗哪个模型的额度），安心体验模式下额度耗尽自动暂停、不会扣费。
撞上限与循环命中均为"软停止"（返回带原因的 RunResult），
不是 error——对齐生产 Agent 的"交还用户"语义。审批点采用风险分级：工具声明基础风险
（`BaseRisk` L0-L3），风险由 `RiskEvaluator` 判定——静态/函数（参数感知）/模型
（`LLMRiskEvaluator` 用独立 LLM 判断，只升不降）；低于审批阈值自动放行、达到阈值
进闸门；未配置 approver 时默认拒绝（fail-closed）；连续拒绝达上限升级中止。
拒绝回填文案为**反绕过版本**：明确"停止该动作，不要尝试效果等价的替代操作"——
真实模型会把"换个方式达成同样目的"（删不掉就覆盖清空）当作合法路径，
只有显式封死等价替代，模型才会走"汇报 / 询问用户"的安全分支。

**真实接入踩坑记录（重要）**：
- `glm-4.7-flash` 是混合思考模型：reasoning 会先吃掉输出预算，`max_tokens` 太小导致
  `content` 为空——风险判断这类"只输出级别名"的任务会解析失败，**静默退回基础风险
  造成高风险动作漏审**。修复两层：① 风险判断 Provider 给足 `MaxTokens`（2048）；
  ② `LLMRiskEvaluator` 解析失败不再退回基础风险，改为保守升级
  `maxRisk(base, MEDIUM)`（宁多送审、勿漏审——不对称风险论）。
- 豆包 2.x（`doubao-seed-2-1` 系列）同为混合思考模型：风险判断任务一样要给足
  `MaxTokens`（2048）。实测：delete 参数被正确判为 L3-high 进闸门，read 判低风险放行；
  拒绝回填后模型走"汇报+询问用户"的安全分支，未尝试等价绕过。
- 真实模型判断存在输出方差：同一请求在不同运行可能给出不同级别，生产应换
  专用小模型/分类器 + 确定性输出约束（JSON mode / 低温度 / 多次采样）。
- 多 tool_call（并行工具调用）：真实模型支持一轮返回多个 tool_calls，内核按
  "1 条 assistant（带全部 tool_calls）+ N 条 tool 结果（各自 tool_call_id 关联）"
  的协议回填；每个调用独立过循环检测/审批/执行，一个被拒不影响其他。
  **执行阶段两阶段设计**：护栏（循环检测/审批，有共享状态）和失败重试预算
  （guard.retryHit）都在主 goroutine 顺序处理；只有"全部工具声明并发安全
  （`ConcurrentSafe: true`）"的整批才用 goroutine + WaitGroup 并行执行
  （`go test -race` 无竞争），结果按**原始 tool_calls 顺序**回填（审计/复现稳定）。
  未声明并发安全或含 load_skill/read_skill_ref 特殊通道（写内核 map）时
  整批回退顺序执行——并发是优化，正确性优先。工具是否线程安全由工具作者
  声明（JS/TS 无共享内存并发，不需要这个概念；Go 里 goroutine 共享内存，
  并发安全声明把责任交给工具实现方）。`-demo parallel` 演示 3 个 400ms
  调用并行约 0.4s 完成。
- 生成式 LLM 摘要器（`LLMSummarizer`）：实现 `Summarizer` 接口，把旧消息交给
  真实模型写**结构化摘要**（6 段式，对齐 Claude Code 的 9 段 compaction：
  任务目标/已执行动作/已完成/失败与问题/未完成与待办/用户约束——结构化比
  自由摘要信息密度高、后续模型更容易续接，是生产框架的标准做法）。
  两个配套踩坑：
  ① **thinking 模型 max_tokens 同坑**：豆包 2.x 是混合思考模型，摘要器
  `MaxTokens` 设太小（如 120/200）会让 reasoning 吃光预算、`content` 为空 →
  摘要失败全部回退 FIFO（`-demo llm-compact` 一度摘要从未插入就是这个原因），
  必须给足（2048）；② **收敛护栏**：生成式摘要输出长度不可控，若摘要比它
  替换的旧批次还大，`CompactContext` 的 while 循环会永不收敛（token 不降、
  每轮调一次模型 → 死循环，实测 12 分钟烧掉数万 token）。修复：摘要不比
  原文小就回退 FIFO 丢弃（`context.go` 的 `CompactContext`），保证循环必然
  收敛。另外预算不宜设得太小：会触发反复压缩、每次压缩多一次模型调用
  （`-demo llm-compact` 预算 650 在 6 步后触发 1 次压缩，摘要稳定插入）。
- **窄任务关 thinking（豆包 2.x / glm-4.7 等深度思考模型）**：思考链（COT）
  计入输出 token 计费，且输出单价是输入的 5 倍（豆包 6 vs 30 元/百万）——
  摘要、风险判断这类"窄任务"用不到深度思考，`thinking:{"type":"disabled"}`
  关掉后输出成本降 70~90%（`-demo llm-compact` 实测 completion 从 1000+ 级
  降到 133）。**注意：thinking 参数非所有模型通用**——DeepSeek 靠模型 ID 区分
  （deepseek-reasoner / deepseek-chat，无参数开关）；OpenAI 用 reasoning_effort
  且不能完全关闭。Provider 默认不传该参数（`DisableThinking=false`），避免塞给
  不支持的模型被 400 拒绝。
- **usage（服务端真实用量）触发压缩**：`usage` 是 OpenAI 兼容协议标准字段
  （prompt/completion/total_tokens，各厂商通用），Provider 解析后随
  `LLMResponse.Usage` 回传并可选打印（`Verbose=true`）——本地估算
  （`EstimateTokens`）漏掉角色标记、工具 schema 等结构性开销，真实计费恒大于
  估算；压缩触发**优先用服务端 usage**（fast-agent 等行业实现同此），mock 无
  usage 时估算兜底。usage 触发时压缩目标取预算 1/2（覆盖结构开销，避免
  "压完仍超"）。
- **上下文缓存红利（免费）**：`doubao-seed-2.0+` 支持隐式上下文缓存，重复前缀
  （system、工具 schema、早期历史）命中后输入单价降 5 倍（6→1.2 元/百万）——
  harness 每轮重发相同前缀，天然受益；保持消息顺序稳定即可，无需额外配置。
  免费额度统计口径 = 输入 + 输出（含思考链），安心体验模式下额度耗尽自动
  暂停、不会扣费（13 万 token 教训：死循环调试 + 思考模型 COT + 压缩过频，
  三者叠加瞬间烧掉 1/4 免费额度）。
- 评测门：任务集 + 判定器分离（`internal/eval`）。RuleJudge（确定性规则：必调工具/
  参数级安全判据/回答子串）零成本可复现；LLMJudge（LLM-as-judge）输出结构化 JSON
  （passed/reason），失败或解析不了判负（评测也 fail-closed）；判定基于完整轨迹
  （`Agent.History()`）而非只凭最终回答——能发现"模型声称删了但没删"这类事实性谎言。
  `go run . -demo eval` 演示 4 用例（成功/多工具并行/审批安全/未收敛）→ 成功率 75%。
- Skill 加载双方案：**A 方案**（`SkillAsTool`）把 SKILL.md 包装成普通工具——Description 是常驻
  metadata（第一层），正文在模型调用后才作为工具结果进入上下文（第二层），即"用 function
  calling 伪装渐进式披露"；**B 方案**（真渐进式披露）用内核内置的 `load_skill` 特殊工具——
  harness 拦截后把 SKILL.md 正文注入 **system 消息**（语义是"技能规范"而非"一次调用结果"），
  同一技能去重只注入一次，tool 消息协议闭合。B 的**发现层**：`SkillProvider.List()` 返回全部
  技能的 name/description/tags 常驻清单，`BuildLoadSkillDescription` 把它拼进 `load_skill`
  工具描述（每次请求模型都看得到），模型"先看目录、再按 name 取正文"——加载只需要 name，
  但发现依赖 description/tags。**L3 按需引用**：`SkillProvider.LoadReference(skill, ref)` +
  `read_skill_ref` 特殊工具，SKILL.md 正文只写"当需要 X 时读取 references/X.md"，执行到
  那一步才拉附属资源（同样注入 system、按 (skill/ref) 去重）；顺序约束——必须先加载技能
  正文才能读其引用；宿主用 `filepath.IsLocal` 拒绝绝对路径和 `..` 穿越。示例
  `skills/example/references/safety.md` 演示了"删除前读取检查清单"的完整链路。
  A 的局限（正文进 tool 结果可能被摘要压缩、触发依赖模型工具
  选择）在 B 中解决。边界清晰：机制归内核（internal/agent），skill 目录/发现归宿主
  （`skillhost.go` 的 `dirSkillProvider`，扫描 `skills/<dir>/SKILL.md` 并解析 frontmatter 的
  name/description/tags 字段）。

token 统计为**估算口径**（CJK 1 字 ≈ 1 token、其余 4 字符 ≈ 1 token，含局限说明见
`internal/agent/context.go` 注释），单元测试见 `internal/agent/context_test.go`、
`internal/agent/approval_test.go`、`internal/agent/provider_test.go`（`go test ./...` 可跑）。

## 约定

- 不自动提交、不自动推送，按明确指示执行 git 操作。
- 代码注释统一标注 JS/TS ↔ Go 差异点（如错误处理、并发模型、类型系统）。
