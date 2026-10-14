---
name: dev-add-demo
description: 给 mine-harness 添加一个新的 -demo 演示场景并配套单元测试。适用于"加一个 demo、新增工具/机制并补测试"的开发任务。
tags: [dev, demo, testing]
paths: ["*.go", "internal/**/*.go"]
---

# 加 demo + 配套单测

## 触发条件

- 要给内核或工具包新增演示场景（`-demo xxx`）
- 要新增工具 / 机制并补充单元测试

## 步骤

1. **先读参照**：看 main.go 的 flag 注释与既有 demo，找结构最相似的一个（如 `runDemoSkillB` / `runDemoFS`）复制骨架；
2. **注册分支**：main.go 的 switch 加 `case`，顶部 flag 注释同步追加场景名；
3. **实现 `runDemoXxx`**：优先用 `MockLLM` 脚本化驱动（确定性、可复现）；真实工具（fsagent / cmdagent）走真实执行，写类动作配审批器（AutoApprover 或打印式放行）；
4. **配套单测**：新增机制放 `internal/xxx` 包 `*_test.go`，覆盖成功 / 失败 / 边界；
5. **验证**：`go build ./... && go test ./...` 全绿后，`go run . -demo <name>` 手动确认输出。

## 验证清单（validation-first，逐项勾完才算完成）

- [ ] `go build ./...` 通过
- [ ] `go test ./...` 全绿（新增用例覆盖成功 / 失败 / 边界）
- [ ] `go run . -demo <name>` 输出符合预期（`-verbose` 可看 trace）
- [ ] 新增写类 / 删除类行为已走审批闸门或备份，未绕过
- [ ] git diff 只含目标文件，未改动无关代码

## 失败时怎么停

- 构建失败 / 测试红：先修到绿，不得强行继续
- mock 脚本走向不确定：先用 `-verbose` 跑一遍观察，再调整脚本
- 某一步卡住超过预期：停下汇报，不换等效方式硬绕
