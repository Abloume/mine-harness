package evolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mine-harness/internal/agent"
	"mine-harness/internal/eval"
)

// ---- Analyze 测试 ----

func TestAnalyzeSkipsPassedCases(t *testing.T) {
	results := []eval.CaseResult{
		{ID: "ok1", Pass: true, Reason: "全部判据通过"},
		{ID: "ok2", Pass: true, Reason: "全部判据通过"},
	}
	got := Analyze(results)
	if len(got) != 0 {
		t.Errorf("通过的用例不应进 FailureRecord，实际 %d 条", len(got))
	}
}

func TestAnalyzeSkipsCorrectlyBlockedNegativeCases(t *testing.T) {
	// 负例被正确拦截（ExpectFail=true 且 Pass=false）不是失败
	results := []eval.CaseResult{
		{ID: "guard", Pass: false, ExpectFail: true, Reason: "安全判据：工具 delete 不应被实际执行"},
	}
	got := Analyze(results)
	if len(got) != 0 {
		t.Errorf("负例正确拦截不应进 FailureRecord，实际 %d 条", len(got))
	}
}

func TestAnalyzeClassifiesToolFailure(t *testing.T) {
	results := []eval.CaseResult{
		{ID: "t1", Pass: false, Reason: "要求实际执行 read_file，但轨迹中未发现"},
	}
	got := Analyze(results)
	if len(got) != 1 {
		t.Fatalf("应产出 1 条记录，实际 %d", len(got))
	}
	if got[0].Category != CategoryTool {
		t.Errorf("应归为 tool 类，实际 %s", got[0].Category)
	}
	if got[0].Severity != SeverityHigh {
		t.Errorf("tool 类失败应为 high，实际 %s", got[0].Severity)
	}
}

func TestAnalyzeClassifiesKnowledgeFailure(t *testing.T) {
	results := []eval.CaseResult{
		{ID: "k1", Pass: false, Reason: `最终回答缺少关键内容 "验证" `},
	}
	got := Analyze(results)
	if got[0].Category != CategoryKnowledge {
		t.Errorf("应归为 knowledge 类，实际 %s", got[0].Category)
	}
}

func TestAnalyzeClassifiesSafetyAsNotEvolvable(t *testing.T) {
	results := []eval.CaseResult{
		{ID: "s1", Pass: false, Reason: "安全判据：工具 delete 不应被实际执行"},
	}
	got := Analyze(results)
	if got[0].Category != CategorySafety {
		t.Errorf("应归为 safety 类，实际 %s", got[0].Category)
	}
}

func TestAnalyzeClassifiesJudgeError(t *testing.T) {
	results := []eval.CaseResult{
		{ID: "j1", Pass: false, Reason: "评审模型调用失败: timeout"},
	}
	got := Analyze(results)
	if got[0].Category != CategoryJudge {
		t.Errorf("应归为 judge 类，实际 %s", got[0].Category)
	}
}

func TestAnalyzeClassifiesIncompleteAsClarification(t *testing.T) {
	results := []eval.CaseResult{
		{ID: "c1", Pass: false, Reason: "未完成（aborted）: 步数用尽"},
	}
	got := Analyze(results)
	if got[0].Category != CategoryClarification {
		t.Errorf("应归为 clarification 类，实际 %s", got[0].Category)
	}
}

func TestAnalyzeDefaultToStyle(t *testing.T) {
	results := []eval.CaseResult{
		{ID: "x1", Pass: false, Reason: "某种莫名其妙的失败"},
	}
	got := Analyze(results)
	if got[0].Category != CategoryStyle {
		t.Errorf("兜底应为 style 类，实际 %s", got[0].Category)
	}
}

// ---- Diagnose 测试 ----

func TestDiagnoseProducesKnowledgeSuggestion(t *testing.T) {
	failures := []FailureRecord{
		{CaseID: "k1", Category: CategoryKnowledge},
		{CaseID: "k2", Category: CategoryKnowledge},
	}
	got := Diagnose(failures)
	if len(got) != 1 {
		t.Fatalf("knowledge 失败应产出 1 条建议，实际 %d", len(got))
	}
	if got[0].Section != "补充知识点" {
		t.Errorf("section 应为补充知识点，实际 %s", got[0].Section)
	}
	if !strings.Contains(got[0].Content, "2") {
		t.Error("内容里应包含失败数量")
	}
}

func TestDiagnoseProducesMultipleSuggestions(t *testing.T) {
	failures := []FailureRecord{
		{CaseID: "k1", Category: CategoryKnowledge},
		{CaseID: "t1", Category: CategoryTool},
		{CaseID: "t2", Category: CategoryTool},
	}
	got := Diagnose(failures)
	if len(got) != 2 {
		t.Fatalf("knowledge+tool 两类应产出 2 条建议，实际 %d", len(got))
	}
}

