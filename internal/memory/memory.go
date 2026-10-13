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
// UsageCount / LastRecalled 是反馈闭环字段（改进二）：被召回即回写——
// "使用即强化"（lazy 验证），长期低使用条目可被 Prune 过期清理。
type MemoryEntry struct {
	ID           string    `json:"id"`
	Content      string    `json:"content"`
	Category     string    `json:"category"`
	Source       string    `json:"source"`
	CreatedAt    time.Time `json:"created_at"`
	UsageCount   int       `json:"usage_count"`
	LastRecalled time.Time `json:"last_recalled_at,omitempty"`
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
// 反馈闭环：**命中关键词**的条目才回写 UsageCount+1 / LastRecalled
// （"被召回"=因为相关才被召回；limit 凑数返回的无关条目不算被用上，
// 避免错误强化噪音）——精确版需模型侧确认引用，见 reflections 改造方案 3。
func (s *MemoryStore) Recall(query string, limit int) ([]agent.MemoryItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	terms := strings.Fields(strings.ToLower(query))
	type scored struct {
		idx   int
		e     MemoryEntry
		score float64
		hit   int
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
		// 反馈强化（改进二）：被召回次数越多权重越高（使用即验证）
		score += float64(e.UsageCount) * 0.1
		// 新鲜度：越新排越前（索引越小越新，用倒序加分）
		score += float64(len(s.entries)-i) * 0.01
		scoreds = append(scoreds, scored{idx: i, e: e, score: score, hit: hit})
	}
	sort.SliceStable(scoreds, func(a, b int) bool {
		return scoreds[a].score > scoreds[b].score
	})
	out := make([]agent.MemoryItem, 0, limit)
	now := time.Now()
	updated := false
	for _, sc := range scoreds {
		if len(out) >= limit {
			break
		}
		out = append(out, agent.MemoryItem{
			Content:  sc.e.Content,
			Category: sc.e.Category,
			Source:   sc.e.Source,
		})
		// 命中即强化：仅关键词命中的条目回写（凑数返回不回写）
		if sc.hit > 0 {
			s.entries[sc.idx].UsageCount++
			s.entries[sc.idx].LastRecalled = now
			updated = true
		}
	}
	if updated {
		_ = s.persist()
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

// Prune 过期清理（反馈闭环 · 长期未使用淘汰）：
// 删除「创建超过 olderThan 且被召回次数 < minUsage」的条目——从未被用上的
// 记忆视为噪音，可被回收（记忆库只增不减的问题由此闭环）。
func (s *MemoryStore) Prune(minUsage int, olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-olderThan)
	kept := s.entries[:0]
	removed := 0
	for _, e := range s.entries {
		if e.CreatedAt.Before(cutoff) && e.UsageCount < minUsage {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	if removed > 0 {
		s.entries = kept
		return removed, s.persist()
	}
	return 0, nil
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

// EventExtractor 是事件信号提取器（改进一 · 反思改造方案 1）：
// 从步骤账本中提取"改变了世界"的动作——非只读且执行成功的工具调用
// （写/审批/外部调用）→ decision 类记忆。
// 依据：账本事件比最终回答可靠——回答可能含糊，动作已发生（OK=true 由
// 内核记账）；类别 decision 使 Recall 权重高于普通 detail。
type EventExtractor struct {
	maxInput int // Input 摘要最大长度（默认 80）
}

// NewEventExtractor 构造事件信号提取器。
func NewEventExtractor() *EventExtractor {
	return &EventExtractor{maxInput: 80}
}

// Extract 提取决策/变更记忆（实现 agent.MemoryExtractor 的组成部分）。
// 每条非只读成功动作产出一条 decision：Content="已执行 <工具>(<入参摘要>)"，
// Source 定位到账本步骤号（可追溯回原始执行）。
func (e *EventExtractor) Extract(msgs []agent.Message, ledger []agent.StepRecord, sessionID string) []agent.MemoryItem {
	var items []agent.MemoryItem
	for _, rec := range ledger {
		if !rec.OK || rec.SideEffect == agent.SideReadonly {
			continue // 只读/失败的动作不改变世界
		}
		in := rec.Input
		if len(in) > e.maxInput {
			in = in[:e.maxInput] + "…"
		}
		items = append(items, agent.MemoryItem{
			Content:  "已执行 " + rec.Name + "(" + in + ")",
			Category: "decision",
			Source:   fmt.Sprintf("session:%s/step%d", sessionID, rec.Step),
		})
	}
	return items
}

// DefaultExtractor 是默认组合提取器：Heuristic（回答 → conclusion）
// + Event（账本 → decision）——两类信号都沉淀，覆盖"结论"与"决策"
// 两种最高价值记忆（类别权重都是 2 分）。
type DefaultExtractor struct {
	heuristic *HeuristicExtractor
	events    *EventExtractor
}

// NewDefaultExtractor 构造默认组合提取器（推荐用法）。
func NewDefaultExtractor() *DefaultExtractor {
	return &DefaultExtractor{heuristic: NewHeuristicExtractor(), events: NewEventExtractor()}
}

// Extract 组合两条通道（实现 agent.MemoryExtractor）。
func (d *DefaultExtractor) Extract(msgs []agent.Message, ledger []agent.StepRecord, sessionID string) []agent.MemoryItem {
	out := d.heuristic.Extract(msgs, ledger, sessionID)
	out = append(out, d.events.Extract(msgs, ledger, sessionID)...)
	return out
}

// HeuristicExtractor 是回答类记忆提取器（DefaultExtractor 的组成部分，
// 也可单独使用）：从会话历史中提取最后一条"最终回答"（assistant 非工具
// 调用消息）作为 conclusion 记忆——启发式零成本（不调 LLM），可被 LLM 版
// 替换（同 Summarizer 的替换模式）。ledger 参数为接口对齐，启发式不使用
// （回答类记忆与动作无关）。
type HeuristicExtractor struct {
	maxLen int // 记忆内容最大长度（默认 200）
}

// NewHeuristicExtractor 构造回答类提取器。
func NewHeuristicExtractor() *HeuristicExtractor {
	return &HeuristicExtractor{maxLen: 200}
}

// Extract 提取关键记忆（实现 agent.MemoryExtractor 的组成部分）。
// 提取策略（启发式）：
//   - 最后一条 assistant 非工具消息 = 本次会话的结论 → conclusion 条目；
//   - 找不到结论（如被中止）→ 返回空（不沉淀垃圾记忆）。
func (e *HeuristicExtractor) Extract(msgs []agent.Message, _ []agent.StepRecord, sessionID string) []agent.MemoryItem {
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
