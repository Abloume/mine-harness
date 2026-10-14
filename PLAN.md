# mine-harness 演进路径（Plan to do）

> 目标：从"演示内核"演进为**帮助日常代码开发任务的个人 Agent**。
> 本文融合两版规划：早期学习计划（4 步：补内核缺口 → RAG → 多 Agent → 可观测性）与
> 代码 Agent 新愿景（真实文件工具 → 命令执行 → 开发工作流 Skill → 会话持久化）。
> 每步沿用「做什么 / Go 练习点 / 验证」三段式，与早期计划保持一致。

状态图例：✅ 已完成 · 🚧 进行中 · 📋 待做

---

## 现状（已完成）

第一版内核跑通 **25 个演示场景**（`go run . -demo normal | loop | soft | compact | approval | real | real-approval | eval | skill | skill-b | parallel | llm-compact | doubao | doubao-approval | fs | cmd | figma | web | plan | stack | mcp | mcp-client | mcp-http | validate | skill-history | skill-path | session`）：

- [x] agent loop（ReAct 主循环）、工具注册、上下文管理（token 预算 + 压缩 + FIFO 兜底）
- [x] 循环检测 + 软停止、审批点（风险分级 + fail-closed）、评测门
- [x] 真实模型接入（OpenAI 兼容 Provider：智谱 / DeepSeek / 豆包 Ark 通用）
- [x] **第 1 步全部完成**：多 tool_call 并发执行（goroutine + 并发安全声明）、真渐进式 Skill
      （B 方案 load_skill + 常驻清单 + L3 按需引用）、生成式 LLM 摘要器（6 段结构化 + 收敛护栏）
- [x] 成本优化（额外完成）：摘要关 thinking、服务端 usage 触发压缩、隐式缓存红利
- [x] 第 2 步命令执行（超时 + 进程组杀灭 + 参数感知风险）、第 3 步前端能力（预览/截屏/Figma）、
      第 4 步 MCP 双向适配（Server + Client + Streamable HTTP 传输）已落地——场景与实现要点见 README

**下一步愿景**：工具集目前全是教学示例（`get_weather`、演示 `file_op`），
补齐真实文件系统与命令执行能力后，harness 才能真正介入日常代码开发。
作为前端工程师，日常开发还涉及设计稿（Figma）、本地预览、浏览器截屏等
第三方场景能力——规划为第 3 步专项。

---

## 第 1 步 · 真实文件工具集（代码 Agent 地基） ✅ 已完成（2026-10-02）

**做什么**
- [x] `read_file` / `write_file` / `edit_file` / `glob` / `grep` 注册为真实工具（`internal/fsagent`，独立包与内核解耦）— 已实现
- [x] 原子写入：临时文件 + `os.Rename`，写失败不留半截文件 — 已实现（atomicWrite）
- [x] 写前备份 + 审批接入（沿用 `skills/example/SKILL.md` 的 file-ops-policy 原则与既有审批机制）— 已实现（自动备份到 `backup/`，备份失败即停止；写/编辑 RiskMedium 走审批闸门）
- [x] 路径安全：`filepath.IsLocal` 拒绝绝对路径与 `..` 穿越（复用 `read_skill_ref` 已有防护）— 已实现

**Go 练习点**
- `os` / `path/filepath` 文件与路径 API（vs JS 无内置文件系统，需 node:fs）
- 事务性写入（temp + rename）——对应 JS 直接 `writeFile` 覆盖的"写一半就崩"问题
- 显式 error 处理（Go 返回值 vs JS throw/catch）

**验证**
- [x] 真实仓库 demo：`go run . -demo fs` — 临时项目里 Agent 读文件 → 修改 → 备份可回滚
- [x] 越权路径用例（`../`、绝对路径）被拒 — `fsagent_test.go` 的 TestPathTraversalRejected 等 14 个测试全绿

> 实现要点与踩坑已记录在 README「文件工具集实现要点」节。

---

## 第 2 步 · 命令执行工具 ✅ 已完成（2026-10-06）

**做什么**
- [x] `run_cmd`：`go build/test/vet`、`git status/diff` 等注册为工具（`internal/cmdagent`，独立包与内核解耦）— 已实现
- [x] `context.WithTimeout` 超时控制（防命令挂死卡住主循环）— 已实现（Setpgid 进程组杀灭 + kill(-pid) + WaitDelay，实测踩坑：exec.Cmd 管道 EOF 等待让超时多等整个命令时长，见 README「命令执行工具要点」）
- [x] 输出截断（长输出只保留头尾，防爆上下文）——联动已有压缩机制 — 已实现（头尾各半 + 显式截断标记）
- [x] 写类命令（git commit/push、rm 等）走审批闸门 — 已实现（参数感知风险：`RiskOfArgs` 白名单降 RiskLow 自动放行、危险模式升 RiskHigh 强制审批、未知 RiskMedium）

**Go 练习点**
- `os/exec` 子进程（vs JS child_process）— 已实现（数组模式无 shell 免注入、Setpgid 进程组）
- `context` 超时取消（vs JS `Promise.race` + AbortController）— 已实现（WithTimeout + Cancel + WaitDelay）
- stdout/stderr 管道流式读取（vs JS stream）— 教学注释已覆盖（bytes.Buffer 收集 + 截断兜底，生产用 StdoutPipe 流式）

**验证**
- [x] demo 在真实仓库跑 `go test` 并解析失败输出回填给模型 — `-demo cmd`：go vet 真实执行（自动放行）→ git status 真实输出 → rm -rf 被审批拒绝（RiskHigh）
- [x] 超时用例（sh 脚本 sleep 5 + timeout 1 → 1.01s 终止）、退出码回填、截断、路径/风险判定 — `cmdagent_test.go` 10 个测试全绿