func TestDiagnoseIgnoresSafetyAndJudge(t *testing.T) {
	failures := []FailureRecord{
		{CaseID: "s1", Category: CategorySafety},
		{CaseID: "j1", Category: CategoryJudge},
	}
	got := Diagnose(failures)
	if len(got) != 0 {
		t.Errorf("safety/judge 类不应产出建议，实际 %d 条", len(got))
	}
}

func TestDiagnoseEmptyFailures(t *testing.T) {
	got := Diagnose(nil)
	if len(got) != 0 {
		t.Error("空失败列表不应产出建议")
	}
}

// ---- Optimize 测试 ----

func TestOptimizeAppendsWithoutModifyingOriginal(t *testing.T) {
	old := "# 修复 bug\n\n## 流程\n1. 复现\n2. 定位"
	suggestions := []SuggestedChange{
		{Section: "补充知识点", Content: "## 补充知识点（自进化 v1）\n\n- 新知识点"},
	}
	newBody := Optimize(old, suggestions)

	// 原文必须完整保留
	if !strings.Contains(newBody, old) {
		t.Error("改写后的 Skill 必须包含原文完整内容")
	}
	// 追加内容必须存在
	if !strings.Contains(newBody, "补充知识点（自进化 v1）") {
		t.Error("改写后的 Skill 应包含追加的知识点段落")
	}
}

func TestOptimizeNoSuggestionsReturnsOriginal(t *testing.T) {
	old := "# 原始 Skill"
	newBody := Optimize(old, nil)
	if newBody != old {
		t.Error("没有建议时应原样返回")
	}
}

// ---- Evolve 端到端测试 ----

func TestE2EEvolve(t *testing.T) {
	// 构造一个有失败的 Report
	report := eval.Report{
		Results: []eval.CaseResult{
			{ID: "k1", Pass: false, Reason: `最终回答缺少关键内容 "验证"`},
			{ID: "t1", Pass: false, Reason: "要求实际执行 read_file，但轨迹中未发现"},
			{ID: "ok", Pass: true, Reason: "全部判据通过"},
		},
	}

	currentSkill := "# 修复 bug\n\n## 流程\n1. 复现"

	result := Evolve(report, currentSkill, RuleDiagnostician{})

	// 应识别出 2 条失败（knowledge + tool）
	if len(result.Failures) != 2 {
		t.Errorf("应识别 2 条失败，实际 %d", len(result.Failures))
	}
	// 应产出 2 条建议
	if len(result.Suggestions) != 2 {
		t.Errorf("应产出 2 条建议，实际 %d", len(result.Suggestions))
	}
	// 新 Skill 应包含原文
	if !strings.Contains(result.NewSkillBody, currentSkill) {
		t.Error("新 Skill 应包含原文")
	}
	// 新 Skill 应包含追加内容
	if !strings.Contains(result.NewSkillBody, "自进化 v1") {
		t.Error("新 Skill 应包含自进化标记")
	}
}

// ---- EvolveLoop 多轮进化测试 ----

func TestEvolveLoopStopsWhenNoFailures(t *testing.T) {
	// 第一轮有失败，第二轮没失败 → 应提前结束（只跑 2 轮）
	callCount := 0
	runSuite := func() eval.Report {
		callCount++
		if callCount == 1 {
			// 第一轮：有 tool 类失败
			return eval.Report{
				Results:       []eval.CaseResult{{ID: "t1", Pass: false, Reason: "要求实际执行 run_tests，但轨迹中未发现"}},
				PositiveTotal: 1, PositivePassed: 0,
			}
		}
		// 第二轮：全部通过
		return eval.Report{
			Results:       []eval.CaseResult{{ID: "ok", Pass: true, Reason: "全部判据通过"}},
			PositiveTotal: 1, PositivePassed: 1,
		}
	}

	results := EvolveLoop(runSuite, "# v0 skill", 5, nil, RuleDiagnostician{})

	if len(results) != 2 {
		t.Errorf("应跑 2 轮后提前结束，实际 %d 轮", len(results))
	}
	if results[0].Score != 0 {
		t.Errorf("第 1 轮通过率应为 0，实际 %.0f", results[0].Score)
	}
	if results[1].Score != 100 {
		t.Errorf("第 2 轮通过率应为 100，实际 %.0f", results[1].Score)
	}
}

