package memory

import (
	"strings"
	"testing"

	"mine-harness/internal/agent"
)

// TestMemoryStoreAddRecall 关键信息优先召回：命中查询词的条目优先、
// 类别权重次之、limit 生效。
func TestMemoryStoreAddRecall(t *testing.T) {
	store, err := NewMemoryStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 三条记忆：两条与 "go" 相关（conclusion 与 detail），一条无关（conclusion）
	adds := []agent.MemoryItem{
		{Content: "结论：项目使用 Go 1.27.1 开发", Category: "conclusion", Source: "s1"},
		{Content: "细节：go.mod 中 go-rod 为间接依赖", Category: "detail", Source: "s1"},
		{Content: "项目根目录为 /learn/mine-harness", Category: "conclusion", Source: "s2"},
	}
	for _, a := range adds {
		if err := store.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	// 查 "go"：命中 2 条，conclusion 排在前
	got, err := store.Recall("Go 版本", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 应返回 2 条, got %d", len(got))
	}
	if got[0].Content != adds[0].Content {
		t.Fatalf("conclusion 命中应排最前, got %q", got[0].Content)
	}
	// 查无关词：无命中，仍按类别权重优先返回 conclusion（detail 被压后）
	got2, _ := store.Recall("root", 1)
	if len(got2) != 1 || got2[0].Category != "conclusion" {
		t.Fatalf("无命中时结论类应优先, got %+v", got2)
	}
}

// TestAddDedup 同一 (Content, Category) 重复沉淀只保留一条（幂等）。
func TestAddDedup(t *testing.T) {
	store, _ := NewMemoryStore(t.TempDir())
	item := agent.MemoryItem{Content: "结论 X", Category: "conclusion", Source: "s1"}
	for i := 0; i < 3; i++ {
		if err := store.Add(item); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.List()); got != 1 {
		t.Fatalf("重复沉淀应去重为 1 条, got %d", got)
	}
}

// TestMemoryStorePersist 落盘往返：Add 后新实例能加载（长期记忆跨进程存活）。
func TestMemoryStorePersist(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewMemoryStore(dir)
	if err := s1.Add(agent.MemoryItem{Content: "结论：Go 1.27.1", Category: "conclusion", Source: "s1"}); err != nil {
		t.Fatal(err)
	}
	s2, err := NewMemoryStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.List()
	if len(got) != 1 || got[0].Content != "结论：Go 1.27.1" || got[0].Category != "conclusion" {
		t.Fatalf("新实例应加载既有记忆, got %+v", got)
	}
	if got[0].ID == "" || got[0].CreatedAt.IsZero() {
		t.Fatalf("记忆条目应含 ID 与时间, got %+v", got[0])
	}
}

// TestHeuristicExtractor 启发式提取：取最后一条 assistant 非工具回答为
// conclusion；无回答返回空；超长截断。
func TestHeuristicExtractor(t *testing.T) {
	ex := NewHeuristicExtractor()
	msgs := []agent.Message{
		{Role: "assistant", Content: "调用工具", ToolCalls: []agent.ToolCall{{ID: "c1"}}},
		{Role: "tool", Content: "go 1.27.1", ToolCallID: "c1"},
		{Role: "assistant", Content: "  结论：Go 1.27.1  "},
	}
	items := ex.Extract(msgs, "s1")
	if len(items) != 1 || items[0].Category != "conclusion" {
		t.Fatalf("应提取 1 条 conclusion, got %+v", items)
	}
	if items[0].Content != "结论：Go 1.27.1" || !strings.Contains(items[0].Source, "s1") {
		t.Fatalf("内容/来源不对, got %+v", items[0])
	}

	// 无最终回答（全工具调用）→ 空
	msgs2 := []agent.Message{{Role: "assistant", Content: "x", ToolCalls: []agent.ToolCall{{ID: "c"}}}}
	if got := ex.Extract(msgs2, "s2"); got != nil {
		t.Fatalf("无最终回答应返回空, got %+v", got)
	}

	// 超长截断
	long := strings.Repeat("结", 300)
	items3 := ex.Extract([]agent.Message{{Role: "assistant", Content: long}}, "s3")
	if len(items3) != 1 || len(items3[0].Content) >= len(long) {
		t.Fatalf("超长记忆应截断, got %d chars", len(items3[0].Content))
	}
}
