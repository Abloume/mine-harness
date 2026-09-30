// Package agent 实现一个最小 Agent 运行时内核（mini-harness）。
//
// 学习定位：这是"最小内核"练习，不是生产框架。生产请使用成熟运行时
// （Agent Framework / LangGraph / Agent SDK 等）。
// 注释中统一标注 JS/TS ↔ Go 差异点，便于前端转 Go 迁移学习。
package agent

import "fmt"

// Message 表示对话中的一条消息。
//
// ToolCallID：仅 tool 角色消息使用，关联它回答的那次工具调用。
// 真实 API（OpenAI 兼容）要求 tool 结果消息必须带 tool_call_id——
// 这是 mock → 真实协议的关键差异，Provider 负责把它填对。
//
// ToolCalls：仅 assistant 角色消息使用，携带结构化的工具调用记录（可为多个——
// 真实模型支持"一轮并行调用多个工具"，对应 OpenAI 协议的 tool_calls 数组）。
// mock 内核用自然语言文本（"调用工具 X"）记录即可；真实协议需要
// assistant 消息携带 tool_calls 结构（含 id/name/arguments），
// Provider 优先读这个字段做协议适配。
//
// JS/TS ↔ Go 差异：TS 常用 discriminated union 表达角色
// （type: 'user' | 'assistant' | 'tool'），Go 里用 string 常量 + 简单 struct，
// 类型安全靠使用处的约定，缺少 TS 的编译期穷尽检查。
type Message struct {
	Role       string // user / assistant / tool / system
	Content    string
	ToolCallID string     // tool 角色：关联的工具调用 id（真实 API 必需）
	ToolCalls  []ToolCall // assistant 角色：本次请求的工具调用（可为多个，并行调用）
}

// ToolCall 表示模型请求调用某个工具。
type ToolCall struct {
	ID    string // 模型响应中的工具调用 id（真实 API 用于关联结果；mock 可空）
	Name  string // 工具名
	Input string // 输入，JSON 字符串（真实系统里会用 JSON Schema 做结构化校验）
}

// LLMResponse 是模型一次返回的结果：要么给一段文本，要么请求调用一个或多个工具。
//
// JS/TS ↔ Go 差异：TS 常用联合类型 { text } | { toolCall: ToolCall[] }；
// Go 用 struct + slice 字段，len==0 表示"没有"，对应 TS 的空数组。
type LLMResponse struct {
	Content   string
	ToolCalls []ToolCall // 非空表示本轮要调用工具（可为多个）
}

// LLM 是模型接入层接口，mini-harness 只依赖这个抽象，不关心底层是哪个模型。
//
// JS/TS ↔ Go 差异：TS 接口是结构类型、可 extends；Go interface 靠方法集
// 隐式实现（鸭子类型），类型实现方法即自动满足接口，无需显式 implements。
type LLM interface {
	Chat(messages []Message, tools []Tool) (LLMResponse, error)
}

// MockDecision 描述 mock 模型"下一轮"的行为，用于精确控制演示走向。
// ToolCalls 非空则模拟"一轮并行调用多个工具"（真实模型的多 tool_calls 行为）。
type MockDecision struct {
	Content   string     // 最终回答（与 ToolName/ToolCalls 互斥，后两者优先）
	ToolName  string     // 本轮要调用的工具名（非空则优先走工具调用）
	ToolInput string     // 传给工具的 JSON 输入
	ToolCalls []ToolCall // 本轮并行调用多个工具（非空时优先于 ToolName）
}

// MockLLM 是一个可编程假模型：按脚本依次返回"先调用工具、再给答案"。
// 它让 loop 在没有真实 API Key 的情况下完整跑通，是学习内核时的标准做法。
type MockLLM struct {
	steps []MockDecision
	index int
}

// NewMockLLM 构造一个按 steps 顺序决策的假模型。
func NewMockLLM(steps []MockDecision) *MockLLM {
	return &MockLLM{steps: steps}
}

// Chat 实现 LLM 接口：按脚本顺序弹出一步。
//
// JS/TS ↔ Go 差异：JS 数组越界返回 undefined；Go 越界会 panic，
// 所以必须先判断 index >= len(steps)，把"脚本耗尽"转成显式 error。
func (m *MockLLM) Chat(messages []Message, tools []Tool) (LLMResponse, error) {
	if m.index >= len(m.steps) {
		return LLMResponse{}, fmt.Errorf("mock 脚本已耗尽（共 %d 步），loop 可能没有正常收敛", len(m.steps))
	}
	d := m.steps[m.index]
	m.index++
	if len(d.ToolCalls) > 0 {
		return LLMResponse{ToolCalls: d.ToolCalls}, nil
	}
	if d.ToolName != "" {
		return LLMResponse{ToolCalls: []ToolCall{{Name: d.ToolName, Input: d.ToolInput}}}, nil
	}
	return LLMResponse{Content: d.Content}, nil
}
