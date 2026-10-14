package evolve

import "mine-harness/internal/eval"

// RoundResult 记录一轮进化的完整结果。
//
// 对应 SkillForge 论文里"每轮迭代后分数单调递增"的实验数据点。
type RoundResult struct {
	Round        int               // 第几轮（从 1 开始）
	Score        float64           // 这轮评测的通过率（0-100）
	Failures     []FailureRecord   // 这轮收集到的失败
	Suggestions  []SuggestedChange // 这轮诊断出的改写建议
	NewSkillBody string            // 进化后的 Skill 全文
}

// EvolveLoop 跑多轮自进化，直到没有失败或达到 maxRounds。
//
// 流程：
//  1. runSuite() 跑一次评测
//  2. Evolve() 产出新 Skill
//  3. 调用 onEvolved 回调（调用方在这里切换自己的 Skill 版本状态）
//  4. 如果还有失败且没到 maxRounds，回到第 1 步
//
// onEvolved 参数：每轮进化后的回调，传 (轮次, 新 Skill 全文)。
// 调用方在这里做"切换 Skill 版本"的动作——比如 demo 里改 skillVersion 变量。
// 传 nil 表示不回调。
//
// JS/TS ↔ Go 差异：TS 里回调常用箭头函数闭包；Go 同样用闭包，
// 但要注意闭包捕获的变量是引用（跟 JS 一致），不是值拷贝。
func EvolveLoop(
	runSuite func() eval.Report,
	initialSkill string,
	maxRounds int,
	onEvolved func(round int, newSkill string),
) []RoundResult {
	results := []RoundResult{}
	currentSkill := initialSkill

	for i := 0; i < maxRounds; i++ {
		report := runSuite()
		evo := Evolve(report, currentSkill)

		results = append(results, RoundResult{
			Round:        i + 1,
			Score:        report.PassRate(),
			Failures:     evo.Failures,
			Suggestions:  evo.Suggestions,
			NewSkillBody: evo.NewSkillBody,
		})

		currentSkill = evo.NewSkillBody

		// 没有失败了 → 提前结束（已经收敛）
		if len(evo.Failures) == 0 {
			break
		}

		// 通知调用方：这轮进化完了，可以切换 Skill 版本了
		if onEvolved != nil {
			onEvolved(i+1, currentSkill)
		}
	}

	return results
}
