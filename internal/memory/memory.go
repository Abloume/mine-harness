// 第 5 步③ · 长期记忆层：MemoryStore + 关键信息优先召回 + 启发式提取器。
//
// 与 session.go（阶段一·中期记忆）的分工：
//   - SessionStore：整段会话历史落盘/恢复（粒度=完整消息流）；
//   - MemoryStore：跨会话的关键记忆（粒度=条目），召回时只取相关条目。
//
// 依赖方向：本包实现 agent 定义的窄接口（MemoryStore / MemoryExtractor），
// 避免循环依赖（agent 不 import 本包，本包 import agent 的消息类型）。
package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mine-harness/internal/agent"
)

// MemoryEntry 是一条持久化的长期记忆（MemoryItem + 持久化元数据）。
// ID 与 CreatedAt 由存储侧生成；Content/Category/Source 来自提取器。
type MemoryEntry struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Category  string    `json:"category"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// MemoryStore 是长期记忆存储（实现 agent.MemoryStore 窄接口）。
// 持久化：单文件 memories.json（原子写，与 SessionStore 同款 temp+rename）。
// 并发安全：mutex 保护读-改-写（Add 先查重再落盘）。
//
// 关键信息优先召回（Recall）排序：
//   - 关键词命中：query 分词（空格分隔）与 Content 的交集数，优先；
//   - 类别权重：conclusion/decision 2 分（结论/决策是最高价值记忆），
//     constraint/preference 1.5 分，detail 1 分；
//   - 新鲜度：CreatedAt 降序。
//   总分 = 命中数*2 + 类别权重 + 新鲜度次序——"关键信息优先"的最小实现，
//   对应 PLAN 第 5 步③（召回按关键信息优先设计，不整体恢复历史）。
type MemoryStore struct {
	mu      sync.Mutex
	dir     string
	entries []MemoryEntry
}

// NewMemoryStore 构造长期记忆存储；目录不存在则创建，已有文件则加载。
func NewMemoryStore(dir string) (*MemoryStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("记忆目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建记忆目录失败: %w", err)
	}
	s := &MemoryStore{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *MemoryStore) file() string { return filepath.Join(s.dir, "memories.json") }

// load 读取既有记忆文件（不存在=空库，不是错误）。
func (s *MemoryStore) load() error {
	data, err := os.ReadFile(s.file())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, &s.entries)
}

// persist 原子写记忆文件（复用同包 atomicWrite：temp + rename）。
func (s *MemoryStore) persist() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.file(), data)
}

// Add 沉淀一条关键记忆（实现 agent.MemoryStore）。
// 去重：同 (Content, Category) 不重复写入——同一结论跨会话重复沉淀
// 只会保留一条（关键记忆的稳定性，防止记忆库膨胀）。
func (s *MemoryStore) Add(item agent.MemoryItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Content == item.Content && e.Category == item.Category {
			return nil // 已存在：幂等
		}
	}
	s.entries = append(s.entries, MemoryEntry{
		ID:        shortID(item.Source + "|" + item.Content),
		Content:   item.Content,
		Category:  item.Category,
		Source:    item.Source,
		CreatedAt: time.Now(),
	})
	return s.persist()
}

// Recall 按任务召回相关记忆（实现 agent.MemoryStore；关键信息优先排序，
// 返回最多 limit 条；limit<=0 返回空）。
func (s *MemoryStore) Recall(query string, limit int) ([]agent.MemoryItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	terms := strings.Fields(strings.ToLower(query))
	type scored struct {
		e     MemoryEntry
		score float64
	}
	scoreds := make([]scored, 0, len(s.entries))
	for i, e := range s.entries {
		hit := 0
		for _, t := range terms {
			if strings.Contains(strings.ToLower(e.Content), t) {
				hit++
			}
		}
		score := float64(hit) * 2
		score += categoryWeight(e.Category)
		// 新鲜度：越新排越前（索引越小越新，用倒序加分）
		score += float64(len(s.entries)-i) * 0.01
		scoreds = append(scoreds, scored{e: e, score: score})
	}
	sort.SliceStable(scoreds, func(a, b int) bool {
		return scoreds[a].score > scoreds[b].score
	})
	out := make([]agent.MemoryItem, 0, limit)
	for _, sc := range scoreds {
		if len(out) >= limit {
			break
		}
		out = append(out, agent.MemoryItem{
			Content:  sc.e.Content,
			Category: sc.e.Category,
			Source:   sc.e.Source,
		})
	}
	return out, nil
}

// List 返回全部记忆条目（按时间倒序，展示/调试用）。
func (s *MemoryStore) List() []MemoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MemoryEntry, len(s.entries))
	copy(out, s.entries)
	sort.SliceStable(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

// categoryWeight 记忆类别权重（关键信息优先的"关键"定义）。
func categoryWeight(c string) float64 {
	switch c {
	case "conclusion", "decision":
		return 2
	case "constraint", "preference":
		return 1.5
	default:
		return 1
	}
}

// shortID 生成记忆条目 ID（FNV 哈希 hex，同包 shortHash 思路；独立函数
// 避免与 tree 包耦合——此处仅需短且稳定）。
func shortID(s string) string {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}

// HeuristicExtractor 是默认记忆提取器（实现 agent.MemoryExtractor）：
// 从会话历史中提取最后一条"最终回答"（assistant 非工具调用消息）作为
// conclusion 记忆——启发式零成本（不调 LLM），可被 LLM 版替换
// （同 Summarizer 的替换模式）。
type HeuristicExtractor struct {
	maxLen int // 记忆内容最大长度（默认 200）
}

// NewHeuristicExtractor 构造默认提取器。
func NewHeuristicExtractor() *HeuristicExtractor {
	return &HeuristicExtractor{maxLen: 200}
}

// Extract 提取关键记忆（实现 agent.MemoryExtractor）。
// 提取策略（启发式）：
//   - 最后一条 assistant 非工具消息 = 本次会话的结论 → conclusion 条目；
//   - 找不到结论（如被中止）→ 返回空（不沉淀垃圾记忆）。
func (e *HeuristicExtractor) Extract(msgs []agent.Message, sessionID string) []agent.MemoryItem {
	content := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) != "" {
			content = strings.TrimSpace(m.Content)
			break
		}
	}
	if content == "" {
		return nil
	}
	if len(content) > e.maxLen {
		content = content[:e.maxLen-3] + "…"
	}
	return []agent.MemoryItem{{
		Content:  content,
		Category: "conclusion",
		Source:   fmt.Sprintf("session:%s/最后回答", sessionID),
	}}
}