func TestEvolveLoopCallsOnEvolved(t *testing.T) {
	// 验证 onEvolved 回调被正确调用
	var calledRounds []int
	runSuite := func() eval.Report {
		return eval.Report{
			Results:       []eval.CaseResult{{ID: "t1", Pass: false, Reason: "要求实际执行 run_tests，但轨迹中未发现"}},
			PositiveTotal: 1, PositivePassed: 0,
		}
	}

	EvolveLoop(runSuite, "# v0", 3, func(round int, newSkill string) {
		calledRounds = append(calledRounds, round)
		if !strings.Contains(newSkill, "自进化 v1") {
			t.Errorf("回调的 newSkill 应包含自进化标记")
		}
	}, RuleDiagnostician{})

	if len(calledRounds) != 3 {
		t.Errorf("onEvolved 应被调用 3 次（每轮都有失败，跑满 maxRounds），实际 %d 次", len(calledRounds))
	}
	if calledRounds[0] != 1 || calledRounds[1] != 2 || calledRounds[2] != 3 {
		t.Errorf("回调轮次应为 [1, 2, 3]，实际 %v", calledRounds)
	}
}

// ---- LLMDiagnostician 测试 ----

func TestLLMDiagnosticianParsesStructuredJSON(t *testing.T) {
	// mock LLM 返回结构化 JSON → 正确解析出建议
	mockLLM := agent.NewMockLLM([]agent.MockDecision{
		{Content: `{"suggestions":[{"section":"工具调用指引","content":"补充 run_tests 的触发条件：修改代码后必须跑测试验证"}]}`},
	})
	d := NewLLMDiagnostician(mockLLM)

	failures := []FailureRecord{
		{CaseID: "t1", Category: CategoryTool, Severity: SeverityHigh, Detail: "要求实际执行 run_tests，但轨迹中未发现"},
	}
	suggestions, err := d.Diagnose(failures, "# 修复 bug\n\n## 流程")
	if err != nil {
		t.Fatalf("LLM 诊断不应报错: %v", err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("应产出 1 条建议，实际 %d", len(suggestions))
	}
	if suggestions[0].Section != "工具调用指引" {
		t.Errorf("section 应为工具调用指引，实际 %s", suggestions[0].Section)
	}
	if !strings.Contains(suggestions[0].Content, "run_tests") {
		t.Errorf("content 应包含具体建议，实际 %s", suggestions[0].Content)
	}
}

func TestLLMDiagnosticianFallbackOnError(t *testing.T) {
	// mock LLM 返回乱码 → DiagnoseWithFallback 回退到规则版
	mockLLM := agent.NewMockLLM([]agent.MockDecision{
		{Content: "我觉得不太行"}, // 不是 JSON
	})

	failures := []FailureRecord{
		{CaseID: "t1", Category: CategoryTool, Severity: SeverityHigh, Detail: "要求实际执行 run_tests，但轨迹中未发现"},
	}
	suggestions := DiagnoseWithFallback(mockLLM, failures, "# 修复 bug")
	// 回退到规则版：tool 类失败 → 建议"工具调用指引"
	if len(suggestions) != 1 {
		t.Fatalf("回退后应产出 1 条建议，实际 %d", len(suggestions))
	}
	if suggestions[0].Section != "工具调用指引" {
		t.Errorf("回退建议 section 应为工具调用指引，实际 %s", suggestions[0].Section)
	}
}

// ---- SkillStore 持久化测试 ----

func TestSkillStoreSaveAndLoadVersion(t *testing.T) {
	// 用临时目录，不污染项目里的 skills/
	tmp := t.TempDir()
	// 模拟 skills/fix-bug/ 目录结构
	os.MkdirAll(filepath.Join(tmp, "fix-bug"), 0755)

	store := NewSkillStore(tmp)

	// 存 v0 和 v1
	v0Content := "# 修复 bug v0"
	v1Content := "# 修复 bug v1（进化后）"

	if _, err := store.SaveVersion("fix-bug", 0, v0Content); err != nil {
		t.Fatalf("保存 v0 失败: %v", err)
	}
	if _, err := store.SaveVersion("fix-bug", 1, v1Content); err != nil {
		t.Fatalf("保存 v1 失败: %v", err)
	}

	// 读回来
	got0, err := store.LoadVersion("fix-bug", 0)
	if err != nil {
		t.Fatalf("读 v0 失败: %v", err)
	}
	if got0 != v0Content {
		t.Errorf("v0 内容不匹配，实际 %q", got0)
	}

	got1, err := store.LoadVersion("fix-bug", 1)
	if err != nil {
		t.Fatalf("读 v1 失败: %v", err)
	}
	if got1 != v1Content {
		t.Errorf("v1 内容不匹配，实际 %q", got1)
	}
}

func TestSkillStoreListVersions(t *testing.T) {
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "fix-bug"), 0755)
	store := NewSkillStore(tmp)

	// 故意乱序保存
	store.SaveVersion("fix-bug", 2, "v2")
	store.SaveVersion("fix-bug", 0, "v0")
	store.SaveVersion("fix-bug", 1, "v1")

	versions, err := store.ListVersions("fix-bug")
	if err != nil {
		t.Fatalf("列版本失败: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("应有 3 个版本，实际 %d", len(versions))
	}
	// 必须是升序
	if versions[0] != 0 || versions[1] != 1 || versions[2] != 2 {
		t.Errorf("版本应升序 [0,1,2]，实际 %v", versions)
	}
}

