package agent

import (
	"strings"
	"testing"
)

// 断点续跑（第 5 步阶段一）：WithInitialHistory 恢复上次 Run 的历史，
// 新 Run 只追加 user 消息，不重建 system 首条。

// TestRunWithInitialHistory 初始历史 + 新任务：历史包含上次对话内容，
// system 首条只有一条（不重复注入），新 user 消息在末尾。
func TestRunWithInitialHistory(t *testing.T) {
	reg := NewRegistry()
	llm := NewMockLLM([]MockDecision{
		{Content: "完成。"},
	})
	initial := []Message{
		{Role: roleSystem, Content: "You are a helpful agent."},
		{Role: roleUser, Content: "上次任务"},
		{Role: roleAssistant, Content: "上次结论：Go 1.27.1"},
	}
	a := NewAgent(llm, reg, WithMaxSteps(3), WithInitialHistory(initial))
	res := a.Run("确认上次结论")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s: %s", res.Status, res.Reason)
	}
	h := a.History()
	if len(h) != len(initial)+2 { // 初始 + 新 user 任务 + 最终回答
		t.Fatalf("历史应为初始 + 2, got %d", len(h))
	}
	// system 首条只保留一条
	sysCount := 0
	for _, m := range h {
		if m.Role == roleSystem {
			sysCount++
		}
	}
	if sysCount != 1 {
		t.Fatalf("system 首条不应重复注入, got %d 条", sysCount)
	}
	// 新 user 任务紧随初始历史之后，旧对话内容完整保留
	if h[len(initial)].Role != roleUser || h[len(initial)].Content != "确认上次结论" {
		t.Fatalf("初始历史后应是新任务, got %+v", h[len(initial)])
	}
	if !strings.Contains(h[2].Content, "上次结论") {
		t.Fatalf("上次对话应保留, got %+v", h[2])
	}
}

// TestRunWithoutInitialHistory 不传初始历史时行为不变（向后兼容）：
// 仍是 system + user 起步。
func TestRunWithoutInitialHistory(t *testing.T) {
	reg := NewRegistry()
	a := NewAgent(NewMockLLM([]MockDecision{{Content: "好"}}), reg, WithMaxSteps(2))
	a.Run("新任务")
	h := a.History()
	if len(h) != 3 { // system + user + assistant
		t.Fatalf("默认应 system+user 起步, got %d 条", len(h))
	}
	if h[0].Role != roleSystem || h[1].Role != roleUser {
		t.Fatalf("起始角色不对: %+v", h[:2])
	}
}

// TestInitialHistoryNotMutated Run 内对初始历史的追加不污染外部切片。
func TestInitialHistoryNotMutated(t *testing.T) {
	reg := NewRegistry()
	initial := []Message{{Role: roleSystem, Content: "s"}, {Role: roleUser, Content: "u"}}
	a := NewAgent(NewMockLLM([]MockDecision{{Content: "好"}}), reg,
		WithMaxSteps(2), WithInitialHistory(initial))
	a.Run("x")
	if len(initial) != 2 {
		t.Fatalf("外部初始历史被修改, got %d 条", len(initial))
	}
}
