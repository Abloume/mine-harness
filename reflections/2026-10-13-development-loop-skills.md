# REFLECTIONS · 阶段反思（2026-10-13 · 开发流程 Skill 方法论）

> 定位：mine-harness 建设方向的第二份阶段性复盘（观察视角延续：前一份已沉淀
> 视角一「分层架构与契约设计」、视角二「生产化验收标准」，本份为视角三）。
> 观察对象为一个外部实践——
> 开发者把整个编码流程切成 33 个 SKILL.md（九组）驱动日常开发、一年未手写代码的
> 方法论，以及社区对其的讨论（Anthropic 官方 skill 实践、Masterclass 笔记、
> Perplexity 的 Skill 构建观点、DEV 的市场化批评等）。本文件把它落成与现状的
> 对照清单，用于第 5–8 步的取舍与优先级。

状态图例：✅ 已有 · 📋 规划中 · 🆕 缺口（建议补）

---

## 视角三 · 开发流程 Skill 方法论

> 核心原则：把流程切成边界清晰的一小步，每步一个 skill；写码之前先定验证；
> 管流程的 skill（meta-skill）与管代码的 skill 分开维护。

| 原则 | 与 mine-harness 现状的对照 | 状态 |
|---|---|---|
| **渐进披露**：常驻只加载 skill 简介（33 个共约 4200 token），正文触发才读 | B 方案完全同构：常驻清单 + load_skill 按需加载正文 + read_skill_ref L3 引用 | ✅ |
| **目录自选**：把 skill 清单交给 agent，让它自己挑适合的留下 | load_skill 常驻清单（name/description/tags）同构：模型先看目录再按 name 加载 | ✅ |
| **双循环**：外层（PRD 定 what/why → 架构定 how → 切 ticket）低频定方向；内层（prime→plan→implement→validate→review→commit→PR）高频跑实现 | 第 6 步现规划为单循环「复现→定位→修改→验证」；升级为双循环后与第 7 步 ticket 切分咬合（ticket = 现成的 TaskEnvelope） | 📋 |
| **validation-first**：写码前先规划验证（单测/集成/边界/lint/手动/浏览器自动化），对终点具体、对手段放权 | 评测门 + 视觉验证（serve→screenshot）+ run_cmd 已具备，缺「验证清单前置进实现计划」——也是状态机化节点重试的具体化 | 📋 |
| **实现用全新对话**：规划对话已长，模型有表现下滑区间，按对话边界切分对抗信息过载 | 现用上下文压缩（usage 触发 + LLM 摘要）对抗；补「ticket 级任务切分 = 天然分页」，与第 7 步每 ticket 独立 Run 天然配合 | 🆕 |
| **Skill 评测先行**（社区观点）：先写 Eval 再写 Skill；用例三来源——真实用户查询、已知失败案例、临近领域混淆（像但不该触发） | 评测门只有任务成功率，缺「skill 误触发率」维度；写 SKILL.md 应补「判断边界」而非「操作手册」 | 🆕 |
| **路径作用域**（社区观点）：skill 只在相关目录激活（path scoping） | SkillProvider 目前全局可加载；B 方案可扩展「skill 声明适用目录」，load_skill 按路径过滤 | 🆕 |
| **meta-skills**：rules-check-drift / ablate-ai-layer / system-evolution-review 管流程本身——维护「让任务持续做对的机制」 | reflections + 评测门（对抗用例）是雏形；规则漂移检查、流程复盘可进一步 skill 化/评测化（呼应 Bad Case 闭环） | 📋 |
| **环境工程优先**：单 agent 盯看约 2x，并行才 10x——工程化环境（隔离/调度/防踩踏）而非 agent 本身 | 第 7 步多 Agent 需环境隔离：fsagent root 演进为 per-agent worktree（对应 worktree-create/merge 思路） | 📋 |
| **选择可观测**（社区批评）：AI 选工具用户看不到备选、缺对比透明 | 第 8 步轨迹导出记录「模型为何选/不选某 skill」——关键节点断言之一 | 📋 |

## 优先级结论

1. **先补（社区讨论带来的两个新缺口，改动小、直击 Skill 机制验收点）**
   - Skill 评测先行 + 临近领域混淆用例：评测集扩充「skill 误触发率」维度
     （真实查询 / 已知失败 / 像但不该触发三类用例，第 8 步一并落地）。
   - 路径作用域：skill 声明适用目录，load_skill 按路径过滤（B 方案增强）。
2. **第 6 步开工前定**：双循环结构（外层规划 / 内层实现）+ validation-first
   （验证清单前置进实现计划，agent 照着写、照着跑、照着改）。
3. **方向印证**：渐进披露与目录自选已被外部实践二次确认，继续按 B 方案演进；
   meta-skills 思路印证 reflections 机制的「管机制」价值，可继续用反思驱动流程改进。

## 对后续步骤的映射

- **第 5 步 RAG**：不受本轮影响（记忆分层 + 关键信息优先召回已定）
- **第 6 步工作流 Skill**：双循环结构 + validation-first（验证清单前置）+ 状态机化节点重试
- **第 7 步多 Agent 编排**：信封契约（已有）+ ticket 切分 + per-agent worktree 环境隔离
- **第 8 步可观测性**：评测集补 skill 误触发用例；轨迹导出记录 skill 选择决策
