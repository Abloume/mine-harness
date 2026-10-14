package agent

import (
	"fmt"
	"regexp"
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
	r := CompactContextReport(msgs, budget, s)
	return r.Messages, r.Compacted
}

// CompactReport 是压缩过程的可观测信息（增量 F/G：保真度与保护层状态）。
// 供宿主写轨迹（TraceCompact Detail）与评测——压缩不是黑盒，"压了什么、
// 丢了什么、保护了什么"都要可回答。
type CompactReport struct {
	Messages         []Message // 压缩后的消息
	Compacted        bool      // 本次是否发生了压缩
	SkippedProtected int       // 增量 G：受保护消息数（不参与压缩）
	FidelityDropped  int       // 增量 F：保真度抽查介入的批次次数（全丢→放弃压缩保留原文；部分丢→补注追回）
	FIFODropped      int       // 摘要失败/不收敛兜底 FIFO 丢弃的批次次数
	KeysFound        int       // 增量 F：被压批次抽到的关键实体数
	KeysLost         int       // 增量 F：抽查发现摘要丢失的关键实体数（全丢放弃 / 部分丢补注追回）
}

// CompactContextReport 是 CompactContext 的可观测版本（调用方要写轨迹时用它，
// 不要 JSON 序列化整个历史）。
//
// 增量 G：选批只发生在非 Protected 消息上（硬约束/系统提示不参与压缩，
// 首条 msgs[0] 无条件保留）；增量 F：摘要后做保真度抽查——被压批次的关键
// 实体（数字/URL/引用专名）全部丢失则判定摘要不可信、保留原文放弃压缩；
// 部分丢失则补注进摘要（不丢事实），宁可少压也不压错。
func CompactContextReport(msgs []Message, budget int, s Summarizer) CompactReport {
	var rep CompactReport
	if budget <= 0 {
		rep.Messages = msgs
		return rep
	}
	compacted := false
	for MessagesTokens(msgs) > budget && len(msgs) > 2 {
		// 可压缩消息：跳过 Protected（硬约束/受保护系统提示），
		// 且从索引 1 起（首条 msgs[0] 恒保留——系统提示不参与压缩）
		var cand []int
		for i := 1; i < len(msgs); i++ {
			if !msgs[i].Protected {
				cand = append(cand, i)
			}
		}
		// 受保护消息数（含首条若标记）：本次压缩不参与选批的硬约束/系统提示
		prot := 0
		for _, m := range msgs {
			if m.Protected {
				prot++
			}
		}
		rep.SkippedProtected = prot
		// 只剩 ≤1 条非保护消息：再压会丢掉最后一条上下文，停止
		if len(cand) < 2 {
			break
		}
		// 压缩其中最早的一半（至少 1 条）；索引保持升序
		k := max(1, len(cand)/2) // Go 1.21+ 内置 max
		oldIdx := cand[:k]

		var old []Message
		for _, i := range oldIdx {
			old = append(old, msgs[i])
		}
		before := MessagesTokens(old)
		summary, err := s.Summarize(old)
		if err != nil {
			// 摘要失败：兜底 FIFO，直接丢这一批
			msgs = dropIndices(msgs, oldIdx)
			rep.FIFODropped++
			compacted = true
			continue
		}

		// 增量 F · 保真度抽查：关键实体必须保留在摘要里，否则判定记忆断片。
		// 全部丢失 → 摘要不可信，放弃压缩保留原文；部分丢失 → 补注进摘要
		// （不丢事实）。压缩的可靠性优先于压缩率。
		// Heuristic（抽取式）截断可能丢尾部数字 → 补注兜住；生成式摘要若改写
		// 关键数字/专名 → 同样处理。
		keys, lostKeys := keyEntitiesLost(old, summary)
		rep.KeysFound += len(keys)
		rep.KeysLost += len(lostKeys)
		if len(lostKeys) > 0 {
			if len(lostKeys) == len(keys) {
				// 关键实体**全部**丢失：摘要不可信（生成式摘要改写/归纳过头），
				// 放弃本次压缩、保留原文——宁可略超预算也不留"看似完整实则
				// 丢光硬信息"的误导性摘要
				rep.FidelityDropped++
				break
			}
			// 部分丢失：把缺失实体补注进摘要（不丢事实，摘要仍比原文小）。
			// 前缀刻意精简（【补注】4 字）——补注是兜底手段，开销必须远小于
			// 被追回实体的价值，否则补注膨胀会让摘要不收敛
			summary += "\n【补注】" + strings.Join(lostKeys, "、")
			rep.FidelityDropped++
		}

		// 用一条"摘要消息"替换这批旧消息（角色标记为 system，前缀注明是摘要）。
		// 摘要消息不设 Protected：历史摘要允许被再压缩/合并（旧摘要并入新摘要）。
		sumMsg := Message{Role: roleSystem, Content: "【历史摘要】\n" + summary}
		if MessagesTokens([]Message{sumMsg}) >= before {
			// 摘要（含补注）不比原文小：插入不收敛，回退 FIFO 直丢
			// （压缩率兜底，保证循环必然收敛；被丢批次的关键实体已在
			// rep.KeysLost/KeysFound 中记录，宿主可写进轨迹可追溯）。
			// 注：增量 F 之后"全丢"已在上方 break 保留原文，这里只剩
			// "摘要可接受但不收敛"的场景，直丢不违反保真意图。
			msgs = dropIndices(msgs, oldIdx)
			rep.FIFODropped++
			compacted = true
			continue
		}
		msgs = replaceIndices(msgs, oldIdx, sumMsg)
		compacted = true
	}
	rep.Messages = msgs
	rep.Compacted = compacted
	return rep
}

