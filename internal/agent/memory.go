package agent

import (
	"fmt"
	"log"
	"strings"
)

// 第 5 步③ · 记忆分层 + 关键信息优先召回。
//
// 记忆分层的三层结构（对比"全部历史一把梭"）：
//   - 短期（working memory）：本轮上下文——压缩 + LLM 摘要（既有
//     CompactContext / Summarizer），本步不新增；
//   - 中期（session）：单次会话完整历史落盘——阶段一 SessionStore
//     （WithInitialHistory 整体恢复）；
//   - 长期（key memory）：跨会话的关键记忆——本步新增。不恢复全部历史，
//     只按任务召回相关条目（关键信息优先），token 成本远低于整体恢复。
//
// 依赖方向：agent 只定义窄接口（MemoryStore / MemoryExtractor），
// memory 包负责实现与持久化——Go 惯例"consumer 定义接口"，避免
// 循环依赖（memory 需要 import agent 的消息类型）。
// JS/TS ↔ Go 差异：TS 里这类解耦常靠依赖注入接口或抽象类；Go 用
// 最小接口 + 结构体满足即可（duck typing 的编译期检查）。

// MemoryItem 是一条长期记忆（关键信息，从会话历史提炼）。
// Source 记录来源（会话 ID + 消息定位），保证"记忆可追溯"——
// 召回后模型引用记忆时能回到原始会话。
type MemoryItem struct {
	Content  string // 关键信息一句话
	Category string // conclusion / decision / constraint / preference
	Source   string // 来源：会话 ID + 消息序号
}

// MemoryStore 是长期记忆存储的窄接口（agent 不依赖 memory 包实现）。
//
//	Add    沉淀一条关键记忆（实现方可去重、可持久化）；
//	Recall 按任务召回相关条目（实现方做"关键信息优先"排序，返回 limit 条）。
type MemoryStore interface {
	Add(item MemoryItem) error
	Recall(query string, limit int) ([]MemoryItem, error)
}

// MemoryExtractor 从会话历史提取关键记忆（短期 → 长期的沉淀器）。
// 与 Summarizer 同模式：默认启发式实现，可换成调 LLM 的版本。
type MemoryExtractor interface {
	Extract(msgs []Message, sessionID string) []MemoryItem
}

// recallLimit 默认召回上限（条数）：一次 Run 最多注入的记忆条目——
// 控制"关键信息优先"的 token 预算，避免记忆本身刷爆上下文。
const recallLimitDefault = 3

// recallMemory 在 Run 开始时按任务召回长期记忆，注入 system 上下文。
// 召回注入发生在历史组装之后、主循环之前；记忆以 system 角色进入
// （优先级高于普通对话），语义是"历史沉淀的关键信息，供参考"。
func (a *Agent) recallMemory(task string) {
	if a.memoryStore == nil || a.memoryExtractor == nil {
		return
	}
	items, err := a.memoryStore.Recall(task, a.memoryRecallLimit)
	if err != nil || len(items) == 0 {
		return
	}
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "· [%s] %s（来源: %s）\n", it.Category, it.Content, it.Source)
	}
	a.history = append(a.history, Message{
		Role:    roleSystem,
		Content: "【记忆召回】历史会话沉淀的关键信息（供参考，可避免重复探索；回答时可引用其来源）：\n" + b.String(),
	})
	if a.verbose {
		log.Printf("记忆召回：注入 %d 条关键记忆（limit=%d）", len(items), a.memoryRecallLimit)
	}
}

// settleMemory 在 Run 结束时把本次会话的关键信息沉淀进长期记忆。
// 只在 completed 时沉淀（被中止的会话没有可靠结论，不该污染长期记忆）；
// 提取器产出多条目时逐条写入，单条失败不阻塞其余。
func (a *Agent) settleMemory(status string) {
	if status != StatusCompleted || a.memoryStore == nil || a.memoryExtractor == nil {
		return
	}
	items := a.memoryExtractor.Extract(a.history, a.sessionID)
	for _, it := range items {
		if err := a.memoryStore.Add(it); err != nil && a.verbose {
			log.Printf("记忆沉淀失败: %v", err)
		}
	}
	if a.verbose && len(items) > 0 {
		log.Printf("记忆沉淀：本次会话产出 %d 条关键记忆", len(items))
	}
}
