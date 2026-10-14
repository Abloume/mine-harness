package evolve

import (
	"strings"
	"testing"

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

	result := Evolve(report, currentSkill)

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
