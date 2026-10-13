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
- [x] 真实文件工具集（PLAN 第 1 步：`read_file` / `write_file` / `edit_file` / `glob` / `grep`，独立包 `internal/fsagent`，与内核解耦——机制归内核、工具归应用层）— 原子写入（同目录 temp + rename）、写前自动备份到 `backup/`（file-ops-policy 落地，备份失败即停止）、`filepath.IsLocal` 路径越界防护、edit 精确替换防幻觉（old 必须恰好出现一次）、glob/grep 跳过备份目录；读类 RiskNone 自动放行，写/编辑 RiskMedium 走审批闸门 — `-demo fs`
- [x] 命令执行工具（PLAN 第 2 步：`run_cmd`，独立包 `internal/cmdagent`）— `context.WithTimeout` 超时 + **进程组杀灭**（Setpgid + kill(-pid)，实测踩坑见下）、输出截断保留头尾防爆上下文、**参数感知风险**（`RiskOfArgs`：白名单命令 go build/test/vet、git status/diff 等降 RiskLow 自动放行，rm/git push/curl/sudo 等危险模式升 RiskHigh 强制审批，未知命令 RiskMedium）— `-demo cmd`
- [x] MCP 双向适配（PLAN 第 4 步：**Server 对外暴露** `internal/mcpserver/mcpserver.go`——Registry 工具映射为 MCP 工具，外部 Agent 可直接调用；**Client 对内接入** `internal/mcpserver/client.go`——外部 MCP server 的工具经 tools/list 发现、包装为普通工具注册进主循环，超时护栏 + 风险映射 + isError 转 error；**Streamable HTTP 传输** `internal/mcpserver/http.go` / `http_client.go`——POST JSON-RPC + GET SSE，对齐火山方舟 Remote MCP 接入形态，实现要点见下）— `-demo mcp` / `-demo mcp-client` / `-demo mcp-http`

## 当前进度（2026-10-13）

第一版已跑通：`go run . -demo normal | loop | soft | compact | approval | real | real-approval | eval | skill | skill-b | parallel | llm-compact | doubao | doubao-approval | fs | cmd | figma | web | plan | stack | mcp | mcp-client | mcp-http | validate | skill-history | skill-path | session | cancel | tree | memory-recall` 二十八个场景分别演示
「正常链路 / 循环检测中止 / 步数软停止 / 摘要压缩 / 生成式 LLM 摘要 / 审批点 / 真实模型 / 真实审批 / 评测门 / Skill 加载(A) / 真渐进式 Skill(B) / 多 tool_call 并发 / 豆包真实链路 / 豆包真实审批 / 真实文件工具集 / 命令执行 / Figma 设计稿 / 前端预览截屏 / 自动决策计划 / 技术栈感知 / MCP 双向适配（Server 暴露 + Client 接入）/ Streamable HTTP（SSE）传输 / 工具结果 schema 校验 / Skill History 短期工作记忆 / Skill 路径作用域 / 会话持久化断点续跑 / 协作式取消契约 / 树索引推理检索 / 长期记忆关键召回」，
前七个用 `MockLLM`（脚本化假模型）不依赖 API Key，真实场景可走智谱 GLM 或豆包（火山方舟）：

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
`internal/agent/approval_test.go`、`internal/agent/provider_test.go`、
`internal/fsagent/fsagent_test.go`（`go test ./...` 可跑）。

**文件工具集实现要点（`internal/fsagent`，PLAN 第 1 步）**：
- **原子写入**：同目录 `os.CreateTemp` + `os.Rename`——同目录 rename 在 POSIX 上是
  原子操作（要么旧文件、要么新文件，绝无半截文件），跨目录会退化/失败；临时文件
  `defer Remove` 兜底清理。对应 JS `writeFile` 直接覆盖的"写一半断电就坏"问题。
- **写前自动备份**：`write_file`/`edit_file` 覆盖已有文件前，旧内容自动存到
  `backup/`（镜像相对路径 + 时间戳），失败即停止写入（file-ops-policy 落地）——
  备份是**内核工具行为**而非模型纪律，不依赖"模型记得先调 backup"。
- **路径越界防护**：所有 path 参数经 `filepath.Clean` + `filepath.IsLocal` 校验
  （拒绝绝对路径与 `..` 穿越，与 `read_skill_ref` 同套路）；symlink 逃逸（root 内
  链接指向 root 外）是生产增强点，需 `EvalSymlinks` 二次校验。
