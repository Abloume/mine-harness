# AGENTS.md — mine-harness 开发守则

## 技术栈与定位

- Go（标准库优先，无第三方运行时依赖）；内核是学习/练习项目，不是生产框架
- 代码注释统一标注 JS/TS ↔ Go 差异点（错误处理、并发模型、类型系统），便于迁移学习
- 机制归内核（internal/agent），工具/宿主归应用层；新增能力遵循该边界

## 目录职责（internal/*）

- `internal/agent` — 内核：agent loop、工具注册、上下文压缩、审批点、Skill 加载（A/B 方案）、记忆集成、取消契约
- `internal/fsagent` — 真实文件工具（read/write/edit/glob/grep：原子写 + 写前备份 + 路径越界防护）
- `internal/cmdagent` — 命令执行工具（超时 + 进程组杀灭 + 参数感知风险）
- `internal/mcpserver` — MCP 双向适配（Server / Client / Streamable HTTP）
- `internal/memory` — 会话持久化 + 长期记忆
- `internal/tree` — 树索引推理检索
- `internal/eval` — 评测门（RuleJudge / LLMJudge）
- `skills/` — Skill 定义（每个子目录一个 SKILL.md + references/）
- `main.go` — 宿主：demo 场景注册、技能发现（dirSkillProvider）、registry 组装

## 常用命令

- 构建与测试：`go build ./... && go test ./...`
- 竞态检查：`go test -race ./...`
- 跑演示：`go run . -demo <name>`（完整列表见 main.go 顶部 flag 注释）
- 新能力必须配套 `-demo` 场景 + 单元测试，全部绿才算完成

## 禁改禁读（硬约束）

- `backup/`（fsagent 自动备份产物）、`.env`（密钥）、`memory/`、`sessions/`、`traces/`（运行时产物）——不得读入模型上下文、不得提交
- 新增文件工具必须沿用 `filepath.IsLocal` 路径越界防护与原子写（temp + rename）
- 写类 / 删除类动作必须走审批闸门，不得绕过；拒绝回填文案保持反绕过语义
- 不自动 commit / push，按用户明确指示执行 git 操作