// keyEntitiesLost 抽取被压批次的关键实体并核对摘要保真度（增量 F）。
// 返回：抽取到的实体列表、丢失的实体列表。规则式、零新依赖——只兜"硬信息"
// （数字/URL/引用专名），不追求全量实体识别；摘要保留全部实体才算通过。
func keyEntitiesLost(old []Message, summary string) ([]string, []string) {
	var sb strings.Builder
	for _, m := range old {
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	keys := extractKeyEntities(sb.String())
	if len(keys) == 0 {
		return nil, nil // 无可抽查实体：不干预
	}
	var lost []string
	for _, k := range keys {
		if !strings.Contains(summary, k) {
			lost = append(lost, k)
		}
	}
	return keys, lost
}

// extractKeyEntities 关键实体抽取（增量 F）：≥2 位数字（含小数/百分比）、URL、
// 双引号/中文引号包裹的专有名词。regexp 包级编译一次、复用（并发安全）。
// 只抽 ≥2 位数字：1 位数字几乎都是序号/计数（"第 1 步"、"调用 1 次"）——
// 抽进来会让抽查把结构噪声当硬信息，造成补注膨胀。
var (
	reNumber = regexp.MustCompile(`\d{2,}(?:[.,]\d+)?%?`)
	reURL    = regexp.MustCompile(`https?://[^\s"'《》]+`)
	reQuoted = regexp.MustCompile(`["“]([^"”]{1,24})["”]`)
)

func extractKeyEntities(text string) []string {
	seen := map[string]bool{}
	var out []string
	// 数字：独立成串（避免把时间戳/ID 的每一位拆开——先整串匹配再整体入列）
	for _, m := range reNumber.FindAllString(text, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, m := range reURL.FindAllString(text, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	// 引号包裹的专名（如 "订单号 50231" 里的整体）
	for _, m := range reQuoted.FindAllString(text, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// dropIndices 删除指定索引（升序）的消息，返回新切片（不修改原切片）。
func dropIndices(msgs []Message, idx []int) []Message {
	drop := make(map[int]bool, len(idx))
	for _, i := range idx {
		drop[i] = true
	}
	out := make([]Message, 0, len(msgs)-len(idx))
	for i, m := range msgs {
		if !drop[i] {
			out = append(out, m)
		}
	}
	return out
}

// replaceIndices 把指定索引（升序）的消息整体替换为一条消息：
// 摘要插在第一批被压消息的位置，其余顺序保持。
func replaceIndices(msgs []Message, idx []int, sub Message) []Message {
	drop := make(map[int]bool, len(idx))
	for _, i := range idx {
		drop[i] = true
	}
	out := make([]Message, 0, len(msgs)-len(idx)+1)
	inserted := false
	for i, m := range msgs {
		if i == idx[0] && !inserted {
			out = append(out, sub)
			inserted = true
		}
		if !drop[i] {
			out = append(out, m)
		}
	}
	return out
}

// TrimContext FIFO 兜底：直接丢弃最旧的非保护消息，直到满足预算。
// 仅在摘要压缩仍无法收敛时使用（保底，避免无限膨胀）。
// 增量 G：跳过 Protected 消息（硬约束/系统提示不在兜底丢弃范围内）。
func TrimContext(msgs []Message, budget int) []Message {
	if budget <= 0 {
		return msgs
	}
	for MessagesTokens(msgs) > budget && len(msgs) > 1 {
		dropped := false
		for i := range msgs {
			if msgs[i].Protected {
				continue
			}
			msgs = append(msgs[:i], msgs[i+1:]...)
			dropped = true
			break
		}
		if !dropped {
			break // 全部受保护：不能再丢，停止
		}
	}
	return msgs
}
