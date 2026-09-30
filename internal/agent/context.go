package agent

import "strings"

// 本文件是"上下文管理"的实现：token 估算 + FIFO 兜底 + 摘要压缩（compaction）。
//
// 对齐真实 harness（如微软 Agent Framework 的 context compaction）：
// 超预算时不是裸丢旧消息，而是把旧消息压缩成一条摘要再放回，
// 保留跨轮依赖（比如工具结果的要点），同时显著降低 token 占用。

// EstimateTokens 粗略估算一段文本的 token 数。
// 近似规则：CJK 字符按 1 字 ≈ 1 token，其余按 4 字符 ≈ 1 token。
// 真实系统会用模型自带 tokenizer / tiktoken 等做精确计算。
func EstimateTokens(s string) int {
	cjk := 0
	rest := 0
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF { // CJK 统一表意文字区
			cjk++
		} else {
			rest++
		}
	}
	return cjk + rest/4
}

// MessagesTokens 累计整段对话的估算 token 数。
func MessagesTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(m.Content)
	}
	return total
}

// Summarizer 是摘要压缩的抽象：给定一批旧消息，产出浓缩文本。
//
// JS/TS ↔ Go 差异：TS 常用 interface + implements 组合；Go interface 靠
// 方法集隐式实现。这里的默认实现是"抽取式"（截断拼接），生产可换
// "生成式"（调 LLM 写摘要）而不改动调用方。
type Summarizer interface {
	Summarize(msgs []Message) (string, error)
}

// HeuristicSummarizer 是抽取式摘要：每条消息取前 maxLenPerMsg 个字符，
// 加角色前缀后拼接。成本为零、确定性强，适合学习演示。
type HeuristicSummarizer struct {
	MaxLenPerMsg int // 每条最多保留多少字符
}

// NewHeuristicSummarizer 构造默认摘要器（每条保留 80 字符）。
func NewHeuristicSummarizer() *HeuristicSummarizer {
	return &HeuristicSummarizer{MaxLenPerMsg: 80}
}

// Summarize 实现 Summarizer 接口。
//
// JS/TS ↔ Go 差异：Go 处理 UTF-8 时按 rune（字符）操作，[]rune(s) 转字符数组，
// 中文不会被字节截断成乱码；JS 的 s.slice 按 UTF-16 code unit 切，与中文兼容但
// 与 Go 的语义不完全一致。
func (s *HeuristicSummarizer) Summarize(msgs []Message) (string, error) {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(": ")
		runes := []rune(m.Content)
		if len(runes) > s.MaxLenPerMsg {
			b.WriteString(string(runes[:s.MaxLenPerMsg]))
			b.WriteString("…")
		} else {
			b.WriteString(m.Content)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// CompactContext 上下文压缩：超预算时，把最早的一批"非首条"消息压缩成
// 一条摘要消息放回历史；仍超预算则继续压缩下一批；最终兜底用 FIFO 丢弃。
// 返回值中 compacted 表示本轮是否发生过压缩（供调用方打 trace）。
//
// 约定：第 0 条是 system 指令，永不被压缩/丢弃。
//
// JS/TS ↔ Go 差异：切片 msgs[1:1+k] 是取子切片（引用原数组），
// 对应 JS 的 slice(1, 1+k)。Go 切片是视图，拼接新数组时用 append 组合。
func CompactContext(msgs []Message, budget int, s Summarizer) ([]Message, bool) {
	if budget <= 0 {
		return msgs, false
	}
	compacted := false
	for MessagesTokens(msgs) > budget && len(msgs) > 2 {
		// 除 system 外的消息数；压缩其中最早一半（至少 1 条）
		n := len(msgs) - 1
		k := max(1, n/2) // Go 1.21+ 内置 max

		old := msgs[1 : 1+k]
		summary, err := s.Summarize(old)
		if err != nil {
			// 摘要失败：兜底 FIFO，直接丢这一批
			msgs = append(msgs[:1], msgs[1+k:]...)
			compacted = true
			continue
		}

		// 用一条"摘要消息"替换这批旧消息（角色标记为 system，前缀注明是摘要）
		merged := append([]Message{msgs[0]},
			Message{Role: roleSystem, Content: "【历史摘要】\n" + summary})
		merged = append(merged, msgs[1+k:]...)
		msgs = merged
		compacted = true
	}
	return msgs, compacted
}

// TrimContext FIFO 兜底：直接丢弃最旧的非首条消息，直到满足预算。
// 仅在摘要压缩仍无法收敛时使用（保底，避免无限膨胀）。
func TrimContext(msgs []Message, budget int) []Message {
	if budget <= 0 {
		return msgs
	}
	for MessagesTokens(msgs) > budget && len(msgs) > 1 {
		msgs = msgs[1:]
	}
	return msgs
}
