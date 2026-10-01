package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

// ---- B 方案：真渐进式披露（load_skill 特殊通道 + system 注入）----

// LoadSkillToolName 是 B 方案的特殊工具名（保留名，宿主不应注册同名普通工具）。
// 模型调用它 → harness 拦截 → 读取 SKILL.md 正文 → 注入 system 消息，
// 而不是当作普通工具结果回填。
const LoadSkillToolName = "load_skill"

// SkillProvider 负责 Skill 的发现与正文读取（宿主实现）。
//
// 边界（README 约定）：机制归内核（internal/agent），目录扫描/解析归宿主。
// 内核只依赖这个接口，不关心 skill 存在哪、怎么解析——测试里用 stub，
// 生产里用目录扫描/数据库/远程仓库。
//
// 渐进式披露的两层都在这：List() 是常驻 metadata（第一层，模型先"看目录"），
// LoadSkill() 是按需正文（第二层，选中后才取全文）。
type SkillProvider interface {
	// List 返回全部可用技能的常驻元数据——随 load_skill 工具描述发给模型，
	// 模型据此决定加载哪个技能（对应 Agent Skills 的 metadata 常驻）。
	List() []SkillMeta
	// LoadSkill 返回技能正文（如 SKILL.md 全文）；未找到返回 (ok=false)。
	LoadSkill(name string) (body string, ok bool)
}

// SkillMeta 是技能的常驻元数据（第一层）。正文不进这里——正文太重，
// 只按需加载。Tags 是可选分类标签，辅助模型匹配（如 files/safety）。
type SkillMeta struct {
	Name        string
	Description string
	Tags        []string
}

// BuildLoadSkillDescription 把技能清单拼进 load_skill 工具的 Description：
// tools schema 每次请求都发给模型，清单因此"常驻可见"，模型先在清单里
// 找适用技能，再传 name 加载正文——这就是"先看目录、再取正文"的两层分离。
func BuildLoadSkillDescription(catalog []SkillMeta) string {
	var b strings.Builder
	b.WriteString("加载一个技能规范到系统上下文。执行复杂任务前，若发现适用技能请先调用它。参数是 JSON：{\"name\":\"技能名\"}。")
	if len(catalog) == 0 {
		return b.String()
	}
	b.WriteString("\n当前可用技能：\n")
	for _, s := range catalog {
		b.WriteString("- " + s.Name)
		if len(s.Tags) > 0 {
			b.WriteString(" [tags: " + strings.Join(s.Tags, ", ") + "]")
		}
		if s.Description != "" {
			b.WriteString(" — " + s.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// NewLoadSkillTool 返回内核内置的 load_skill 特殊工具。
// 宿主注册它，模型才能在 tools schema 里看到并决定"加载哪个技能"。
// BaseRisk 给 None：加载规范是只读、无副作用操作，不占用审批闸门。
// Execute 是占位：真实处理由 Agent.Run 按 LoadSkillToolName 拦截（特殊通道），
// 不会走到这里。
func NewLoadSkillTool() Tool {
	return Tool{
		Name:        LoadSkillToolName,
		Description: "加载一个技能规范到系统上下文。执行复杂任务前，若发现适用技能请先调用它。参数是 JSON：{\"name\":\"技能名\"}。",
		BaseRisk:    RiskNone,
		Execute: func(input string) (string, error) {
			return "", fmt.Errorf("load_skill 应由内核特殊处理，不应走到普通工具执行")
		},
	}
}

// parseLoadSkillInput 从 load_skill 的参数里提取技能名：兼容
// `{"name":"x"}`（真实模型的 JSON）和裸 `"x"` / `x`（简写）。
// 解析失败返回空串，由调用方回填"参数无效"。
func parseLoadSkillInput(input string) string {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(input), &m) == nil {
			if n, ok := m["name"]; ok {
				return strings.TrimSpace(n)
			}
		}
		return ""
	}
	return strings.Trim(input, `"'`)
}