> 实现要点与踩坑已记录在 README「命令执行工具要点」节。

---

## 第 3 步 · 前端场景能力（预览服务 + 浏览器截屏 + Figma 接入）

> 前端开发任务与后端不同：产出要"看得见"——设计稿、本地页面、渲染效果。
> 作为前端工程师的日常开发 Agent，这三类第三方能力是刚需。

**做什么**
- **本地预览服务**：`serve_dir` 工具用 `net/http` 静态文件服务起本地页面
  （或复用第 2 步 `run_cmd` 起 vite/webpack dev server）；端口占用检测 +
  进程生命周期管理（防僵尸进程、退出时清理）
- **浏览器截屏**：接入无头 Chrome（**rod** / chromedp，Go 的 Chrome DevTools
  Protocol 客户端）对本地/线上页面截图 → PNG 落盘；截图作为**多模态工具结果**
  （扩展 tool 结果回填：图片可交给视觉模型做页面验证）
- **Figma 接入**：Figma REST API（Personal Access Token）读取设计稿节点树、
  导出 PNG/SVG → 提取颜色/尺寸规范生成样式变量、组件骨架；
  token 存 `.env` 管理（同 ARK_API_KEY）

**Go 练习点**
- `net/http` 起 HTTP 服务（vs JS express/koa）
- `os/exec` 子进程 + 端口/生命周期管理
- Chrome DevTools Protocol（WebSocket 客户端，rod 封装了细节）
- 外部 REST API 调用 + JSON 解析（复用 Provider 的 HTTP 模式，零协议改动）
- 多模态工具结果设计（tool 结果从纯文本扩展到图片）

**验证**
- demo：起本地预览 → 无头浏览器截屏 → PNG 落盘 →（可选）视觉模型验证页面
  （此闭环 = 形态 A 的"改完 → 看效果"验证能力，见文末形态决策）
- Figma demo：读取测试设计稿导出规范（需用户提供 Personal Access Token）

---

## 第 4 步 · MCP 适配层（外部 Agent 互通） ✅ 已完成（2026-10-13）

> 形态决策（见文末）：**形态 A（对话式任务 Agent）优先**，AI IDE 待触发。
> MCP 是"外部 Agent 用上 harness 能力"的标准互通层——Codex 等支持 MCP 的
> 宿主可直接调用 harness 的工具；harness 也能接入外部工具服务。

**做什么**
- [x] **MCP Server（对外暴露）**：`internal/mcpserver` 把 `Registry` 注册的工具映射为
  MCP 工具（JSON-RPC 2.0 over stdio：`initialize` / `ping` / `tools/list` /
  `tools/call`；未知方法回 -32601；notification 不响应）— 已实现
  - 参数映射兼容两形态：`arguments.input`（JSON 字符串）直传；或 arguments
    直接是参数对象 → 整体序列化进 Execute（任何 MCP 宿主零成本接入）
  - 工具执行失败 → `isError:true` + 错误进 content（协议层不回 error）；
    未知工具复用 Registry 白名单护栏
  - 风险透出：tools/list 带 `risk` 字段（none/low/medium/high，源自工具作者声明的
    BaseRisk）——外部 Agent 在调用前就能感知副作用等级
  - 审批注入：`SetApprover` 复用 harness 审批纪律——写类/导图工具（write_file /
    edit_file / figma_export_image 等 RiskMedium+）可被人工评审拦截
    （approver 拒绝 → isError:true，工具不执行）
  - `go run . -demo mcp`：脚本化外部 client 调全量工具（文件+命令+前端+Figma，
    buildFullRegistry 组装，缺 token 跳过 figma 并提示）
- [x] **MCP Client（对内接入）**：harness 作为 client 调用外部 MCP server
  （浏览器、Figma、数据库等第三方工具），注册成普通工具进主循环 — 已实现
  （`internal/mcpserver/client.go`：initialize 握手 / tools/list 发现映射为
  agent.Tool / tools/call 执行；超时护栏默认 30s、风险映射（未声明保守
  RiskMedium 照走审批闸门）、isError 转 error、并发安全不默认；
  `NewInMemory` 内存互通 + `NewStdio` 起外部 server 子进程双传输形态；
  实现要点见 README「MCP 适配层实现要点」）
- [x] **Streamable HTTP 传输层**：Server 端 POST /mcp 同步处理 JSON-RPC +
  GET /mcp SSE 流（`Push` 广播 server 主动消息，如 tools/list_changed）；
  Client 端 `NewHTTP(url)` POST 请求 + session id + 可选 SSE 订阅——对齐
  火山方舟 Remote MCP（**只支持 Streamable HTTP，不认 stdio**）的接入形态，
  同一内核（handleMessage）双传输复用 — 已实现（`internal/mcpserver/http.go` /
  `http_client.go`；踩坑：notification 必须不带 id，见 README）
- [x] 工具 schema 转换：Go 工具签名 ↔ MCP JSON Schema 输入定义 — 已实现
- [x] 第 3 步前端能力（预览/截屏/Figma）优先以 MCP 形式注册——一套实现，
  harness 自用 + 外部 Agent 共用 — 已实现（webagent serve_dir/screenshot +
  figmaagent 5 工具纳入 buildFullRegistry；-demo mcp 实测 serve_dir 起预览成功）

**Go 练习点**
- [x] JSON-RPC 2.0 协议实现（stdio 传输层）— 已实现（bufio.Scanner 按行 + 注入 io.Reader/Writer）
- [x] 协议映射：内部工具模型 ↔ 标准 schema（等价于把内部接口翻译成公共契约）— 已实现
- [x] 双向角色：同一套工具层分别做 server / client 两个方向 — 已实现
  （同包复用 rpcRequest/rpcResponse，server.go ↔ client.go 互为镜像）

