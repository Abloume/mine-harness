package memory

import (
	"strings"
	"testing"
	"time"

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
	items := ex.Extract(msgs, nil, "s1")
	if len(items) != 1 || items[0].Category != "conclusion" {
		t.Fatalf("应提取 1 条 conclusion, got %+v", items)
	}
	if items[0].Content != "结论：Go 1.27.1" || !strings.Contains(items[0].Source, "s1") {
		t.Fatalf("内容/来源不对, got %+v", items[0])
	}

	// 无最终回答（全工具调用）→ 空
	msgs2 := []agent.Message{{Role: "assistant", Content: "x", ToolCalls: []agent.ToolCall{{ID: "c"}}}}
	if got := ex.Extract(msgs2, nil, "s2"); got != nil {
		t.Fatalf("无最终回答应返回空, got %+v", got)
	}

	// 超长截断
	long := strings.Repeat("结", 300)
	items3 := ex.Extract([]agent.Message{{Role: "assistant", Content: long}}, nil, "s3")
	if len(items3) != 1 || len(items3[0].Content) >= len(long) {
		t.Fatalf("超长记忆应截断, got %d chars", len(items3[0].Content))
	}
}

// TestEventExtractor 事件信号提取（改进一）：非只读且成功的动作 → decision；
// 只读/失败的动作不产出；组合提取器（DefaultExtractor）两条通道都收。
func TestEventExtractor(t *testing.T) {
	ex := NewEventExtractor()
	ledger := []agent.StepRecord{
		{Step: 1, Name: "read_file", Input: `{"path":"go.mod"}`, SideEffect: agent.SideReadonly, OK: true},
		{Step: 2, Name: "write_file", Input: `{"path":"memory/demo.md","content":"...long..."}`, SideEffect: agent.SideIrreversible, OK: true},
		{Step: 3, Name: "rm_file", Input: `{"path":"x"}`, SideEffect: agent.SideIrreversible, OK: false}, // 失败不产出
	}
	items := ex.Extract(nil, ledger, "s1")
	if len(items) != 1 {
		t.Fatalf("只应有 1 条 decision（step2）, got %+v", items)
	}
	if items[0].Category != "decision" || !strings.Contains(items[0].Content, "write_file") {
		t.Fatalf("事件记忆应为 decision/write_file, got %+v", items[0])
	}
	if !strings.Contains(items[0].Source, "step2") {
		t.Fatalf("来源应定位到账本步骤, got %q", items[0].Source)
	}

	// 组合提取器：回答 → conclusion + 事件 → decision
	def := NewDefaultExtractor()
	msgs := []agent.Message{{Role: "assistant", Content: "结论：已写入 memory/demo.md"}}
	combined := def.Extract(msgs, ledger, "s1")
	if len(combined) != 2 {
		t.Fatalf("组合应产出 conclusion + decision 共 2 条, got %+v", combined)
	}
	if combined[0].Category != "conclusion" || combined[1].Category != "decision" {
		t.Fatalf("类别顺序不对, got %+v", combined)
	}
}

// TestUsageFeedback 反馈闭环（改进二）：Recall 命中即回写 UsageCount；
// 权重随使用次数提升（同分时被用过的条目排前）。
func TestUsageFeedback(t *testing.T) {
	store, _ := NewMemoryStore(t.TempDir())
	a := agent.MemoryItem{Content: "结论：Go 1.27.1", Category: "conclusion", Source: "s1"}
	b := agent.MemoryItem{Content: "偏好：优先用 MockLLM 跑 demo", Category: "preference", Source: "s1"}
	if err := store.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(b); err != nil {
		t.Fatal(err)
	}
	// 两次召回 "Go"：结论条命中并回写
	if _, err := store.Recall("Go", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recall("Go", 3); err != nil {
		t.Fatal(err)
	}
	got := store.List()
	var aEntry, bEntry *MemoryEntry
	for i := range got {
		if got[i].Content == a.Content {
			aEntry = &got[i]
		}
		if got[i].Content == b.Content {
			bEntry = &got[i]
		}
	}
	if aEntry == nil || bEntry == nil {
		t.Fatal("两条记忆都应存在")
	}
	if aEntry.UsageCount != 2 || aEntry.LastRecalled.IsZero() {
		t.Fatalf("命中条应回写 usage=2, got %+v", aEntry)
	}
	if bEntry.UsageCount != 0 || !bEntry.LastRecalled.IsZero() {
		t.Fatalf("未命中条不应回写, got %+v", bEntry)
	}
	// 持久化往返：反馈字段跨实例存活（新实例加载同一目录）
	store2, _ := NewMemoryStore(store.dir)
	reloaded := store2.List()
	if len(reloaded) != 2 {
		t.Fatalf("应加载 2 条记忆, got %d", len(reloaded))
	}
	for _, e := range reloaded {
		if e.Content == a.Content && e.UsageCount != 2 {
			t.Fatalf("结论条反馈字段应跨实例持久化 usage=2, got %+v", e)
		}
		if e.Content == b.Content && e.UsageCount != 0 {
			t.Fatalf("未命中条不应有使用计数, got %+v", e)
		}
	}
}

// TestPrune 过期清理（改进二）：创建超过阈值且使用次数低于门槛的条目被删。
func TestPrune(t *testing.T) {
	store, _ := NewMemoryStore(t.TempDir())
	if err := store.Add(agent.MemoryItem{Content: "旧噪音", Category: "detail", Source: "s1"}); err != nil {
		t.Fatal(err)
	}
	// 回拨 CreatedAt 模拟过期
	store.mu.Lock()
	store.entries[0].CreatedAt = time.Now().Add(-48 * time.Hour)
	store.mu.Unlock()
	if err := store.persist(); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(agent.MemoryItem{Content: "新结论", Category: "conclusion", Source: "s1"}); err != nil {
		t.Fatal(err)
	}
	// 新结论被召回一次（usage=1）
	if _, err := store.Recall("新结论", 3); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Prune(1, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("应删除 1 条过期低使用记忆, got %d", removed)
	}
	if got := store.List(); len(got) != 1 || got[0].Content != "新结论" {
		t.Fatalf("新结论应保留, got %+v", got)
	}
}
