package agent

import (
	"fmt"
	"strings"
)

// 本文件是"上下文管理"的实现：token 估算 + FIFO 兜底 + 摘要压缩（compaction）。
//
// 对齐真实 harness（如微软 Agent Framework 的 context compaction）：
// 超预算时不是裸丢旧消息，而是把旧消息压缩成一条摘要再放回，
// 保留跨轮依赖（比如工具结果的要点），同时显著降低 token 占用。

// EstimateTokens 粗略估算一段文本的 token 数。
// 近似规则：CJK 字符按 1 字 ≈ 1 token，其余按 4 字符 ≈ 1 token。
//
// ⚠ 当前实现存在的问题（学习版，生产不可直接使用）：
//  1. 只统计 Content 文本，未计入结构性开销：每条消息的角色标记、格式、
//     每轮重复注入的工具 schema、system 指令——真实 API 计费都包含这些，
//     因此这里会系统性低估真实消耗；
//  2. "4 字符 ≈ 1 token" 是通用近似。真实分词是 BPE 词表编码
//     （如 "Beijing" 可能是 1 个也可能是 2 个 token），误差随文本类型波动；
//  3. 估算偏差的方向有实际影响：低估 → 实际超窗被 API 拒绝；
//     高估 → 过早触发压缩导致信息丢失。
//
// ✅ 生产环境的合理做法：
//  1. 精确层：接入模型自带 tokenizer（如 OpenAI tiktoken / 模型官方
//     tokenizer），对全量消息精确编码，误差趋近于零；
//  2. 混合层：长文本（工具结果、文档）用精确 tokenizer，短消息用近似
//     估算，成本与精度折中；
//  3. 预算层：无论用哪种统计，都把 tokenBudget 设为上下文窗口的 85%
//     左右留安全余量，容纳结构性开销；
//  4. 校验层：压缩/截断后重新统计并断言不超预算，形成闭环（见 context_test.go）。
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
		// 工具调用参数也是上下文的一部分：assistant 的 tool_calls（结构化）
		// 以及 tool 消息的输入都占用窗口——多 tool_call 场景下占比不小。
		for _, tc := range m.ToolCalls {
			total += EstimateTokens(tc.Name) + EstimateTokens(tc.Input)
		}
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

// LLMSummarizer 是生成式摘要器：把一批旧消息交给 LLM 生成浓缩摘要。
//
// 与 HeuristicSummarizer（抽取式、零成本、确定性）相对：生成式摘要质量更高、
// 更紧凑（能跨消息归纳），代价是每次压缩多一次模型调用——生产里应给摘要器
// 配独立的小模型/专用摘要模型，避免占用主 agent 的模型预算（与
// LLMRiskEvaluator 的"专用小模型"思路一致）。
type LLMSummarizer struct {
	llm             LLM    // 摘要用的模型（可与主 agent 同实例或独立实例）
	maxSourceTokens int    // 喂给摘要模型的输入预算：超了先丢最旧（防摘要器自身撑爆窗口）
	prompt          string // 摘要指令模板
}

// NewLLMSummarizer 构造生成式摘要器。默认输入预算 1500 token（够覆盖
// 一批工具结果），prompt 要求**结构化摘要**（对齐 Claude Code 的 9 段式
// context compaction，精简为 6 段：目标 / 动作 / 已完成 / 失败 / 待办 / 约束）
// ——结构化比自由摘要信息密度高、后续模型更容易续接，是生产框架的标准做法。
func NewLLMSummarizer(llm LLM) *LLMSummarizer {
	return &LLMSummarizer{
		llm:             llm,
		maxSourceTokens: 1500,
		prompt: `你是对话历史压缩器。把下面对话压缩成结构化摘要，按以下章节组织（某节没有内容就写"无"）：
1. 任务目标：用户最初的要求（尽量保留关键信息）
2. 已执行动作：调用了哪些工具、关键参数与结果
3. 已完成：已确定的结论/数据
4. 失败与问题：失败过的操作、被拒绝的动作、报错
5. 未完成与待办：还差什么、下一步要做什么
6. 用户约束：明确说过的要求、禁止事项
用中文，总长度不超过 200 字，不得编造未出现的信息。

以下是要压缩的对话：
`,
	}
}

