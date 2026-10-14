package evolve

import (
	"strings"

	"mine-harness/internal/eval"
)

// Analyze 把评测报告里的失败 CaseResult 转成结构化 FailureRecord。
//
// 过滤规则：
//   - Pass=true 的跳过（成功用例不需要归因）
//   - ExpectFail=true 且 Pass=false 的跳过（负例被正确拦截 = 护栏正常，不是失败）
//
// 对应 SkillForge 的 Failure Analyzer：把 Bad Case 按四个维度分类。
// 第一版用规则归因（Reason 字符串匹配），不调 LLM——可复现、零成本。
// 后续可以把 classify 换成 LLMJudge 做更精细的归因。
func Analyze(results []eval.CaseResult) []FailureRecord {
	var out []FailureRecord
	for _, r := range results {
		if r.Pass {
			continue
		}
		// 负例被正确拦截（ExpectFail=true 且 Pass=false）不是失败，是护栏生效
		if r.ExpectFail {
			continue
		}
		out = append(out, classify(r))
	}
	return out
}

// classify 按 Reason 字符串模式把失败分到对应类别。
//
// 匹配顺序很重要：从最具体（安全判据、评审器错误）到最通用（style 兜底）。
// 顺序反了会把"安全判据"误判成普通 tool 失败。
//
// JS/TS ↔ Go 差异：TS 里常用 switch case 加 break；Go 的 switch 默认 break，
// 想贯穿要显式 fallthrough——这里每个 case 都 return，不需要 fallthrough。
func classify(r eval.CaseResult) FailureRecord {
	reason := r.Reason

	switch {
	case strings.Contains(reason, "安全判据"):
		// 安全判据失败 = Agent 做了不该做的事（或该拦的没拦住）
		// 这是审批/护栏的问题，不是 Skill 文本能改的 → diagnostician 会忽略
		return FailureRecord{CaseID: r.ID, Category: CategorySafety, Severity: SeverityHigh, Detail: reason}

	case strings.Contains(reason, "评审模型调用失败") || strings.Contains(reason, "JSON"):
		// LLMJudge 自身出问题（网络挂了/返回乱码），不是 Agent 的锅
		return FailureRecord{CaseID: r.ID, Category: CategoryJudge, Severity: SeverityMedium, Detail: reason}

	case strings.Contains(reason, "要求实际执行") && strings.Contains(reason, "但轨迹中未发现"):
		// RuleJudge 的 RequireTools 失败：该调的工具没调
		return FailureRecord{CaseID: r.ID, Category: CategoryTool, Severity: SeverityHigh, Detail: reason}

	case strings.Contains(reason, "存在不应执行的调用"):
		// RuleJudge 的 MustNotCall 失败：调了不该调的工具
		// 归到 Tool 类（工具使用偏差），但严重度也是 high
		return FailureRecord{CaseID: r.ID, Category: CategoryTool, Severity: SeverityHigh, Detail: reason}

	case strings.Contains(reason, "最终回答缺少关键内容"):
		// RuleJudge 的 RequireAnswer 失败：回答里缺了关键信息
		return FailureRecord{CaseID: r.ID, Category: CategoryKnowledge, Severity: SeverityHigh, Detail: reason}

	case strings.Contains(reason, "未完成"):
		// Agent 没跑完就中止了（步数用尽/异常），可能是任务理解偏差或流程有洞
		return FailureRecord{CaseID: r.ID, Category: CategoryClarification, Severity: SeverityMedium, Detail: reason}

	default:
		// 兜底：风格问题（啰嗦/机械/答非所问但没触发上面任何判据）
		return FailureRecord{CaseID: r.ID, Category: CategoryStyle, Severity: SeverityLow, Detail: reason}
	}
}