**验证**
- [x] demo：harness 起 MCP server，用 MCP client（脚本化 JSON-RPC 客户端）
  调用真实文件读取工具 — `go run . -demo mcp`
- [x] 反向：harness 作为 client 调外部 MCP server 完成一次查询 — `go run . -demo mcp-client`
  （net.Pipe 内存互通：client 发现 14 个外部工具 → 注册进主循环 → MockLLM 驱动
  调外部 read_file 读取真实 go.mod；单元测试 5 个全绿，`go test -race` 无竞争）
- [x] HTTP 形态：真实 TCP 端口起 HTTP Server + HTTPClient 跨网络全链路 +
  SSE 主动推送接收 — `go run . -demo mcp-http`
  （httptest 全链路/SSE/超时/isError 转 error 共 4 个测试全绿）

---

## 外部启发回写 · 增量改进（2026-10-13）

> 与 PLAN 逐条对照后确认：Skill 按需加载、工具三形态、记忆三层、输出过 Schema、
> 单 Agent 优先、主项目串路线**均已对齐**——外部实践印证，方向不动，继续走；
> 真正增量集中在四项：可观测性前置 / 仓库 dogfood / 检索数据系统化 /
> 意图路由先于多 Agent（另有 Prompt 回归门禁并入第 8 步）。
> 落地形态：增量 C/D/E 直接并入对应步骤（见下方指引），增量 A/B/F/G/H/I/J 本节落地。

### 增量 A · 结构化轨迹导出前置（可观测性，原第 8 步①提前）

> 评估与 AI 监测不能拖到项目最后临时补；一条完整 Trace 至少要
> 串起输入、Prompt 版本、模型、检索结果、工具参数与结果、审批、Token、缓存命中、
> 延迟与用户反馈，否则线上出错只能看到最终答案，分不清问题来自模型、检索、
> Prompt、工具还是状态管理。

**做什么**
- [x] 以取消契约的步骤账本（RunResult.Ledger / Compensations）为基础扩展为统一
      轨迹事件流：步骤事件（工具选择/参数/结果/成败）+ 决策事件（为何选/不选
      某 skill，含候选清单）+ 审批事件（通过/拒绝/人工接管）+ 取消/补偿事件 +
      请求级元数据（Prompt 版本、模型、Token、缓存命中、首字延迟）
- [x] 轨迹 JSON 作为每次 demo / 评测的**默认产物**落盘（`traces/<run>.json`），
      回放工具（第 8 步①）直接消费——监测能力先于功能补全，防"最后临时补"
- [x] 字段按监测清单预留：请求级 / 链路级 / 质量与成本
      （拒答率、单次任务成本、日预算告警、模型降级次数）

**Go 练习点**
- 事件流建模：类型化事件 + 顺序持久化（vs JS 无内置结构化日志设施）
- JSON 落盘复用既有原子写（temp + rename）模式

**验证**
- [x] demo：一次普通 Run 产出可读轨迹 JSON，能回答「模型为何选/不选某 skill、
      审批在哪一步通过/拒绝、Token 与延迟多少」（`go run . -demo trace`，
      落盘 `traces/trace-<时间戳>.json`；已实现 TraceLLM/TraceTool/TraceApproval/
      TraceSkill/TraceCompact/TraceCancel 六类事件 + `trace_test.go` 单测覆盖）

### 增量 B · 仓库自用 AGENTS.md（dogfood）

> 项目规则写进仓库（AGENTS.md / CLAUDE.md），只留每次必守的短约束；
> 手册式长文会浪费 Token、把真正重要的约束冲淡。