func TestSkillStorePromoteToCurrent(t *testing.T) {
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "fix-bug"), 0755)
	// 先写一个初始 SKILL.md
	os.WriteFile(filepath.Join(tmp, "fix-bug", "SKILL.md"), []byte("# v0 原始"), 0644)

	store := NewSkillStore(tmp)
	store.SaveVersion("fix-bug", 1, "# v1 进化后")

	// 把 v1 提升为当前
	if err := store.PromoteToCurrent("fix-bug", 1); err != nil {
		t.Fatalf("提升版本失败: %v", err)
	}

	// 读当前 SKILL.md，应该是 v1 的内容
	current, err := os.ReadFile(filepath.Join(tmp, "fix-bug", "SKILL.md"))
	if err != nil {
		t.Fatalf("读当前 SKILL.md 失败: %v", err)
	}
	if string(current) != "# v1 进化后" {
		t.Errorf("当前版本应为 v1 内容，实际 %q", string(current))
	}
}

// ---- 回归检测测试 ----

func TestEvolveLoopDetectsRegression(t *testing.T) {
	// 模拟：第 1 轮 50%，第 2 轮降到 25% → 应标记 Regressed=true
	callCount := 0
	runSuite := func() eval.Report {
		callCount++
		switch callCount {
		case 1:
			// 第 1 轮：50% 通过率（2 个用例 1 个过）
			return eval.Report{
				Results: []eval.CaseResult{
					{ID: "ok", Pass: true, Reason: "ok"},
					{ID: "fail", Pass: false, Reason: "要求实际执行 run_tests，但轨迹中未发现"},
				},
				PositiveTotal: 2, PositivePassed: 1,
			}
		case 2:
			// 第 2 轮：25% 通过率（1 个过）→ 回归了
			return eval.Report{
				Results: []eval.CaseResult{
					{ID: "ok", Pass: true, Reason: "ok"},
					{ID: "fail", Pass: false, Reason: "要求实际执行 run_tests，但轨迹中未发现"},
					{ID: "fail2", Pass: false, Reason: "要求实际执行 edit_file，但轨迹中未发现"},
					{ID: "fail3", Pass: false, Reason: "未完成"},
				},
				PositiveTotal: 4, PositivePassed: 1,
			}
		default:
			// 后续都全过
			return eval.Report{
				Results:       []eval.CaseResult{{ID: "ok", Pass: true, Reason: "ok"}},
				PositiveTotal: 1, PositivePassed: 1,
			}
		}
	}

	results := EvolveLoop(runSuite, "# v0", 4, nil, RuleDiagnostician{})

	if len(results) < 2 {
		t.Fatalf("应至少跑 2 轮，实际 %d 轮", len(results))
	}

	// 第 1 轮是基线，Delta=0，Regressed=false
	if results[0].Delta != 0 {
		t.Errorf("第 1 轮 Delta 应为 0，实际 %.1f", results[0].Delta)
	}
	if results[0].Regressed {
		t.Error("第 1 轮不应标记回归")
	}

	// 第 2 轮分数从 50% 降到 25% → Delta=-25，Regressed=true
	if !results[1].Regressed {
		t.Error("第 2 轮应标记为回归（分数下降）")
	}
	if results[1].Delta >= 0 {
		t.Errorf("第 2 轮 Delta 应为负，实际 %.1f", results[1].Delta)
	}
}

func TestEvolveLoopDetectsImprovement(t *testing.T) {
	// 模拟：第 1 轮 0%，第 2 轮 100% → 应标记 Regressed=false（提升了）
	callCount := 0
	runSuite := func() eval.Report {
		callCount++
		if callCount == 1 {
			return eval.Report{
				Results:       []eval.CaseResult{{ID: "fail", Pass: false, Reason: "要求实际执行 run_tests，但轨迹中未发现"}},
				PositiveTotal: 1, PositivePassed: 0,
			}
		}
		return eval.Report{
			Results:       []eval.CaseResult{{ID: "ok", Pass: true, Reason: "ok"}},
			PositiveTotal: 1, PositivePassed: 1,
		}
	}

	results := EvolveLoop(runSuite, "# v0", 3, nil, RuleDiagnostician{})

	if len(results) < 2 {
		t.Fatalf("应至少跑 2 轮，实际 %d 轮", len(results))
	}
	// 第 2 轮从 0% 涨到 100% → 不是回归
	if results[1].Regressed {
		t.Error("第 2 轮分数提升，不应标记为回归")
	}
	if results[1].Delta <= 0 {
		t.Errorf("第 2 轮 Delta 应为正，实际 %.1f", results[1].Delta)
	}
}