// Summarize 实现 Summarizer 接口。
//
// 递归风险：摘要器内部也要调 LLM，若把超长历史原样喂进去，摘要器自己就会
// 撑爆窗口——所以按 maxSourceTokens 从"最新消息"反向累积、预算封顶
// （预算内保留最新信息，与 TrimContext 的 FIFO 语义一致，且细化到单条级别：
// 单条本身超预算的消息宁可跳过也不截半截）。LLM 失败时返回 error，
// 由 CompactContext 兜底 FIFO（不崩 loop）。
func (s *LLMSummarizer) Summarize(msgs []Message) (string, error) {
	// 1. 从最新往前收集，控制喂给模型的输入总量
	//    （TrimContext 只能整条丢，单条超预算会漏网；这里逐条判定更细）
	var lines []string
	total := EstimateTokens(s.prompt)
	for i := len(msgs) - 1; i >= 0; i-- {
		line := msgs[i].Role + ": " + msgs[i].Content + "\n"
		if total+EstimateTokens(line) > s.maxSourceTokens {
			break // 再加就超预算：丢弃这条及更旧的（宁缺毋滥，防撑爆）
		}
		lines = append(lines, line)
		total += EstimateTokens(line)
	}

	// 2. 反序写回（保持时间顺序：prompt + 最早的可行消息 → 最新消息）
	var b strings.Builder
	b.WriteString(s.prompt)
	for i := len(lines) - 1; i >= 0; i-- {
		b.WriteString(lines[i])
	}

	// 3. 调 LLM 生成摘要（tools 传 nil：摘要不是工具调用场景）
	resp, err := s.llm.Chat([]Message{{Role: roleUser, Content: b.String()}}, nil)
	if err != nil {
		return "", fmt.Errorf("生成式摘要失败: %w", err)
	}
	if strings.TrimSpace(resp.Content) == "" {
		return "", fmt.Errorf("生成式摘要返回空内容")
	}
	return resp.Content, nil
}

// CompactContext 上下文压缩：超预算时，把最早的一批"非首条"消息压缩成
// 一条摘要消息放回历史；仍超预算则继续压缩下一批；最终兜底用 FIFO 丢弃。
// 返回值中 compacted 表示本轮是否发生过压缩（供调用方打 trace）。
//
// 约定：第 0 条是 system 指令，永不被压缩/丢弃。
//
// 收敛保证：摘要消息若不比它替换的旧批次更小（生成式摘要器输出长度
// 不可控，可能超预算），插入摘要反而会让循环永不收敛——此时回退为
// FIFO 直接丢弃该批（token 必然下降）。这是生成式摘要的配套护栏：
// 抽取式摘要（Heuristic）天然收敛，生成式必须显式防死循环。
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
		before := MessagesTokens(old)
		summary, err := s.Summarize(old)
		if err != nil {
			// 摘要失败：兜底 FIFO，直接丢这一批
			msgs = append(msgs[:1], msgs[1+k:]...)
			compacted = true
			continue
		}

		// 用一条"摘要消息"替换这批旧消息（角色标记为 system，前缀注明是摘要）
		sumMsg := Message{Role: roleSystem, Content: "【历史摘要】\n" + summary}
		if MessagesTokens([]Message{sumMsg}) >= before {
			// 摘要不比原文小：插入不收敛（如生成式摘要器输出超长），
			// 回退 FIFO 丢弃，保证循环必然收敛
			msgs = append(msgs[:1], msgs[1+k:]...)
			compacted = true
			continue
		}
		merged := append([]Message{msgs[0]}, sumMsg)
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
