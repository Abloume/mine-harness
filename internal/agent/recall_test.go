package agent

import (
	"strings"
	"testing"
)

// 第 5 步③ · 记忆分层：召回注入 + 沉淀。用内存 stub 实现 MemoryStore /
// MemoryExtractor（agent 只依赖窄接口，测试不需要 memory 包）。

// stubMemory 是内存版 MemoryStore（测试替身：Add 收集、Recall 返回全部）。
type stubMemory struct {
	items []MemoryItem
}

func (s *stubMemory) Add(item MemoryItem) error {
	for _, e := range s.items {
		if e.Content == item.Content && e.Category == item.Category {
			return nil
		}
	}
	s.items = append(s.items, item)
	return nil
}

func (s *stubMemory) Recall(query string, limit int) ([]MemoryItem, error) {
	out := make([]MemoryItem, 0, limit)
	for _, it := range s.items {
		if len(out) >= limit {
			break
		}
		out = append(out, it)
	}
	return out, nil
}

// stubExtractor 返回固定条目的提取器（测试用）。
type stubExtractor struct{ items []MemoryItem }

func (e *stubExtractor) Extract(msgs []Message, sessionID string) []MemoryItem {
	return e.items
}

// TestRecallInjected Run 开始时按任务召回记忆并注入 system 上下文
// （关键信息优先：只召回 limit 条，不恢复全部历史）。
func TestRecallInjected(t *testing.T) {
	store := &stubMemory{items: []MemoryItem{
		{Content: "结论：Go 1.27.1", Category: "conclusion", Source: "s1"},
		{Content: "细节：rod 间接依赖", Category: "detail", Source: "s1"},
	}}
	reg := NewRegistry()
	llm := NewMockLLM([]MockDecision{{Content: "完成。"}})
	a := NewAgent(llm, reg, WithMaxSteps(3),
		WithMemoryStore(store), WithMemoryExtractor(&stubExtractor{}),
		WithMemoryRecallLimit(1))
	a.Run("确认 Go 版本")

	found := false
	for _, m := range a.History() {
		if m.Role == roleSystem && strings.Contains(m.Content, "【记忆召回】") {
			found = true
			if !strings.Contains(m.Content, "Go 1.27.1") {
				t.Fatalf("召回注入应含记忆内容, got %q", m.Content)
			}
			if strings.Contains(m.Content, "rod") {
				t.Fatal("limit=1 不应注入第二条记忆")
			}
		}
	}
	if !found {
		t.Fatal("Run 应注入记忆召回 system 消息")
	}
}

// TestNoRecallWhenStoreNil 未配置记忆存储时历史干净（向后兼容）。
func TestNoRecallWhenStoreNil(t *testing.T) {
	reg := NewRegistry()
	a := NewAgent(NewMockLLM([]MockDecision{{Content: "好"}}), reg, WithMaxSteps(2))
	a.Run("任务")
	for _, m := range a.History() {
		if strings.Contains(m.Content, "【记忆召回】") {
			t.Fatal("未配置记忆存储不应注入召回")
		}
	}
}

// TestMemorySettledOnComplete completed 时提取器产出的关键记忆沉淀进 store。
func TestMemorySettledOnComplete(t *testing.T) {
	store := &stubMemory{}
	ex := &stubExtractor{items: []MemoryItem{
		{Content: "结论：Go 1.27.1", Category: "conclusion", Source: "s1"},
	}}
	reg := NewRegistry()
	a := NewAgent(NewMockLLM([]MockDecision{{Content: "完成。"}}), reg, WithMaxSteps(3),
		WithMemoryStore(store), WithMemoryExtractor(ex))
	res := a.Run("调研")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s", res.Status)
	}
	if len(store.items) != 1 || store.items[0].Content != "结论：Go 1.27.1" {
		t.Fatalf("completed 应沉淀关键记忆, got %+v", store.items)
	}
}

// TestMemoryNotSettledOnAbort 中止/取消的会话不沉淀（无可靠结论不污染长期记忆）。
func TestMemoryNotSettledOnAbort(t *testing.T) {
	store := &stubMemory{}
	ex := &stubExtractor{items: []MemoryItem{
		{Content: "半成品结论", Category: "conclusion", Source: "s1"},
	}}
	reg := NewRegistry()
	// 永远调工具不收敛 → 步数耗尽 aborted
	steps := make([]MockDecision, 0, 2)
	for i := 0; i < 2; i++ {
		steps = append(steps, MockDecision{ToolName: "read_x", ToolInput: `{"i":"1"}`})
	}
	reg.Register(Tool{Name: "read_x", Execute: func(string) (string, error) { return "x", nil }})
	a := NewAgent(NewMockLLM(steps), reg, WithMaxSteps(2),
		WithMemoryStore(store), WithMemoryExtractor(ex))
	res := a.Run("不收敛任务")
	if res.Status != StatusAborted {
		t.Fatalf("应 aborted, got %s", res.Status)
	}
	if len(store.items) != 0 {
		t.Fatalf("aborted 不应沉淀记忆, got %+v", store.items)
	}
}
