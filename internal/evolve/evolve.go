package evolve

import "mine-harness/internal/eval"

// Evolve 是自进化闭环的主入口：
// 输入一轮评测报告 + 当前 SKILL.md 正文，输出诊断报告和改写后的新 Skill。
//
// 闭环流程（对应 SkillForge 论文的 creation → evaluation → refinement）：
//
//	1. Analyze：从 Report 中提取失败 Case，做四维归因
//	2. Diagnose：聚合失败，映射到 SKILL.md 的改写点
//	3. Optimize：最小修改原则产出新版本
//
// 为什么是纯函数式：Evolve 不负责"重新跑评测"——它只做"从报告到新 Skill"的转换。
// 重新跑评测、对比分数由调用方（demo 或上层编排）决定。
// 这样 evolve 包不耦合 Agent 构造、Skill 注入等细节，好测试。
//
// JS/TS ↔ Go 差异：TS 里这种管道常用函数组合（pipe/flow）；
// Go 里直接顺序调用三个函数更直白，类型安全由编译期保证。
func Evolve(report eval.Report, currentSkillBody string) EvolutionReport {
	failures := Analyze(report.Results)
	suggestions := Diagnose(failures)
	newBody := Optimize(currentSkillBody, suggestions)

	return EvolutionReport{
		Failures:     failures,
		Suggestions:  suggestions,
		NewSkillBody: newBody,
	}
}
