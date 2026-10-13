package memory

import (
	"path/filepath"
	"testing"

	"mine-harness/internal/agent"
)

// TestSaveLoadRoundTrip Save → Load 往返一致（消息内容逐字段相等）。
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	msgs := []agent.Message{
		{Role: "system", Content: "You are a helpful agent."},
		{Role: "user", Content: "读取 go.mod"},
		{Role: "assistant", Content: "调用工具", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "read_file", Input: `{"path":"go.mod"}`}}},
		{Role: "tool", Content: "go 1.27.1", ToolCallID: "c1"},
		{Role: "assistant", Content: "结论：Go 1.27.1。"},
	}
	if err := store.Save("demo-session", msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Load("demo-session")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("消息数不一致: got %d want %d", len(got), len(msgs))
	}
	for i := range msgs {
		if got[i].Role != msgs[i].Role || got[i].Content != msgs[i].Content {
			t.Errorf("第 %d 条不一致: got %+v want %+v", i, got[i], msgs[i])
		}
		if len(got[i].ToolCalls) != len(msgs[i].ToolCalls) {
			t.Errorf("第 %d 条 ToolCalls 数不一致", i)
		}
	}
}

// TestLoadMissing 不存在的会话返回 ok=false（不是错误）。
func TestLoadMissing(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := store.Load("ghost")
	if err != nil || ok {
		t.Fatalf("Load(ghost): ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// TestListAndInvalidID List 返回排序后的会话 ID；非法 ID 被 Save/Load 拒绝。
func TestListAndInvalidID(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	if err := store.Save("b", []agent.Message{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("a", []agent.Message{}); err != nil {
		t.Fatal(err)
	}
	ids, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("List 应排序返回 [a b], got %v", ids)
	}

	bad := []string{"../etc/passwd", "a/b", "a b", ""}
	for _, id := range bad {
		if err := store.Save(id, []agent.Message{}); err == nil {
			t.Errorf("Save(%q) 应拒绝非法 ID", id)
		}
		if _, _, err := store.Load(id); err == nil {
			t.Errorf("Load(%q) 应拒绝非法 ID", id)
		}
	}
}

// TestAtomicOverwrite 重复 Save 覆盖旧文件：读回的是新内容（原子替换生效）。
func TestAtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	if err := store.Save("s", []agent.Message{{Role: "user", Content: "旧"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("s", []agent.Message{{Role: "user", Content: "新"}}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.Load("s")
	if len(got) != 1 || got[0].Content != "新" {
		t.Fatalf("覆盖后应读到新内容, got %+v", got)
	}
	// 目录里不应残留临时文件
	entries, _ := filepath.Glob(filepath.Join(dir, ".session-*"))
	if len(entries) != 0 {
		t.Fatalf("应无残留临时文件, got %v", entries)
	}
}
