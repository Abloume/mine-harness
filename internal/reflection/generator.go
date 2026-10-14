package reflection

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StandardGenerator 是标准生成器——让 LLM 根据任务自动生成检查标准。
//
// 设计思路：
//   1. 用户给任务 + 特殊要求
//   2. LLM 先想清楚：这个任务验收标准是什么？
//   3. 输出结构化的检查清单（必过项 + 质量项）
//   4. 用户确认后，按这个清单做任务 + 自检
//
// 这就是"标准驱动"——先想清楚什么叫"好"，再动手做。
type StandardGenerator struct {
	llm StandardGenLLM
}

// StandardGenLLM 是标准生成器依赖的 LLM 接口。
type StandardGenLLM interface {
	Generate(prompt string) (string, error)
}

// NewStandardGenerator 创建一个标准生成器。
func NewStandardGenerator(llm StandardGenLLM) *StandardGenerator {
	return &StandardGenerator{llm: llm}
}

// GeneratedStandards 是 LLM 生成的检查标准。
type GeneratedStandards struct {
	TaskType string           `json:"task_type"` // 任务类型
	Summary  string           `json:"summary"`   // 一句话总结
	Required []CheckStandard `json:"required"`  // 必过项
	Quality  []CheckStandard `json:"quality"`   // 质量项
}

// Generate 根据任务描述生成检查标准。
//
// 参数：
//   - task：用户的原始任务描述
//   - userNotes：用户特别强调的要求（可选）
func (g *StandardGenerator) Generate(task string, userNotes string) (*GeneratedStandards, error) {
	prompt := g.buildPrompt(task, userNotes)

	raw, err := g.llm.Generate(prompt)
	if err != nil {
		return nil, fmt.Errorf("调用 LLM 失败: %w", err)
	}

	// 清理 markdown 代码块
	cleaned := cleanJSON(raw)

	var result GeneratedStandards
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("解析生成结果失败: %w\n原始输出: %s", err, raw)
	}

	return &result, nil
}

// buildPrompt 构造生成标准的 prompt。
func (g *StandardGenerator) buildPrompt(task string, userNotes string) string {
	var sb strings.Builder

	sb.WriteString("你是一个资深的技术评审专家。请根据下面的任务，帮用户列出验收标准——也就是做到什么程度算合格。\n\n")

	sb.WriteString("## 用户任务\n")
	sb.WriteString(task)
	sb.WriteString("\n\n")

	if userNotes != "" {
		sb.WriteString("## 用户特别强调\n")
		sb.WriteString(userNotes)
		sb.WriteString("\n\n")
	}

	sb.WriteString("## 输出格式\n")
	sb.WriteString("严格输出 JSON：\n")
	sb.WriteString("```json\n")
	sb.WriteString(`{
  "task_type": "任务类型（一句话）",
  "summary": "这个任务的核心验收点（一句话）",
  "required": [
    {
      "id": "req-1",
      "dimension": "正确性/完整性/性能/安全/兼容性",
      "description": "标准描述",
      "pass_if": "怎么算通过（可检查的具体标准）"
    }
  ],
  "quality": [
    {
      "id": "q-1",
      "dimension": "规范性/可维护性/体验",
      "description": "标准描述",
      "pass_if": "怎么算通过"
    }
  ]
}`)
	sb.WriteString("\n```\n\n")

	sb.WriteString("## 要求\n")
	sb.WriteString("1. 必过项（required）：缺了就不合格的底线要求，3-5 条\n")
	sb.WriteString("2. 质量项（quality）：有更好、没有也能过的加分项，2-4 条\n")
	sb.WriteString("3. 每条标准必须是**可检查的**——不能是\"写得好一点\"这种模糊话\n")
	sb.WriteString("4. pass_if 要具体——比如\"有 WHERE 条件\"，不是\"性能好\"\n")
	sb.WriteString("5. 结合任务特点——前端任务考虑性能/兼容性/交互，后端任务考虑性能/安全/稳定性\n")

	return sb.String()
}

// ReflectorFromStandards 把生成的标准转成一个 Reflector。
//
// 用法：
//   generator := NewStandardGenerator(llm)
//   standards, _ := generator.Generate("写个登录页面", "要兼容 Chrome 最新版")
//   reflector := ReflectorFromStandards(standards)
//   result := reflector.Check(answer, task, nil)
func ReflectorFromStandards(s *GeneratedStandards) Reflector {
	return &customReflector{standards: s}
}

// customReflector 是用自定义标准的 Reflector 实现。
type customReflector struct {
	standards *GeneratedStandards
}

// Check 实现 Reflector 接口。
func (r *customReflector) Check(answer string, task string, history []Message) Result {
	var issues []Issue
	requiredPassed := 0
	qualityPassed := 0

	answerLower := strings.ToLower(answer)

	// 检查必过项
	for _, std := range r.standards.Required {
		if checkStandard(std, answerLower, task) {
			requiredPassed++
		} else {
			issues = append(issues, Issue{
				Severity: "high",
				Standard: std.Description,
				Message:  fmt.Sprintf("必过项未达标：%s", std.Description),
			})
		}
	}

	// 检查质量项
	for _, std := range r.standards.Quality {
		if checkStandard(std, answerLower, task) {
			qualityPassed++
		} else {
			issues = append(issues, Issue{
				Severity: "medium",
				Standard: std.Description,
				Message:  fmt.Sprintf("质量项未达标：%s", std.Description),
			})
		}
	}

	// 判定
	totalRequired := len(r.standards.Required)
	totalQuality := len(r.standards.Quality)
	allRequiredPassed := requiredPassed == totalRequired

	qualityRatio := 0
	if totalQuality > 0 {
		qualityRatio = (qualityPassed * 100) / totalQuality
	}

	passed := allRequiredPassed && qualityRatio >= 50

	totalChecks := totalRequired + totalQuality
	totalPassed := requiredPassed + qualityPassed
	score := 0
	if totalChecks > 0 {
		score = (totalPassed * 100) / totalChecks
	}

	return Result{
		Passed:      passed,
		Score:       score,
		Issues:      issues,
		TotalChecks: totalChecks,
	}
}

// cleanJSON 清理模型返回的 JSON——去掉 markdown 代码块标记。
func cleanJSON(raw string) string {
	s := strings.TrimSpace(raw)

	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
	}

	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}

	return strings.TrimSpace(s)
}