- **edit 防幻觉**：`old` 必须恰好出现一次——0 次（臆想内容）或多次（定位不精确）
  都报错，让模型先 `read_file` 精确定位；宁可报错也不乱改。
- **备份目录自排除**：`glob`/`grep` 跳过 `backup/`——否则模型会从备份里读到
  "已修改/已删除"的旧内容造成幻觉。
- 风险分级：读类（read/glob/grep）RiskNone 自动放行；写/编辑 RiskMedium 达默认
  审批阈值，必须走审批闸门（`-demo fs` 演示了闸门触发 → 放行 → 备份 → 替换全链路）。

**命令执行工具要点（`internal/cmdagent`，PLAN 第 2 步）**：
- **超时杀进程的隐藏坑（实测踩坑，10/06 修复）**：`exec.CommandContext` 默认
  Cancel 只杀"直接子进程"。命令是 `sh script.sh` 时，bash 会 fork 孙进程
  （如 sleep），bash 被 SIGKILL 立即死，但孙进程还攥着 stdout 管道写端——
  **exec.Cmd 的 Wait() 要等管道 EOF 才返回**（stdout 接 `io.Writer` 时内部是
  os.Pipe + copy goroutine），于是"超时了却多等了整个命令时长"（sleep 5 →
  超时 1s 却 5s 才返回）。解法：`SysProcAttr{Setpgid: true}` 自成进程组 +
  `Cancel` 里 `kill(-pid, SIGKILL)` 连孙进程一起杀 + `WaitDelay` 兜底竞态。
  对照：直接跑 `sleep`（无 shell）没这问题（进程自己就是叶子）；stdout 直接
  继承终端也没问题（无内部管道）。JS/TS 对应物：`child_process` 的
  `detached:true` + `process.kill(-pid)`，及 kill 后等 stdout 关闭的处理。
- **参数感知风险**：同一 `run_cmd` 工具，命令不同风险天差地别——`RiskOfArgs`
  先解包 JSON 取命令（**工具参数永远是 JSON 字符串，风险判定必须自己
  Unmarshal**，否则会把 `{"cmd":"go` 当命令名，白名单全不命中），白名单
  （go build/test/vet、git status/diff 等）降 RiskLow 自动放行、危险模式
  （rm/git push/curl/sudo/重定向等）升 RiskHigh 强制审批、未知命令 RiskMedium。
  数组执行模式（无 shell）已免疫 shell 注入，危险词检测是第二道防线。
- **命令退出码 ≠ Go error**：非零退出码是"业务结果"（测试失败、编译报错要
  回填给模型继续处理），只有执行层面失败（命令不存在/超时/无法启动）才是
  error——与 JS child_process 的 exit code / throw 之分同理。
- **输出截断保留头尾**：长输出头尾各半 + 显式"已截断 N 字符"标记——报错通常在
  结尾，头尾保真即可；与内核压缩机制同动机（防爆上下文）。

**MCP 适配层实现要点（`internal/mcpserver`，PLAN 第 4 步）**：
- **双向角色同包复用**：Server（`mcpserver.go`）与 Client（`client.go`）共用
  `rpcRequest`/`rpcResponse` 协议结构——"一套协议，两个方向"是学习点：
  Server 把 harness 工具翻译成 MCP 工具（外部 Agent 调用），Client 把外部
  MCP 工具翻译回 harness 工具（注册进主循环）。传输双形态：`NewInMemory`
  （net.Pipe 全内存互通，测试/demo 零依赖）+ `NewStdio`（起外部 server
  子进程，生产形态；stderr 收集供调试——MCP 约定日志走 stderr，混入
  stdout 会破坏按行解析）。
- **超时护栏**：单次请求默认 30s（`time.Timer` + select 多路等待：响应/超时/
  ctx 取消），防外部 server 挂死卡住主循环——"外部依赖不信任"，对齐
  生产"下游服务超时"的处理。`WithTimeout` 可覆盖。
- **风险映射**：外部工具透出 `risk` 字段（harness 扩展）则映射为对应
  RiskLevel；**未声明一律保守 RiskMedium**（宁多送审、勿漏审）——外部工具
  照样走 harness 审批闸门，"MCP 接入不等于放开风险"。`-demo mcp-client`
  实测 read_file 等读类工具映射 RiskNone 自动放行、write_file/edit_file/
  run_cmd 映射 RiskMedium 达审批阈值。
