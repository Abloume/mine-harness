package agent

import (
	"strings"
	"testing"
)

// 缺口 B · Skill History 短期工作记忆：环形 buffer 记录最近工具调用，
// 压缩时注入快照 system 消息，防「语义重复」的失败路径。

// TestSkillHistoryRingOverwrite 环形覆盖：容量 3 写 5 条，
// 快照只保留最近 3 条且按时间序（最旧 → 最新）。
func TestSkillHistoryRingOverwrite(t *testing.T) {
	h := NewSkillHistory(3)
	for i := 0; i < 5; i++ {
		h.Record(i+1, "read_file", `{"path":"a.go"}`, true)
	}
	snap := h.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("应只保留最近 3 条, got %d", len(snap))
	}
	if snap[0].Step != 3 || snap[1].Step != 4 || snap[2].Step != 5 {
		t.Fatalf("应按时间序保留最新 3 条, got steps %d,%d,%d", snap[0].Step, snap[1].Step, snap[2].Step)
	}
}

// TestSkillHistoryStringFormat 格式化含成败标记与参数摘要。
func TestSkillHistoryStringFormat(t *testing.T) {
	h := NewSkillHistory(4)
	h.Record(1, "read_file", `{"path":"x.go"}`, true)
	h.Record(2, "run_cmd", `{"cmd":"go build ./... --verbose --long-flag --extra-very-long-argument-that-exceeds-limit"}`, false)
	s := h.String()
	if !strings.Contains(s, "step 1: read_file") || !strings.Contains(s, "[成功]") {
		t.Fatalf("成功记录标记不对: %s", s)
	}
	if !strings.Contains(s, "step 2: run_cmd") || !strings.Contains(s, "[失败]") {
		t.Fatalf("失败记录标记不对: %s", s)
	}
	if strings.Contains(s, "exceeds-limit") {
		t.Fatalf("长参数应被截断成摘要: %s", s)
	}
}

// TestSkillHistoryDisabled 容量 <= 0 时禁用：Record 空操作、String 空串。
func TestSkillHistoryDisabled(t *testing.T) {
	h := NewSkillHistory(0)
	h.Record(1, "read_file", "x", true)
	if s := h.String(); s != "" {
		t.Fatalf("禁用时快照应为空, got: %s", s)
	}
}

// TestHistorySnapshotInjectedOnCompact 端到端：低预算 + 大工具结果触发
// 上下文压缩 → 压缩后「已尝试」快照注入 system（模型可见之前调用过什么）。
func TestHistorySnapshotInjectedOnCompact(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name: "big_out",
		Execute: func(input string) (string, error) {
			return strings.Repeat("x", 2000), nil // 大结果撑爆预算
		},
	})
	llm := NewMockLLM([]MockDecision{
		{ToolName: "big_out", ToolInput: `{}`},
		{Content: "完成。"},
	})
	a := NewAgent(llm, reg, WithMaxSteps(4), WithTokenBudget(500))
	res := a.Run("任务")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s: %s", res.Status, res.Reason)
	}
	seen := false
	for _, m := range a.History() {
		if m.Role == roleSystem && strings.Contains(m.Content, "【已尝试】") {
			seen = true
			if !strings.Contains(m.Content, "big_out") {
				t.Fatalf("快照应含工具名 big_out: %s", m.Content)
			}
		}
	}
	if !seen {
		t.Fatal("压缩后应注入 Skill History 快照（含【已尝试】标记）")
	}
}

// TestHistoryRecordsValidationFailureAsFailed 校验失败的工具调用
// 也记为失败（ok=false），后续快照能提示模型该路径不可行。
func TestHistoryRecordsValidationFailureAsFailed(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name: "bad",
		Execute: func(input string) (string, error) {
			return "garbage", nil
		},
		ResultValidator: func(output string) error { return &ringTestError{} },
	})
	a := NewAgent(
		NewMockLLM([]MockDecision{
			{ToolName: "bad", ToolInput: `{}`},
			{Content: "完成。"},
		}),
		reg, WithMaxSteps(4),
	)
	a.Run("任务")
	found := false
	for _, e := range a.skillHistory.Snapshot() {
		if e.Tool == "bad" && !e.OK {
			found = true
		}
	}
	if !found {
		t.Fatal("校验失败应记录为失败（ok=false）")
	}
}

// ringTestError 让校验必然失败（error 接口最小实现）。
type ringTestError struct{}

func (*ringTestError) Error() string { return "校验失败" }
