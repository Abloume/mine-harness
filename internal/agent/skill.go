package agent

import (
	"fmt"
	"os"
)

// Skill 描述一个"可被加载的技能"：本质是一份文档（SKILL.md）+ 附属资源。
//
// A 方案（当前实现）：把 Skill 包装成普通工具注册进 Registry，用 function calling
// 机制"伪装"渐进式披露——
//
//	第一层（metadata）：Description 常驻，每次请求都随 tools schema 传给模型，
//	                  模型靠它判断"这个任务需不需要加载该 skill"（对应 Agent Skills 的
//	                  metadata 常驻 system prompt）；
//	第二层（正文）：   SKILL.md 全文只在模型决定调用该工具后才作为"工具结果"进入
//	                  上下文（对应"按需加载正文"）。
//
// 与 B 方案（真渐进式披露）的差异：
//  1. 触发判断：A 交给模型的 function calling 工具选择；B 由 harness 相关性路由
//     或模型调用 load_skill 特殊工具，正文注入 system 而非工具结果；
//  2. 正文去向：A 进对话历史（assistant→tool 往返），可能被摘要压缩摘要掉
//     （加载后的 skill 正文大时，compact 会压缩它——这是真实交互点）；
//  3. references：A 需要 Execute 内部自己读；B 由 harness 提供按需读取通道。
type Skill struct {
	Name        string
	Description string    // 常驻 metadata：模型靠它判断何时加载
	BaseRisk    RiskLevel // skill 本身的基础风险（如"文件操作规范"给 Low）
	Path        string    // SKILL.md 文件路径（正文按需读取）
}

// SkillAsTool 把 Skill 包装为 Tool，Execute 读取并返回 SKILL.md 全文。
//
// JS/TS ↔ Go 差异：TS 里这种包装常写闭包返回新函数；Go 同样用闭包，
// 但要注意闭包捕获的 s 是值拷贝（Skill 是值类型）。若在循环里批量注册，
// Go 1.22+ 每次迭代变量是新变量（与 JS 的 let 语义一致），不会踩经典
// "循环变量捕获"坑——Go 1.21 及以前会全部捕获同一个变量。
func SkillAsTool(s Skill) Tool {
	return Tool{
		Name:        s.Name,
		Description: s.Description,
		BaseRisk:    s.BaseRisk,
		Execute: func(input string) (string, error) {
			data, err := os.ReadFile(s.Path)
			if err != nil {
				return "", fmt.Errorf("加载 skill %s 失败: %w", s.Name, err)
			}
			return string(data), nil
		},
	}
}
