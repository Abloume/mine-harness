// Package evolve 实现"自进化 Skill"闭环：
// 从评测失败（Bad Case）中归因 → 诊断 Skill 缺陷 → 自动改写 Skill 文本。
//
// 灵感来自阿里 SkillForge 论文（arXiv:2604.08618），在本项目中做最小切片：
//   - 不调 LLM，全规则归因（可复现、零成本、便于学习）
//   - 不改写原有段落，只追加（最小修改原则，防回归）
//   - Safety（护栏）和 Judge（评审器自身）类失败不进 Skill 改写
//
// JS/TS ↔ Go 差异：TS 里"分类"常用 string union 或 enum；
// Go 用自定义 string 类型 + 常量，编译期类型安全，运行时就是字符串。
package evolve

// Category 是失败归因的四个维度（对应 SkillForge 的四维分析）。
// 额外两个类别用于过滤不该进 Skill 改写的失败。
type Category string

const (
	CategoryKnowledge     Category = "knowledge"      // 知识缺失/错误/矛盾
	CategoryTool          Category = "tool"           // 工具该调没调/参数错/结果理解错
	CategoryClarification Category = "clarification" // 该问的没问/不该问的瞎问
	CategoryStyle         Category = "style"           // 语气机械/太啰嗦/太冷
	CategorySafety        Category = "safety"         // 护栏问题（审批/安全判据），不是 Skill 的锅
	CategoryJudge         Category = "judge"          // 评审器自身问题（LLM 调用失败/JSON 解析失败）
)

// Severity 是失败严重度。
type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

// FailureRecord 是一条结构化失败记录。
//
// eval.CaseResult.Reason 只是给人看的句子，机器没法聚合；
// FailureRecord 把它转成"类别 + 严重度 + 摘要"，诊断器才能统计规律。
type FailureRecord struct {
	CaseID   string   // 哪个用例失败了
	Category Category // 四维归因的哪一类
	Severity Severity // 严重度
	Detail   string   // 原始 Reason 摘要（保留上下文）
}

// SuggestedChange 是诊断器产出的一条改写建议。
//
// Section 标记改哪类段落（不强制精确到 SKILL.md 的某个 ## 标题，
// 第一版粒度粗一点，够 demo 用）。
type SuggestedChange struct {
	Section string // 段落类别：补充知识点 / 工具调用指引 / 澄清策略 / 回答风格指引
	Content string // 要追加的 Markdown 内容
}

// EvolutionReport 是一轮自进化的完整输出。
//
// 注意：这里不包含 BeforeScore/AfterScore——Evolve 只负责"从报告到新 Skill"，
// 分数对比由调用方（demo）在重新跑一遍评测后自己算。
// 这样 evolve 包保持纯函数式，不耦合"怎么把 Skill 注入 Agent"的细节。
type EvolutionReport struct {
	Failures     []FailureRecord   // 本轮收集到的结构化失败
	Suggestions  []SuggestedChange // 诊断出的改写建议
	NewSkillBody string            // 改写后的 SKILL.md 全文（v_n+1）
}