- **isError 转 error**：MCP 约定工具执行失败是"业务结果"进 result
  （`isError:true`），不是协议错误——Client 必须把它转成 error 回填，
  模型看到失败而非"假成功"（与内核工具失败语义对齐）。
- **并发安全不默认**：外部工具默认不声明 `ConcurrentSafe`（不知道对方是否
  线程安全），内核自动顺序执行，正确性优先。
- **响应路由**：常驻 reader goroutine + `pending map[int]chan` 按 JSON-RPC id
  投递响应（标准 client 模式）；写请求前先登记 pending，防止"响应先到"
  竞态；无 pending 的消息（server 主动通知）丢弃。
- **Streamable HTTP 传输（`http.go` / `http_client.go`）**：与 stdio 并列的
  第三传输形态——stdio 是进程内/本地（起子进程、管道通信），HTTP 是网络
  形态（**火山方舟 Remote MCP 只支持 Streamable HTTP，不认 stdio**，云上
  Agent 通过 URL 接入）。同一内核（`handleMessage`）双传输复用，"机制与
  传输解耦"。Server 端：POST /mcp 同步处理 JSON-RPC（notification 回 202
  Accepted 空 body）、GET /mcp 开 SSE 流 + `Push` 广播 server 主动消息
  （tools/list_changed 等，`event: message\ndata: <json>`）。Client 端：
  `NewHTTP(url)` POST 请求（`Accept: application/json, text/event-stream`）、
  session id 自动跟随、`Subscribe` 可选 SSE 订阅。**踩坑**：notification
  （initialized）必须**不带 id**（rpcRequest.ID 为 nil）——HTTPClient 第一版
  误用带 id 的 request 发 initialized，Server 按 -32601 Method not found 回拒；
  修复后专门抽 `sendNotification`（"发出即忘"，只检查 202/200）。简化点
  （文档化）：多 session 隔离、"202 + SSE 延迟回响应"的异步模式未做
  （本包工具全部同步执行）。`-demo mcp-http` 实测：真实端口起 HTTP Server →
  HTTPClient 连接发现 14 工具 → 主循环调外部 read_file 读真实文件 →
  server Push tools/list_changed，SSE 订阅打印收到。

## 短期缺口落地（2026-10-13 反思回写）

来源：`reflections/` 视角一/二/三的「先补」结论——三个缺口不依赖第 5 步，
已全部落地（PLAN 状态 ✅），配套 demo `validate` / `skill-history` / `skill-path`。

- **工具返回结果 schema 校验（缺口 A，`tools.go` + `loop.go`）**：`Tool` 新增可选
  `ResultValidator func(output string) error`（nil = 不校验，向后兼容）；内核在
  工具执行成功后、结果回填上下文**之前**调用 `Registry.ValidateResult`——校验失败
  的原文**不进上下文**，回填明确错误（"工具返回结果未通过校验: …"），并计入
  重试预算（`guard.retryHit`）。与循环检测分工：同参重复永远先被循环检测拦截
  （精确重复），校验重试预算兜"换了目标仍连续失败"。`-demo validate` 实测：
  模型两次收到校验错误后给出"建议人工检查"而非假成功。
- **Skill History 短期工作记忆（缺口 B，`skillhistory.go` + `loop.go`）**：环形
  buffer（容量 8）记录最近工具调用（工具名/参数摘要 60 字符/成败，失败含校验
  失败）；**上下文压缩触发时**注入「已尝试」快照 system 消息（快照未变化不
  重复注入）——模型看到"之前试过什么、结果如何"，避免**语义重复**的失败路径
  （循环检测只拦精确重复）。不改 Summarizer 接口（快照是独立 system 消息而非
  摘要的一部分，机制边界更清晰）。`-demo skill-history` 实测：低预算触发压缩，
  快照含 step1 成功 + step2 失败两条记录注入 system。
- **Skill 路径作用域（缺口 C，`skill.go` + `skillhost.go` + `loop.go`）**：
  `SkillMeta` 新增 `Paths []string`（适用目录 glob，空 = 全局，向后兼容）；
  宿主侧 `FilterCatalogByPath` 收窄常驻候选清单（模型"看目录"时选不到不适用
  技能），内核 `runLoadSkill` 二次兜底（contextPath 不匹配 → 拒绝加载，区分
  "技能不存在"与"不适用"两类错误）；`BuildLoadSkillDescription` 输出 paths
  标注。`-demo skill-path` 实测：候选收窄 + 内核拒绝 + 命中路径加载成功三连。

