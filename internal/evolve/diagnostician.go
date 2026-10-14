package evolve

import "fmt"

// Diagnose 聚合失败记录，产出对 SKILL.md 的改写建议。
//
// 对应 SkillForge 的 Skill Diagnostician：把单个 Bad Case 的失败
// 聚合成系统性规律，再映射到 Skill 的具体段落。
//
// 第一版粒度：按类别计数，哪类失败多就追加哪类指引。
// 不做"精确到某一行"的诊断——那需要 LLM 读 SKILL.md 全文，
// 第一版保持规则化、可测试。
//
// Safety（护栏问题）和 Judge（评审器问题）不产出建议——
// 那些不是改 Skill 文本能解决的。
func Diagnose(failures []FailureRecord) []SuggestedChange {
	counts := countByCategory(failures)

	var suggestions []SuggestedChange

	if n := counts[CategoryKnowledge]; n > 0 {
		suggestions = append(suggestions, SuggestedChange{
			Section: "补充知识点",
			Content: fmt.Sprintf(`## 补充知识点（自进化 v1）

根据 %d 个失败案例，模型在领域知识上存在缺失：
- 检查并补充常见报错对应的处理方法
- 确认关键流程步骤是否完整描述
- 核对易混淆的概念边界`, n),
		})
	}

	if n := counts[CategoryTool]; n > 0 {
		suggestions = append(suggestions, SuggestedChange{
			Section: "工具调用指引",
			Content: fmt.Sprintf(`## 工具调用指引（自进化 v1）

根据 %d 个失败案例，模型在工具调用上存在偏差：
- 明确什么场景下必须调用哪些工具（触发条件）
- 补充工具调用的参数规范
- 说明工具返回结果后应该怎么解读`, n),
		})
	}

	if n := counts[CategoryClarification]; n > 0 {
		suggestions = append(suggestions, SuggestedChange{
			Section: "澄清策略",
			Content: fmt.Sprintf(`## 澄清策略（自进化 v1）

根据 %d 个失败案例，模型在澄清提问上存在偏差：
- 明确什么信息缺失时需要向用户确认
- 避免重复询问用户已提供的信息
- 确认任务理解后再动手，不盲目开干`, n),
		})
	}

	if n := counts[CategoryStyle]; n > 0 {
		suggestions = append(suggestions, SuggestedChange{
			Section: "回答风格指引",
			Content: fmt.Sprintf(`## 回答风格指引（自进化 v1）

根据 %d 个失败案例，模型在回答风格上存在问题：
- 回答应简洁直接，结论先行
- 避免过度啰嗦和机械套话
- 该给结论时给结论，不要绕圈子`, n),
		})
	}

	return suggestions
}

// countByCategory 统计每类失败的数量。
//
// JS/TS ↔ Go 差异：JS 用对象字面量当 map（counts[cat] = (counts[cat]||0)+1）；
// Go 用 make(map[Category]int)，零值就是 0，不用 || 兜底。
func countByCategory(failures []FailureRecord) map[Category]int {
	counts := make(map[Category]int)
	for _, f := range failures {
		counts[f.Category]++
	}
	return counts
}

// RuleDiagnostician 是规则版诊断器——按类别计数追加通用模板。
//
// 它实现 Diagnostician 接口，跟 LLMDiagnostician 可互换。
// 零成本、确定性、不依赖外部服务——适合单元测试和兜底。
//
// JS/TS ↔ Go 差异：TS 里常用 class 实现策略接口；
// Go 用空结构体 + 方法集，零值就能用，不需要构造函数。
type RuleDiagnostician struct{}

// Diagnose 实现 Diagnostician 接口：调用原有的规则版 Diagnose 函数。
func (RuleDiagnostician) Diagnose(failures []FailureRecord, currentSkill string) ([]SuggestedChange, error) {
	// currentSkill 在规则版里用不上（它只按类别计数），
	// 但接口签名要求传，这里忽略——跟 LLM 版的区别就是 LLM 会读 Skill 内容。
	return Diagnose(failures), nil
}
