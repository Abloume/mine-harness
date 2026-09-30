package agent

import (
	"fmt"
	"log"
)

// 常量消息角色（与 llm.go 的 Message.Role 配合，避免魔法字符串散落）。
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// Agent 是最小运行时内核：把 LLM + 工具 + 上下文预算 + 步数限制
// 组装成一个 ReAct 风格主循环。
//
// JS/TS ↔ Go 差异：TS 里通常是一个 class + 私有字段；Go 用 struct +
// 指针接收者方法，等价物是 "method on struct"。字段首字母大小写决定
// 是否导出（大写导出，小写包内私有）。
type Agent struct {
	llm         LLM
	registry    *Registry
	maxSteps    int
	tokenBudget int
	verbose     bool // 打印每步，对应 harness 的 trace / 可观测概念

	history []Message
}

// Option 是函数式选项模式（functional options），Go 里扩展构造函数参数的常用手法。
//
// JS/TS ↔ Go 差异：TS 常用 options object 参数 { maxSteps?: number }；
// Go 用"返回闭包"的 Option 函数，逐个覆盖字段，调用方按需组合。
type Option func(*Agent)

// WithMaxSteps 设置最大循环步数（防死循环的护栏）。
func WithMaxSteps(n int) Option { return func(a *Agent) { a.maxSteps = n } }

// WithTokenBudget 设置上下文 token 预算（超出触发截断）。
func WithTokenBudget(n int) Option { return func(a *Agent) { a.tokenBudget = n } }

// WithVerbose 开启每步 trace 打印。
func WithVerbose(on bool) Option { return func(a *Agent) { a.verbose = on } }

// NewAgent 构造一个 Agent，默认 maxSteps=10、budget=4096、verbose=false。
func NewAgent(llm LLM, registry *Registry, opts ...Option) *Agent {
	a := &Agent{
		llm:         llm,
		registry:    registry,
		maxSteps:    10,
		tokenBudget: 4096,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Run 执行任务并返回最终回答。
//
// 这就是"agent loop"的核心，对应真实 harness 的一轮循环：
//
//	调模型 → 解析工具调用 → 执行工具 → 回填结果 → 再调模型（直到结束）
func (a *Agent) Run(task string) (string, error) {
	a.history = append(a.history,
		Message{Role: roleSystem, Content: "You are a helpful agent. 可以调用工具完成任务，拿到结果后给出最终回答。"},
		Message{Role: roleUser, Content: task},
	)

	for step := 1; step <= a.maxSteps; step++ {
		// 0. 上下文预算检查：超预算先截断最旧消息（context.go 的 FIFO 截断）
		a.history = TrimContext(a.history, a.tokenBudget)

		// 1. 调模型（把工具清单一起给它，对应 function calling 的 tools 参数）
		resp, err := a.llm.Chat(a.history, a.registry.List())
		if err != nil {
			return "", fmt.Errorf("step %d: 模型调用失败: %w", step, err)
		}

		// 2a. 模型请求调用工具：执行并把结果作为 tool 消息回填，继续下一轮
		if resp.ToolCall != nil {
			tc := resp.ToolCall
			if a.verbose {
				log.Printf("[step %d] tool_call → %s(%s)", step, tc.Name, tc.Input)
			}

			// 工具执行错误不直接崩掉 loop，而是把错误回填给模型，
			// 让它自己决定是换参数重试还是放弃——这就是 agent 的容错循环。
			out, err := a.registry.Call(tc.Name, tc.Input)
			if err != nil {
				out = fmt.Sprintf("工具调用失败: %v", err)
			}
			if a.verbose {
				log.Printf("[step %d] tool_result ← %s", step, out)
			}

			a.history = append(a.history,
				Message{Role: roleAssistant, Content: fmt.Sprintf("调用工具 %s", tc.Name)},
				Message{Role: roleTool, Content: out},
			)
			continue
		}

		// 2b. 模型给出最终文本答案：追加进历史并返回
		if a.verbose {
			log.Printf("[step %d] final answer", step)
		}
		a.history = append(a.history, Message{Role: roleAssistant, Content: resp.Content})
		return resp.Content, nil
	}

	// 步数耗尽仍未收敛：对应生产 harness 里的超时 / 循环检测护栏。
	return "", fmt.Errorf("达到最大步数 %d 仍未收敛，任务中止", a.maxSteps)
}
