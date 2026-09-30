package agent

// 本文件是"上下文管理"的最小实现，对应真实 harness 中
// token 预算控制与 context compaction 的极简雏形。

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

// TrimContext 超出预算时丢弃最旧的"非首条"消息，直到满足预算。
//
// 这是最粗暴的 FIFO 截断（丢最旧）；真实 harness（如微软 Agent Harness 的
// context compaction）会把旧内容压缩成摘要再放回，而不是裸丢——
// 裸丢会丢失跨轮依赖，第一版先跑通，后续再升级成摘要压缩。
func TrimContext(msgs []Message, budget int) []Message {
	if budget <= 0 {
		return msgs
	}
	for MessagesTokens(msgs) > budget && len(msgs) > 1 {
		// 始终保留第 0 条（约定为 system 指令），只丢它之后的旧消息
		msgs = msgs[1:]
	}
	return msgs
}