## 第 5 步阶段一落地 · 会话持久化 + 断点续跑（2026-10-13）

来源：PLAN 第 5 步 ①（长期记忆地基）。内核从"单次 Run"扩到"跨会话"。

- **`internal/memory`（新包）**：`SessionStore` 把 Agent 对话历史
  （`[]agent.Message`）JSON 落盘，每会话一个文件（`sessions/<id>.json`）。
  原子写（同目录 temp + rename，与 fsagent 同模式）：写一半崩溃不留半截
  文件；会话 ID 只允许字母数字与 `-_`（拼路径，防穿越）；`Load` 对不存在的
  会话返回 ok=false（首次续跑前本来就没文件）。依赖边界：import
  internal/agent 的消息类型（学习项目允许；生产框架会把消息模型放独立
  schema 包防循环依赖）。
- **断点续跑（`loop.go`）**：`WithInitialHistory(msgs)` 把恢复的历史作为
  初始上下文——`Run` 只追加本次 user 消息，不重建 system 首条（历史已含）。
  约定：guard（循环检测）/审批计数等**本轮护栏状态不随历史恢复**（跨会话
  不该继承，新 Run 是新护栏）；B 方案已加载技能可从 history 的 system 消息
  （`[已加载技能规范 X]` 前缀）推导，无需单独序列化。
- `-demo session` 实测：Run1 完成任务 → 5 条消息落盘 → 新 Agent 实例加载
  恢复 → 直接汇报上次结论（无需重述探索过程）。
- 单元测试：`internal/memory/session_test.go` 4 用例（往返/缺失/List/原子覆盖）、
  `internal/agent/resume_test.go` 3 用例（恢复续跑/向后兼容/不污染外部切片）全绿。

## 协作式取消契约落地（缺口 D · 2026-10-13）

来源：`reflections/2026-10-13-cooperative-cancel.md`（视角四）。取消不是强杀，
是受控状态转换；副作用先分类再补偿；不可逆执行前解决；状态如实。

- **取消入口（`loop.go`）**：`RunWithContext(ctx, task)`——ctx.Done() 触发
  cancel_requested，任务在安全检查点停止；`Run(task)` 保持原签名（委托
  Background ctx，向后兼容）。安全检查点三处：①每步顶部（不开始新步骤）
  ②工具执行前（不可逆动作额外二次检查）③工具返回记账后再查（最多多完成
  一个可控单元）。
- **副作用契约（`tools.go`）**：`Tool.SideEffect`（SideReadonly /
  SideCompensatable / SideIrreversible，默认只读向后兼容）+ `Tool.Compensate`
  （补偿动作声明）+ `Tool.Idempotent`（幂等声明）。与 BaseRisk 分工：
  风险分级管「该不该做」（审批前置护栏），副作用类型管「做了之后怎么撤」。
- **步骤账本 + 逆序补偿（`cancel.go`）**：每次工具执行后立即记账
  （请求标识 InputHash / 结果 / 副作用类型 / 完成时间），闭合"外部成功后
  本地落账前"的缝隙；取消收尾按执行日志**逆序**调用补偿动作（撤销栈）；
  补偿失败记录尝试次数 → 状态 `needs_review`（人工接管），不假装回滚。
- **状态如实分级**：`canceled`（无可逆残留）/ `partially_completed`（不可逆
  已提交，如实告知）/ `needs_review`（补偿收不干净）——不把"进程已停"
  冒充"外部世界已恢复"。账本经 RunResult.Ledger / Compensations 暴露
  （completed/aborted 路径同样携带，喂第 8 步轨迹导出）。
- `-demo cancel` 实测三场景：可补偿逆序收尾 → canceled；不可逆已提交 →
  partially_completed；补偿失败 → needs_review（含尝试次数记录）。
- 单元测试：`internal/agent/cancel_test.go` 8 用例（开始前取消/请求发出后
  响应前取消/逆序补偿/不可逆 partial/不可逆执行前拦截/补偿失败/未声明补偿/
  正常完成账本）全绿。

## 第 5 步阶段二落地 · 树索引 + 推理检索（2026-10-13）

来源：PLAN 第 5 步②（决策依据 2026-10-13：PageIndex 定路线 + OpenViking 补
分层规格）。检索不做向量相似度，让模型像翻书一样沿树定位；成本只随命中
节点增长，结果引用可追溯。

