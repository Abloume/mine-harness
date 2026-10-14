package evolve

import "mine-harness/internal/eval"

// Evolve 是自进化闭环的主入口：
// 输入一轮评测报告 + 当前 SKILL.md 正文 + 诊断器，输出诊断报告和改写后的新 Skill。
//
// 闭环流程（对应 SkillForge 论文的 creation → evaluation → refinement）：
//
//	1. Analyze：从 Report 中提取失败 Case，做四维归因
//	2. diag.Diagnose：用注入的诊断器产出改写建议（规则版或 LLM 版）
//	3. Optimize：最小修改原则产出新版本
//
// diag 参数：调用方想选规则版就传 RuleDiagnostician{}，
// 想用 LLM 版就传 NewLLMDiagnostician(llm)。
//
// JS/TS ↔ Go 差异：TS 里这种管道常用函数组合（pipe/flow）；
// Go 里直接顺序调用三个函数更直白，类型安全由编译期保证。
func Evolve(report eval.Report, currentSkillBody string, diag Diagnostician) EvolutionReport {
	failures := Analyze(report.Results)
	suggestions, _ := diag.Diagnose(failures, currentSkillBody)
	newBody := Optimize(currentSkillBody, suggestions)

	return EvolutionReport{
		Failures:     failures,
		Suggestions:  suggestions,
		NewSkillBody: newBody,
	}
}
