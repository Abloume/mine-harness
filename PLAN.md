# mine-harness 演进路径（Plan to do）

> 目标：从"演示内核"演进为**帮助日常代码开发任务的个人 Agent**。
> 本文融合两版规划：早期学习计划（4 步：补内核缺口 → RAG → 多 Agent → 可观测性）与
> 代码 Agent 新愿景（真实文件工具 → 命令执行 → 开发工作流 Skill → 会话持久化）。
> 每步沿用「做什么 / Go 练习点 / 验证」三段式，与早期计划保持一致。

状态图例：✅ 已完成 · 🚧 进行中 · 📋 待做

---

## 现状（已完成）

第一版内核跑通 **24 个演示场景**（`go run . -demo normal | loop | soft | compact | approval | real | real-approval | eval | skill | skill-b | parallel | llm-compact | doubao | doubao-approval | fs | cmd | figma | web | plan | stack | mcp | mcp-client | mcp-http | validate | skill-history | skill-path`）：

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

## 反思回写 · 短期缺口（2026-10-13）

> 来源：`reflections/2026-10-13-architecture-and-acceptance.md` 与
> `reflections/2026-10-13-development-loop-skills.md` 的「先补」结论。
> 三个缺口均不依赖第 5 步，已全部落地（✅）；实现要点与 demo 见 README
> 「短期缺口落地」节。
> 与既有机制的分工：循环检测拦「精确重复」，Skill History 拦「语义重复」；
> 工具结果校验拦「模型以为成功了」。

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

---

## 第 5 步 · 记忆与检索（RAG 接入）

**做什么**
- ① 长期记忆：会话持久化 + 断点续跑（呼应已有上下文管理，从"单次 Run"扩到跨会话）
- ② 检索工具化——**树索引 + 推理检索优先**（决策依据 2026-10-13，PageIndex 观察）：
  「相似不等于相关」是向量检索的老毛病；代码库/文档天然有层级结构
  （目录树、包/文件/函数、章节/段落），检索不做向量相似度，而是**复用既有
  glob / grep / read_file 工具**，让模型沿树推理定位（先看目录 → 翻到文件 →
  扫到函数），结果可追溯到**文件/行**——"检索 = 工具调用序列"，零新依赖、
  复用第 1/2 步；索引侧只做「树结构摘要」（可全量一次、由基础模型精修，
  把 LLM 请出每次检索的关键路径）
- ③ 记忆分层 + 关键信息优先召回（反思回写 2026-10-13）：短期记忆（压缩 + LLM 摘要）已有，
  本步补齐长期记忆层，召回按「关键信息优先」设计
- ④ （可选增强，不进主线）向量相似度检索：语义模糊查询（会话历史/笔记）才值得
  embedding，作为第 ② 条的补充路线，需要时再引入

**Go 练习点**
- 文件与 JSON 持久化（encoding/json 序列化）
- 切片 / 排序 / 编码
- 树遍历与层级索引结构（对应"树结构从文档布局提取，不依赖 LLM"）
- 向量计算与 TopK 选择（仅第 ④ 增强路线）

**验证**
- 跨会话 demo：上次任务结论可被下次会话检索召回（断点续跑 + 记忆查询）
- 树检索 demo：模型沿目录树定位到具体文件/行并作答（引用可追溯）；
  大代码库/长文档场景成本只随命中节点增长（不整读）

---

## 第 6 步 · 开发工作流 Skill（个人 Agent 落地）

**做什么**
- 把日常开发流程封装为 Skill（复用第 1/2/3 步真实工具 + 既有 `load_skill` 渐进加载）：
  修复 bug / 改功能 / 跑测试修失败 / **前端视觉验证**（起服务 → 截屏 → 给视觉模型核对），
  流程 = 复现 → 定位 → 修改 → 验证
- **双循环结构**（反思回写 2026-10-13）：外层循环 = 规划（拆需求/定 what·why·how、
  切 ticket，低频、事关方向）；内层循环 = 实现（一个 ticket 一轮：加载上下文 →
  规划实现 → 写码 → 验证 → 复核，高频、占满日常）——内层产物即第 7 步的
  TaskEnvelope 切片，两层频率与职责分离
- **validation-first**（反思回写 2026-10-13）：实现计划先产出「验证清单」
  （单测/边界/lint/手动/视觉核对），agent 照着写、照着跑、照着改——对终点具体、
  对手段放权；验证清单复用既有评测门 + 视觉验证机制
- Skill 只写"何时加载、加载什么"，正文按需注入 system（机制已在）
- 工作流按状态机思路设计（反思回写 2026-10-13）：节点级可重试（沿用软停止语义）、
  分支失败有回滚路径，不做成线性 Chain——防单点失败拖垮整条链路

**Go 练习点**
- 复用既有 Skill 机制，把领域流程沉淀为可复用技能（学习重点是"机制边界划分"）

**验证**
- 端到端 demo：真实仓库一个小 bug，Agent 按 Skill 流程完成修复并跑测试确认；
  前端场景走"视觉验证 Skill"端到端（起服务 → 截屏 → 核对）

---

## 第 7 步 · 多 Agent 编排

**做什么**
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
- ② 评测集扩充：对抗用例（绕过审批、循环诱导）+ **skill 误触发用例**
  （临近领域混淆：看起来很像但根本不该加载该 Skill 的请求，反思回写 2026-10-13）
  + 成功率门槛 CI 门禁

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
- 第 8 步的"结构化轨迹导出"门槛低，可提前穿插；评测门禁在功能稳定后收紧

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