- **`internal/tree`（新包）**：`TreeIndex` 把目录树组织成可浏览的层级索引。
  - L0（一行摘要，browse 用）：目录=名字+子项数，文件=名字+行数+首行——
    "先看目录"，一屏决定下钻方向；
  - L1（概览，overview 用）：目录=子项清单，文件=头部 40 行+行数——
    确认"这层有没有我要的东西"；
  - L2（详情，read 用）：完整内容**带行号**——引用可追溯的载体（file:line）。
  - 构建"索引侧全量一次"（walk + 统计行数），检索关键路径（browse/overview）
    **不经过 LLM**（启发式摘要；"基础模型精修"是可选项）。
  - **默认忽略 `.env`**（密钥防泄露：browse 摘要首行会进模型上下文/日志）、
    .git / node_modules / sessions / .DS_Store。
- **检索 = 工具调用序列**：`tree_browse` → `tree_overview` → `read_file`，
  复用 Agent 既有工具协议（字符串入参/出参），零新依赖、复用第 1/2 步。
- `-demo tree` 实测：模型 browse 根 → 下钻 internal/agent → overview loop.go
  → read 全文 → 定位到 **RunWithContext 定义于 loop.go:307**（真实行号）；
  成本对比：树索引 74 节点 / 全量 17052 行，本次只读 1 个文件全文。
- 单元测试：`internal/tree/tree_test.go` 7 用例（构建+browse/忽略规则/
  browse 文件报错/目录与文件 overview/L2 行号/缺失路径/根路径错误/.env 防泄露）
  全绿。

## 第 5 步③落地 · 记忆分层 + 关键信息优先召回（2026-10-13）

来源：PLAN 第 5 步③（反思回写）。记忆分三层，本步补齐**长期层**：

- **短期（working memory）**：本轮上下文——压缩 + LLM 摘要（既有机制）；
- **中期（session）**：整段会话历史落盘——阶段一 SessionStore；
- **长期（key memory）**：跨会话关键记忆——本步新增。**不恢复全部历史，
  只按任务召回相关条目**（关键信息优先），成本与历史长度无关。

- **`internal/memory.MemoryStore`（扩展）**：长期记忆存储，`memories.json`
  原子写落盘（与 SessionStore 同款 temp+rename）；`Add` 按 (内容,类别) 去重
  （幂等）；**`Recall` 关键信息优先排序**：查询词命中数 ×2 + 类别权重
  （conclusion/decision 2 分 > constraint/preference 1.5 分 > detail 1 分）
  + 新鲜度降序。
- **`memory.HeuristicExtractor`**：默认记忆提取器（实现 agent 窄接口）——
  取最后一条最终回答为 conclusion 记忆，零成本不调 LLM；可换 LLM 版
  （同 Summarizer 替换模式）。局限如实：会提取所有最终回答（含元回答），
  低价值记忆靠 Recall 排序衰减。
- **Agent 集成（`internal/agent/memory.go`）**：`WithMemoryStore` /
  `WithMemoryExtractor` / `WithMemoryRecallLimit`（默认 3 条）/ `WithSessionID`
  （来源标注）。Run 开始 `recallMemory` 注入【记忆召回】system 消息；
  Run 结束 `settleMemory` 只在 **completed** 时沉淀（中止/取消的会话
  无可靠结论，不污染长期记忆）。
- **依赖方向**：agent 只定义窄接口（`MemoryStore` / `MemoryExtractor`），
  memory 包实现——Go 惯例 consumer 定义接口，避免循环依赖
  （memory 需要 import agent 消息类型）。
- `-demo memory-recall` 实测：会话1 调研 → 沉淀 1 条 conclusion →
  会话2 **新实例不带完整历史** → 仅召回记忆注入 → 直接汇报
  （未恢复阶段一的 5 条消息，只注入记忆条目）。
- 单元测试：`memory/memory_test.go` 4 用例（召回排序/去重/落盘往返/提取器）
  + `agent/recall_test.go` 4 用例（召回注入/未配置不注入/completed 沉淀/
  aborted 不沉淀）全绿。
- **已知局限（沉淀判据）**：当前提取器为占位实现——"最后一条最终回答
  全收"，无复用价值判断，demo 中元回答也会入库；改造方向（三问判据 +
  三类信号 + 事件信号优先）见 `reflections/2026-10-13-memory-settling-criteria.md`。

## 约定

- 不自动提交、不自动推送，按明确指示执行 git 操作。
- 代码注释统一标注 JS/TS ↔ Go 差异点（如错误处理、并发模型、类型系统）。
