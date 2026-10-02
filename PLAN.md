# mine-harness 演进路径（Plan to do）

> 目标：从"演示内核"演进为**帮助日常代码开发任务的个人 Agent**。
> 本文融合两版规划：早期学习计划（4 步：补内核缺口 → RAG → 多 Agent → 可观测性）与
> 代码 Agent 新愿景（真实文件工具 → 命令执行 → 开发工作流 Skill → 会话持久化）。
> 每步沿用「做什么 / Go 练习点 / 验证」三段式，与早期计划保持一致。

状态图例：✅ 已完成 · 🚧 进行中 · 📋 待做

---

## 现状（已完成）

第一版内核跑通 **14 个演示场景**（`go run . -demo normal | loop | soft | compact | approval | real | real-approval | eval | skill | skill-b | parallel | llm-compact | doubao | doubao-approval`）：

- [x] agent loop（ReAct 主循环）、工具注册、上下文管理（token 预算 + 压缩 + FIFO 兜底）
- [x] 循环检测 + 软停止、审批点（风险分级 + fail-closed）、评测门
- [x] 真实模型接入（OpenAI 兼容 Provider：智谱 / DeepSeek / 豆包 Ark 通用）
- [x] **第 1 步全部完成**：多 tool_call 并发执行（goroutine + 并发安全声明）、真渐进式 Skill
      （B 方案 load_skill + 常驻清单 + L3 按需引用）、生成式 LLM 摘要器（6 段结构化 + 收敛护栏）
- [x] 成本优化（额外完成）：摘要关 thinking、服务端 usage 触发压缩、隐式缓存红利

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

## 第 2 步 · 命令执行工具

**做什么**
- `run_cmd`：`go build/test/vet`、`git status/diff` 等注册为工具
- `context.WithTimeout` 超时控制（防命令挂死卡住主循环）
- 输出截断（长输出只保留头尾，防爆上下文）——联动已有压缩机制
- 写类命令（git commit/push、rm 等）走审批闸门

**Go 练习点**
- `os/exec` 子进程（vs JS child_process）
- `context` 超时取消（vs JS `Promise.race` + AbortController）
- stdout/stderr 管道流式读取（vs JS stream）

**验证**
- demo 在真实仓库跑 `go test` 并解析失败输出回填给模型

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

## 第 4 步 · MCP 适配层（外部 Agent 互通）

> 形态决策（见文末）：**形态 A（对话式任务 Agent）优先**，AI IDE 待触发。
> MCP 是"外部 Agent 用上 harness 能力"的标准互通层——Codex 等支持 MCP 的
> 宿主可直接调用 harness 的工具；harness 也能接入外部工具服务。

**做什么**
- **MCP Server（对外暴露）**：把 `Registry` 注册的工具映射为 MCP 工具
  （JSON-RPC 2.0 over stdio：`tools/list` + `tools/call`）——Codex / 支持 MCP
  的 IDE / 任何 MCP client 都能直接调用 harness 的工具集
- **MCP Client（对内接入）**：harness 作为 client 调用外部 MCP server
  （浏览器、Figma、数据库等第三方工具），注册成普通工具进主循环
- 工具 schema 转换：Go 工具签名 ↔ MCP JSON Schema 输入定义
- 第 3 步前端能力（预览/截屏/Figma）优先以 MCP 形式注册——一套实现，
  harness 自用 + 外部 Agent 共用

**Go 练习点**
- JSON-RPC 2.0 协议实现（stdio 传输层）
- 协议映射：内部工具模型 ↔ 标准 schema（等价于把内部接口翻译成公共契约）
- 双向角色：同一套工具层分别做 server / client 两个方向

**验证**
- demo：harness 起 MCP server，用 MCP client（Codex 或简单 JSON-RPC 客户端）
  调用真实文件读取工具
- 反向：harness 作为 client 调外部 MCP server 完成一次查询

---

## 第 5 步 · 记忆与检索（RAG 接入）

**做什么**
- ① 长期记忆：会话持久化 + 断点续跑（呼应已有上下文管理，从"单次 Run"扩到跨会话）
- ② 检索工具化：embedding + 向量相似度注册为普通工具（模型需要时主动查）

**Go 练习点**
- 文件与 JSON 持久化（encoding/json 序列化）
- 切片 / 排序 / 编码
- 向量计算与 TopK 选择

**验证**
- 跨会话 demo：上次任务结论可被下次会话检索召回（断点续跑 + 记忆查询）

---

## 第 6 步 · 开发工作流 Skill（个人 Agent 落地）

**做什么**
- 把日常开发流程封装为 Skill（复用第 1/2/3 步真实工具 + 既有 `load_skill` 渐进加载）：
  修复 bug / 改功能 / 跑测试修失败 / **前端视觉验证**（起服务 → 截屏 → 给视觉模型核对），
  流程 = 复现 → 定位 → 修改 → 验证
- Skill 只写"何时加载、加载什么"，正文按需注入 system（机制已在）

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

**Go 练习点**
- 多实例并发编排
- 错误聚合（`errgroup`）
- 上下文传播与结果合并

**验证**
- 两 Agent 协作 demo：拆解 → 并行执行 → 合并汇报

---

## 第 8 步 · 可观测性与评测深化

**做什么**
- ① 结构化轨迹导出（JSON）+ 回放工具
- ② 评测集扩充：对抗用例（绕过审批、循环诱导）+ 成功率门槛 CI 门禁

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