**做什么**
- [x] 仓库根写 AGENTS.md：Go 技术栈、目录职责（internal/* 分包含义）、
      测试命令（`go test ./...` / `go run . -demo xxx`）、禁改禁读范围
      （已落地：AGENTS.md + 第一个开发 Skill `skills/dev-add-demo/SKILL.md`，
      含 demo 注册/单测/验证清单的完整流程）
      （`backup/` 备份目录、`.env` 密钥、memory / sessions 数据文件）
- [ ] 与 README 分工：README 保留给人读的完整手册，AGENTS.md 只放
      「每次必守的底线约束」短清单——指令层与记忆层隔离（AGENTS.md
      走指令装配，不混入记忆召回）
- [ ] 把「加 demo + 配套单测」沉淀为本仓库第一个开发 Skill
      （触发条件 / 步骤 / 验收），用 harness 自己的 Skill 机制管自己的开发
      流程（dogfood；复用缺口 C 路径作用域限定在 internal/ 与根目录）

**验证**
- [ ] 外部 Coding Agent 打开本仓库能读到规则、遵守边界（不读 .env、不改 backup/）

### 增量 C · 检索数据系统化 → 并入第 5 步②「数据系统化补强」

### 增量 D · 意图路由先于多 Agent → 并入第 7 步「前置步骤」

### 增量 E · Prompt 回归门禁 → 并入第 8 步③

### 增量 F · 压缩保真度抽查（Compact Fidelity）

> 摘要压缩不是无损操作：压太狠丢关键信息（记忆断片），压太轻成本和延迟又降不下来。
> 现有收敛护栏只保证「压缩必然收敛」（防死循环），不保证「关键信息没被误删」——
> 抽查是关键信息防丢失的兜底环。
> （2026-10-14 已落地 ✅）

**做什么**
- [x] 关键实体抽取（规则式、零新依赖）：从被压批次抽取数字 / URL / 引用包裹的专有名词
      （已落地：`extractKeyEntities` 正则抽取，≥2 位数字/URL/引号专名；regexp 包级编译复用）
- [x] 摘要生成后做保真度抽查：关键实体全部丢失 → 回退 FIFO（保底不丢事实）或注入「缺失提示」
      （已落地：`CompactContextReport` 保真度抽查——**全丢 → 判定摘要不可信、保留原文放弃压缩**；
      **部分丢 → 「【补注】缺失实体」追回**；补注后不收敛 → FIFO 兜底，实体数进报告可追溯）
- [x] 抽查结果写进轨迹：TraceCompact Detail 记录「保真度抽查介入 N 批（丢失 M 个关键实体，
      已补注追回或保留原文）」（与增量 A 打通）
- [x] 配套 `-demo compact-fidelity`：同一批次带关键数字，演示「摘要保留 → 通过」与
      「摘要漏关键实体 → 回退」两条路径（已注册 flag/switch，含硬约束保留段）

**Go 练习点**
- 正则抽取（regexp 编译一次复用）+ 集合运算（map 去重/差集）

**验证**
- [x] 单测：含关键数字的批次，摘要保留 → 抽查通过；摘要漏关键实体 → 回退 FIFO 且轨迹有记录
      （已落地：TestCompactFidelityPass / FullLossKeepsOriginal / PartialAnnotates，全绿）
- [x] demo 手动确认两条路径输出（`go run . -demo compact-fidelity` 实测）

### 增量 G · 硬约束保护层（Protected Constraints）

> 用户第一轮的硬约束（过敏禁忌、禁用动作、红线参数）要常驻 system 提示层、
> 不参与压缩——长会话后期「第 15 轮还记得吗」的标准答案。
> 现状：CompactContext 只隐式保护 msgs[0]（首条），中途提出的硬约束无保护通道。
> （2026-10-14 已落地 ✅）

**做什么**
- [x] Message 增加 Protected 标志（roleSystem 恒受保护；显式标记的硬约束消息也受保护）
      （已落地：`Message.Protected bool`；首条 msgs[0] 无条件保留 + Protected 消息跳过选批；
      摘要消息不设 Protected，允许再压缩）
- [x] CompactContext / TrimContext 选批时跳过 Protected 消息（压缩只发生在非保护消息上）
      （已落地：选批从索引 1 起且跳过 Protected；TrimContext 兜底同样跳过）
- [x] 宿主入口：WithHardConstraints 把硬约束注入为 system 消息并标记 Protected
      （机制归内核、注入归宿主）
- [x] 压缩事件 Detail 体现「跳过 N 条受保护消息」（与增量 A 打通）

**Go 练习点**
- 切片过滤与原地压缩的不可变边界（不破坏原切片语义）；结构体字段扩展对既有调用方零破坏

**验证**
- [x] 单测：硬约束在任意位置 → 压缩后仍完整保留；摘要 / FIFO 兜底路径都不触碰 Protected 消息
      （已落地：TestCompactPreservesProtected / TestTrimSkipsProtected / TestWithHardConstraints，全绿）
- [x] 既有 context_test 全绿（无行为回退）

### 增量 H · 任务系统 + 依赖图（Task Graph）

> 单次 Run 只覆盖"一次任务"，任务系统让目标持久化到单次对话之外：
> 任务可分解（父任务拆子任务）、有依赖图（先 A 后 B）、可并行追踪。
> 现状：Agent 只有单次 Run + StepRecord 账本，任务边界=一次对话。

**做什么**
- [x] 任务模型（内核 task.go）：Task 结构（ID / 目标 / 状态 / 父任务 / 依赖集 / Children 反向索引）
- [x] 依赖图：Register 原子注册（ID 唯一 / 前置已注册 / 环路检测）+ UpdateDeps 动态改依赖；
      完成态传播（子任务全 done → 父任务自动 done，递归向上）；依赖未就绪 → IsBlocked / Schedulable 派生门控
- [x] 宿主入口：WithTaskGraph Option——Run 开始 MarkRunning(task)、completed 时 MarkDone(task)，
      中止/取消不标 done（任务保持原状态可重试）；sink 未接入前事件缓冲回放（注册事件不丢）
- [x] 与增量 A 打通：轨迹 TraceTask 事件（registered / running / done / blocked / deps-updated）
- [x] 配套 `-demo task-graph`：A 段纯 TaskGraph 编排（release 拆 t1/t2 可并行 + t3 依赖前置 →
      门控 → 解除 → 传播 release done）；B 段 mock agent 带 WithTaskGraph 跑 Run（t1 自动 done + 轨迹事件）
      （已落地：README demo 场景数 32→33，main.go 头部注释同步）

**Go 练习点**
- 图结构：map 邻接表（Depends 正向 / Children 反向索引）+ 递归 DFS 环路检测
  （JS/TS ↔ Go：递归闭包需先声明 var 再赋值；map 遍历无序 → order 切片保证稳定输出）
- 状态机（pending → running → done / blocked；blocked 为依赖派生而非独立字段）；
  sync.Mutex 防并行领取数据竞争（JS 单线程无此顾虑）

**验证**
- [x] 单测 6 个全绿：RegisterAndCycle（重复/前置缺失/UpdateDeps 环/拒绝后状态不被写入）、
      BlockedAndSchedulable（初始 t1,t2 可调度、t3 门控）、Propagation（t1→t3 仍阻塞 →
      t2→t3 解除 → t3 done → release 自动 done）、MarkRunningIdempotent、
      AgentHooks（Run completed → 任务 done + 3 条 TraceTask 事件）、
      AgentAbortKeepsPending（脚本耗尽中止 → 任务保持 running、无 done 事件）
- [x] 既有测试全绿（无行为回退）；`go run . -demo task-graph` 端到端输出符合预期
- [ ] 持久化复用 internal/memory（任务图落盘/恢复——当前内存态，宿主可自行持久化）

### 增量 I · hooks 扩展系统（Lifecycle Hooks）

> 宿主常需要在不改内核的前提下挂自定义逻辑（拦截、注入、审计、限流）。
> 现有 trace / 审批是专用插槽，hooks 是通用扩展点：工具调用前/后、step 前/后、
> 压缩前后——文章把它列为 harness 的标准组件。

**做什么**
- [ ] 钩子注册表（内核）：OnBeforeTool / OnAfterTool / OnBeforeStep / OnAfterStep / OnBeforeCompact / OnAfterCompact
- [ ] 宿主入口：WithHooks(...Hook) 注册；钩子可改入参、阻断（返回 err → 终止该步）、注入
- [ ] 与既有机制分工：审批闸门管"能不能做"（权限），hooks 管"做了什么 / 怎么处理"（扩展）；trace 记录结果、hooks 干预过程
- [ ] 配套 `-demo hooks`：钩子拦截特定工具名 + 审计日志 + 压缩前后记录 token

**Go 练习点**
- 函数类型一等公民（TS 闭包 ↔ Go func 类型）；切片注册表遍历执行

**验证**
- [ ] 单测：钩子调用顺序、阻断生效、错误传播；无钩子时行为不变（既有测试无回归）

### 增量 J · Coding Agent 分层测评

> 外部测评实践：Coding Agent 的成功判定高度客观，评分应分三层——
> FAIL_TO_PASS（修前失败、修后必须通过，权重最高）/ PASS_TO_PASS（修前通过、不能退化）/
> 静态检查与 patch 质量（门槛 + 加分），并配套轨迹指标与一票否决门禁。
> 现状：eval 门是"demo + 单测全绿"二元判定，无分层；轨迹数据（增量 A）已就位，缺计算层。

**做什么**
- [ ] 评测任务定义分层用例集：FAIL_TO_PASS 列表 + PASS_TO_PASS 列表 + 门禁项（危险命令 / 密钥泄露 / 静态检查）
- [ ] 分层评分（内核 eval）：`score = F2P 通过比例 × 0.6 + P2P 保留比例 × 0.4`；门禁不满足 → 一票否决
- [ ] 轨迹指标计算层：测试调用效率（测试工具调用次数/步数）、文件导航率（修改文件数 / 读取文件数）、迭代收敛性（F2P 通过数随 step 单调递增判定）——从 TraceEvent 流 + StepRecord 聚合
- [ ] 基线稳定性检查：跑用例前先无改动跑一遍基线，确认 F2P 稳定失败、P2P 稳定通过（防环境假阳性）
- [ ] 配套 `-demo eval-scored`：用自家仓库一个历史 bug 构造 F2P/P2P 用例集，演示分层评分 + 指标输出

**Go 练习点**
- 加权评分与比例计算；事件流聚合统计（遍历 TraceEvent 计数）；接口分层（Verifier 接口按层级注册）

**验证**
- [ ] 单测：分层评分正确（F2P 过/P2P 破 → 部分得分）、一票否决生效、文件导航率与迭代收敛性统计正确
- [ ] 无用例集时行为不变（既有 eval demo 无回归）

---

## 反思回写 · 短期缺口（2026-10-13）

> 来源：`reflections/2026-10-13-architecture-and-acceptance.md`、
> `reflections/2026-10-13-development-loop-skills.md` 与
> `reflections/2026-10-13-cooperative-cancel.md` 的「先补」结论。
> 缺口 A/B/C 均不依赖第 5 步，已全部落地（✅）；缺口 D（协作式取消契约）
> 为内核级改造，与第 5 步并行可做——实现要点与 demo 见 README
> 「短期缺口落地」节。
> 与既有机制的分工：循环检测拦「精确重复」，Skill History 拦「语义重复」；
> 工具结果校验拦「模型以为成功了」；取消契约拦「外部指令中止」。

### 缺口 A · 工具返回结果 schema 校验

**做什么**
- [x] 工具声明返回 schema（结构化描述返回字段/类型），`Registry` 登记
- [x] 执行结果回填前校验：合法才进上下文，非法/缺失字段走明确错误（防模型「以为成功了」）
- [x] 与既有失败重试预算联动：校验失败计入重试，超预算走软停止

**Go 练习点**
- 类型化契约：schema 定义 + `encoding/json` 严格解码（vs JS 直接拿对象不校验）
- 校验与执行解耦：工具只管产出，校验是内核通用层

**验证**
- [x] 单元测试：构造非法返回（字段缺失/类型错）→ 断言不进上下文、错误明确 —
  `validate_test.go`（通过/拒绝/中止/未知工具 4 用例，`go test -race` 全绿）
- [x] demo：一个返回 schema 校验失败的用例，模型收到明确错误而非假成功 —
  `go run . -demo validate`

### 缺口 B · Skill History（短期工作记忆）

**做什么**
- [x] 环形 buffer 记录最近 N 步工具调用（工具名/参数摘要/成败）
- [x] 压缩时并入摘要（6 段式加「已尝试」段），防重复无效调用
- [x] 与循环检测互补：循环检测拦「精确重复」，Skill History 拦「语义重复」

**Go 练习点**
- 环形 buffer / 切片截断（vs JS 无内置环形结构）
- 摘要拼接与上下文预算联动

**验证**
- [x] demo：模型第二次尝试同一失败路径时，历史已注入 system，行为改变可观察 —
  `go run . -demo skill-history`（压缩触发注入快照，含成败标记）
- [x] 单元测试：buffer 满覆盖、压缩后仍保留历史摘要 — `skillhistory_test.go`
  （环形覆盖/格式化/禁用/压缩注入/校验失败记失败 5 用例全绿）

### 缺口 C · Skill 路径作用域（B 方案增强）

> 来源：`reflections/2026-10-13-development-loop-skills.md` 的「先补」结论。
> 外部实践确认 skill 只在相关目录激活（path scoping）是常被漏掉的参数；
> 避免"全局可加载"造成误触发面过大。

**做什么**
- [x] skill 声明适用目录/文件范围（如 `paths: ["internal/mcpserver/**"]`），常驻清单可见
- [x] load_skill 按路径过滤：模型上下文无关的 skill 不出现在候选里（收窄选择面）
- [x] 与缺口 A/B 无关，纯 Skill 加载机制增强；配合 skill 误触发评测用例验证

**Go 练习点**
- 路径匹配（`path/filepath` Match/Glob 语义 vs JS 无内置 glob）
- 过滤逻辑与既有常驻清单生成的解耦

**验证**
- [x] 单元测试：不同工作目录下候选清单正确收窄 — `skill_test.go` 缺口 C 三用例全绿
- [x] demo：候选收窄 + 内核拒绝 + 命中路径加载成功 — `go run . -demo skill-path`
  （与评测门联动：临近领域混淆用例在无关路径下不再触发该 skill）

### 缺口 D · 协作式取消契约（内核级）

> 来源：`reflections/2026-10-13-cooperative-cancel.md` 的「先补」结论。
> 外部实践：Agent 执行一半被用户取消——取消不是强杀，是受控状态转换；
> 副作用先分类再补偿；不可逆动作执行前解决；状态如实。
> 与既有机制分工：审批拦「越权动作」、取消拦「外部指令中止」、
> 循环检测/重试预算拦「自我空转」——三类护栏互不重叠、可叠加。

**做什么**
- [x] `Run` 接入 `context.Context`（外部取消信号）+ 取消状态机：
  running → cancel_requested → canceled / partially_completed / needs_review
  （与现有软停止 StatusAborted 并存：软停止是护栏触发，取消是外部指令）
- [x] 安全检查点：工具调用前查一次取消、返回后记录结果再查一次——
  最多多完成一个可控单元，不把状态截断在任意一行代码
- [x] 副作用契约：工具声明副作用类型（readonly / compensatable / irreversible）
  与补偿动作；任务进入 canceling 后按执行日志逆序补偿；
  补偿本身可能失败，记录尝试次数与人工接管入口（沿用软停止交还用户语义）
- [x] 不可逆动作执行前二次检查取消状态 + 高风险人工确认（复用审批闸门）；
  外部调用幂等键（步骤账本记请求标识，超时后用同一键查结果而非重复创建）
- [x] 步骤账本：请求标识 / 结果 / 完成时间 / 可用补偿方式（喂第 8 步轨迹导出）

**Go 练习点**
- `context` 取消传播（vs JS AbortController / AbortSignal）
- 显式状态机（状态迁移表 vs 隐式 flag 组合）
- 幂等键设计（请求标识生成 + 去重查询，vs JS 无内置幂等语义）

**验证**
- [x] 单元测试：取消竞态（请求发出后响应返回前取消、外部成功后本地落账前取消、
  补偿执行一半再失败）→ 断言无新副作用、已发生动作有记录、可补偿收敛 —
  `cancel_test.go` 8 用例全绿
- [x] demo `-demo cancel`：运行中取消——只读步骤直接结束、
  可补偿步骤逆序执行、不可逆步骤如实上报（needs_review / partially_completed）；
  账本经 RunResult.Ledger / Compensations 暴露（completed/aborted 同样携带）

---

## 第 5 步 · 记忆与检索（RAG 接入）

**做什么**
- ① 长期记忆：会话持久化 + 断点续跑（呼应已有上下文管理，从"单次 Run"扩到跨会话）
  — ✅ 已实现（`internal/memory` SessionStore JSON 落盘 + `WithInitialHistory`
  断点续跑，`-demo session`；实现要点见 README「第 5 步阶段一落地」节）
- ② 检索工具化——**树索引 + 推理检索优先**（✅ 阶段二已实现，2026-10-13；
  决策依据 2026-10-13，PageIndex 观察，
  追加 2026-10-13 OpenViking 观察：字节火山引擎 Viking 团队开源的上下文数据库
  `volcengine/OpenViking` 以同一趋势工程化落地——层级组织 + 渐进披露 + 检索可观测，
  印证本路线非孤例）：
  「相似不等于相关」是向量检索的老毛病；代码库/文档天然有层级结构
  （目录树、包/文件/函数、章节/段落），检索不做向量相似度，而是**复用既有
  glob / grep / read_file 工具**，让模型沿树推理定位（先看目录 → 翻到文件 →
  扫到函数），结果可追溯到**文件/行**——"检索 = 工具调用序列"，零新依赖、
  复用第 1/2 步；索引侧只做「树结构摘要」（可全量一次、由基础模型精修，
  把 LLM 请出每次检索的关键路径）。**树节点摘要采用 L0/L1/L2 分层规格
  （借鉴 OpenViking：摘要 ~100 token / 概览 ~2000 token / 详情按需加载）**——
  节点摘要承担 L0/L1 角色（定位与导航），命中节点再读 L2 详情，渐进披露
  与 Skill B 方案同一思想在检索侧的复用。**反例排除：OpenViking 最新 release
  已移除目录递归与父级分数传播（改一次全局向量召回 + 统一 rerank）——
  目录分数传播路线不抄**。**数据系统化补强（2026-10-13，增量 C）**：
  检索按「数据系统」维护而非一次性索引——① 变更同步：索引构建后记录
  mtime/目录快照，文件增删改时增量重建或失效标记（防模型从过期树结构定位
  已删除/已移动的代码）；② 固定检索评估集：建一组 browse→overview→read
  定位用例（命中文件/行断言 + 引用可追溯断言 + 拒答与 .env 权限用例），
  索引逻辑改动后跑回归，而不是只靠单个 demo
- ③ 记忆分层 + 关键信息优先召回（反思回写 2026-10-13）— ✅ 已实现：
  短期（压缩+LLM 摘要）已有；中期（阶段一 SessionStore 整历史）；本步补齐
  **长期层** MemoryStore（memories.json 原子写、Add 幂等去重）+ 提取器
  （DefaultExtractor = 回答→conclusion + 账本事件→decision）+ Agent 集成
  （Run 开始按任务召回注入【记忆召回】，completed 才沉淀；Recall 关键信息
  优先：关键词命中 ×2 + 类别权重 + **反馈强化 UsageCount×0.1** + 新鲜度；
  命中回写 UsageCount / LastRecalledAt，Prune 过期清理）
  — `-demo memory-recall`；实现要点见 README「第 5 步③落地」节
- ④ （可选增强，不进主线）向量相似度检索：语义模糊查询（会话历史/笔记）才值得
  embedding，作为第 ② 条的补充路线，需要时再引入。定位细化（OpenViking 观察）：
  **向量只做第一跳定位目录（全局召回命中高分目录后，再沿树推理细化），
  不做全文相似度召回**——全文召回正是 PageIndex 与 OpenViking 共同反对的
  「flat 向量黑盒」老路；Rerank 若有需要可复用第 3 步审批/校验的模型调用通道

**Go 练习点**
- 文件与 JSON 持久化（encoding/json 序列化）
- 切片 / 排序 / 编码
- 树遍历与层级索引结构（对应"树结构从文档布局提取，不依赖 LLM"）
- 向量计算与 TopK 选择（仅第 ④ 增强路线）

**验证**
- [x] 跨会话 demo：上次任务结论可被下次会话检索召回（断点续跑 + 记忆查询）—
  `go run . -demo session`（5 条消息落盘 → 新实例恢复 → 直接汇报）；
  单元测试 memory 4 用例 + resume 3 用例全绿
- [x] 树检索 demo：模型沿目录树定位到具体文件/行并作答（引用可追溯）—
  `go run . -demo tree`（browse→overview→read 定位 RunWithContext@loop.go:307；
  成本只随命中节点增长：74 节点索引 / 17052 行全量，只读 1 个文件全文）；
  `internal/tree` 7 用例全绿
- [x] 记忆分层 demo：会话1 沉淀 conclusion + decision（写动作事件信号）→
  会话2 新实例不带完整历史，仅召回记忆注入作答 — `go run . -demo memory-recall`
  （未恢复阶段一 5 条消息，只注入记忆条目；命中回写 UsageCount 反馈闭环；
  memory 7 用例 + agent recall 4 用例全绿）

---

## 第 6 步 · 开发工作流 Skill（个人 Agent 落地）

**做什么**
- [x] 把日常开发流程封装为 Skill（复用第 1/2/3 步真实工具 + 既有 `load_skill` 渐进加载）：
  修复 bug / 改功能 / 跑测试修失败 / **前端视觉验证**（起服务 → 截屏 → 给视觉模型核对），
  流程 = 复现 → 定位 → 修改 → 验证
  （已落地 4 个 Skill：`skills/fix-bug` / `skills/implement-feature` /
  `skills/run-tests-fix` / `skills/visual-verify`，均含触发条件、流程骨架、
  验证清单、失败回滚路径）
- [x] **双循环结构**（反思回写 2026-10-13）：外层循环 = 规划（拆需求/定 what·why·how、
  切 ticket，低频、事关方向）；内层循环 = 实现（一个 ticket 一轮：加载上下文 →
  规划实现 → 写码 → 验证 → 复核，高频、占满日常）——内层产物即第 7 步的
  TaskEnvelope 切片，两层频率与职责分离
  （已落进 implement-feature 流程：一次一个 ticket 小闭环，产物即任务切片）
- [x] **validation-first**（反思回写 2026-10-13）：实现计划先产出「验证清单」
  （单测/边界/lint/手动/视觉核对），agent 照着写、照着跑、照着改——对终点具体、
  对手段放权；验证清单复用既有评测门 + 视觉验证机制
  （4 个 Skill 正文均内嵌验证清单 + 失败时怎么停）
- [x] Skill 只写"何时加载、加载什么"，正文按需注入 system（机制已在）
- [x] 工作流按状态机思路设计（反思回写 2026-10-13）：节点级可重试（沿用软停止语义）、
  分支失败有回滚路径，不做成线性 Chain——防单点失败拖垮整条链路

**Go 练习点**
- 复用既有 Skill 机制，把领域流程沉淀为可复用技能（学习重点是"机制边界划分"）

**验证**
- [x] 端到端 demo：真实仓库一个小 bug，Agent 按 Skill 流程完成修复并跑测试确认；
  前端场景走"视觉验证 Skill"端到端（起服务 → 截屏 → 核对）
  （`go run . -demo workflow`：临时 Go 项目 Double 乘 3 的 bug，load_skill fix-bug
  注入流程 → go test 红（Double(2)=6 期望 4）→ 定位 → edit_file 最小修改 →
  go test 绿，go test 白名单自动放行、edit_file 走审批闸门；
  `go run . -demo visual-verify`：临时页面标题溢出 → serve_dir 起服务 →
  screenshot 移动端截屏 → 视觉核对（mock）→ 修复 → 再截屏（PNG 23KB→13KB 证明
  生效）→ 停服务；配套 `TestRepoSkillsDiscoverable` 验证 4 个 Skill 可被宿主发现加载）

---

## 第 7 步 · 多 Agent 编排

**做什么**
- **前置步骤 · 意图路由（2026-10-13，增量 D，先于多 Agent 落地）**：
  先做单意图分类/路由工具——判断用户要查知识 / 改数据 / 走售后还是闲聊，
  路由到不同链路（查知识→检索链、改数据→审批链、闲聊→直接答）；
  一句话含多个意图时拆成多条意图并行查询（例："这个项目有什么内容、
  要多少次更新、都是谁提交的" → README 读取 + 提交计数 + 作者列表三路并行，
  比硬贴单个"查项目"标签可靠）
- **触发条件写具体**：单 Agent 频繁在多种职责间打架、任务能明确
  拆分、交接与合并规则清楚时才拆多 Agent；不满足则维持单 Agent + 意图路由
- 主管-子代理模式：Planner 拆解任务 → 多个 Agent 实例各自跑独立主循环 → 汇总合并结果
- （与个人 Agent 结合：如"一个写测试、一个改实现"）
- 层间信封契约（反思回写 2026-10-13）：TaskEnvelope（目标/输入/约束/验收标准）+
  ResultEnvelope（产物/证据/状态/错误）Go 结构体；决定权留 Planner（重试/重派/降级），
  Worker 无状态、只拿任务切片、报错原样返回
- 环境工程（反思回写 2026-10-13）：并行前先做环境隔离——per-agent worktree
  （fsagent root 演进为每 Worker 独立工作目录，对应外部实践 worktree-create/merge
  思路），防互相踩踏、隔离依赖与产物

**Go 练习点**
- 多实例并发编排
- 错误聚合（`errgroup`）
- 上下文传播与结果合并

**验证**
- 两 Agent 协作 demo：拆解 → 并行执行 → 合并汇报

---

## 第 8 步 · 可观测性与评测深化

**做什么**
- ① 结构化轨迹导出（JSON）+ 回放工具——记录关键节点断言，含「模型为何选/不选某
  skill」（反思回写 2026-10-13：选择可观测，防"AI 选工具用户看不到备选"）
  — **已前置落地（增量 A，见「外部启发回写 · 增量改进」）**：轨迹事件流 +
  默认产物，本步只需补回放工具
- ② 评测集扩充：对抗用例（绕过审批、循环诱导）+ **skill 误触发用例**
  （临近领域混淆：看起来很像但根本不该加载该 Skill 的请求，反思回写 2026-10-13）
  + **取消竞态用例**（请求发出后响应返回前取消、外部成功后本地落账前取消、
  补偿执行一半再失败，反思回写 2026-10-13）+ 成功率门槛 CI 门禁
- ③ **Prompt 回归门禁（增量 E，2026-10-13）**：system prompt /
  load_skill 描述 / 拒绝回填文案等 Prompt 版本化（如 `prompt-system-v3`），
  改动后跑固定用例集回归（含敏感词、拒答、防绕过维度），通过再替换线上版本——
  不凭手感改 Prompt（未版本化 Prompt 凭感觉改是最常见的走偏点）

**Go 练习点**
- 表驱动测试、`testing` 包、基准测试（benchmark）

**验证**
- 评测报告 + 回归基线：改内核不破坏既有用例

---

## 依赖关系与优先级

```
第 1 步 文件工具 ──► 第 2 步 命令执行 ──► 第 3 步 前端场景能力（预览 + 截屏 + Figma）
     │                    │                                        │
     └──► 第 5 步 记忆（可并行）   └──► 第 4 步 MCP 适配层（把 1-3 工具标准化暴露）
                                              │
                      第 6 步 开发工作流 Skill（用 1-4 的机制与工具）
                      第 7 步 多 Agent（依赖工具与记忆）
                      第 8 步 可观测性（建议贯穿全程，可先行落地轨迹导出）
```

- **第 1 步先行**：文件读写是所有代码任务的地基，也是 Go 文件系统学习的最佳切入点
- 第 3 步依赖第 1/2 步（预览服务要能起、命令要能跑）；截屏基于预览服务或线上页面，
  Figma 接入独立（只需 token）
- 第 4 步依赖第 1-3 步的真实工具（先把工具做出来，再标准化暴露）；前端工具优先
  以 MCP 形式注册，一套实现多处受益
- 第 5 步与第 1/2 步互不阻塞，可并行推进
- 第 8 步的"结构化轨迹导出"门槛低，可提前穿插——已前置落地为增量 A
  （轨迹 JSON 默认产物）；评测门禁在功能稳定后收紧

---

## 形态决策（2026-10-01 校准）

- **形态 A（对话式任务 Agent）＝ 主线**：生成与修改前端页面均走"对话 + 视觉验证"
  闭环——实际验证：一个月纯用豆包开发页面（含改页面）无需 AI IDE，验证靠
  "改完 → 预览 → 看渲染结果"，不依赖编辑器语义反馈。第 3 步（预览 + 截屏 +
  视觉验证）就是该闭环的自建能力，属于形态 A 的组成部分
- **形态 B（AI IDE 扩展）＝ 待触发**：不预设投入。触发条件写具体——出现跨几十个
  文件的全局重构 / 需要编译诊断闭环的深层业务修改时再评估
- **互通**：第 4 步 MCP 适配层让形态 A 的能力可被外部 Agent（Codex 等支持 MCP
  的宿主）调用；豆包/WorkBuddy 的 Agent 循环为封闭产品，不可集成，工作流协作
  即可（豆包可操作电脑运行 harness CLI）
