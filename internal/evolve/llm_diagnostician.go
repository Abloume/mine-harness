package evolve

import (
	"encoding/json"
	"fmt"
	"strings"

	"mine-harness/internal/agent"
)

// LLMDiagnostician 是基于 LLM 的诊断器：
// 让模型读失败记录 + 当前 SKILL.md，产出更精准的改写建议。
//
// 跟规则版 Diagnose 的区别：
//   - 规则版：按类别计数，追加通用模板段落（粗粒度）
//   - LLM 版：模型读具体失败案例和 Skill 原文，给出针对性的修改建议
//
// JS/TS ↔ Go 差异：TS 里这种"模型当评审员"的模式常用高阶函数注入依赖；
// Go 用接口注入（LLMDiagnostician 持有 agent.LLM），编译期类型安全。
type LLMDiagnostician struct {
	llm agent.LLM
}

// NewLLMDiagnostician 构造 LLM 诊断器。
func NewLLMDiagnostician(llm agent.LLM) *LLMDiagnostician {
	return &LLMDiagnostician{llm: llm}
}

// llmSuggestion 是 LLM 应返回的结构化建议（JSON 契约）。
type llmSuggestion struct {
	Section string `json:"section"`
	Content string `json:"content"`
}

// llmResponse 是 LLM 应返回的完整结构。
type llmResponse struct {
	Suggestions []llmSuggestion `json:"suggestions"`
}

// diagnosePrompt 要求模型输出结构化 JSON：
// 输入失败列表和 Skill 原文，输出每条改写建议（段落类别 + 具体内容）。
const diagnosePrompt = `你是 Skill 诊断专家。下面是 Agent 执行失败的记录和当前 Skill 正文。
请分析失败原因，给出针对性的改写建议。

只输出 JSON（不要多余文字）：
{"suggestions": [{"section": "段落类别", "content": "要追加的具体内容"}]}

section 可选值：补充知识点 / 工具调用指引 / 澄清策略 / 回答风格指引
content 要具体，不要套话——指出缺了什么、应该加什么。
`

// Diagnose 用 LLM 做精准诊断。失败时回退到规则版 Diagnose。
func (d *LLMDiagnostician) Diagnose(failures []FailureRecord, currentSkillBody string) ([]SuggestedChange, error) {
	if len(failures) == 0 {
		return nil, nil
	}

	// 拼失败记录摘要
	var failLines []string
	for _, f := range failures {
		failLines = append(failLines, fmt.Sprintf("- [%s/%s] %s: %s", f.Category, f.Severity, f.CaseID, f.Detail))
	}

	userContent := fmt.Sprintf("失败记录：\n%s\n\n当前 Skill 正文：\n%s",
		strings.Join(failLines, "\n"), currentSkillBody)

	resp, err := d.llm.Chat([]agent.Message{
		{Role: "system", Content: diagnosePrompt},
		{Role: "user", Content: userContent},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("诊断 LLM 调用失败: %w", err)
	}

	// 提取 JSON
	start := strings.Index(resp.Content, "{")
	end := strings.LastIndex(resp.Content, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("诊断 LLM 未返回可解析的 JSON")
	}

	var lr llmResponse
	if err := json.Unmarshal([]byte(resp.Content[start:end+1]), &lr); err != nil {
		return nil, fmt.Errorf("诊断 JSON 解析失败: %w", err)
	}

	out := make([]SuggestedChange, 0, len(lr.Suggestions))
	for _, s := range lr.Suggestions {
		out = append(out, SuggestedChange{
			Section: s.Section,
			Content: s.Content,
		})
	}
	return out, nil
}

// DiagnoseWithFallback 是 LLM 诊断的安全版本：
// LLM 调用失败时自动回退到规则版 Diagnose。
//
// 生产环境推荐用这个——模型偶尔抽风不应该搞挂整个闭环。
func DiagnoseWithFallback(llm agent.LLM, failures []FailureRecord, currentSkillBody string) []SuggestedChange {
	d := NewLLMDiagnostician(llm)
	suggestions, err := d.Diagnose(failures, currentSkillBody)
	if err != nil {
		// 回退到规则版
		return Diagnose(failures)
	}
	return suggestions
}
