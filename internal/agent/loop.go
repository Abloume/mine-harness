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

// RunStatus 是任务结束状态。
//
// JS/TS ↔ Go 差异：TS 常用字符串字面量联合类型 "completed" | "aborted"；
// Go 用 string 常量 + 注释约束，编译期没有穷尽检查。
const (
	StatusCompleted = "completed"
	StatusAborted   = "aborted"
)

// RunResult 是一次 Run 的完整结果。
// 引入它是为了"软停止"：撞上限/循环命中不再是 error（不是异常），
// 而是带状态的正常返回，调用方可以展示进度并交还用户继续指挥。
type RunResult struct {
	Answer string // 最终回答（仅在 completed 时有意义）
	Steps  int
	Status string // StatusCompleted / StatusAborted
	Reason string // 中止原因（循环检测 / 步数超限 / 重试超预算）
}

// ToolTrace 记录一次工具调用，供循环检测比对。
//
// JS/TS ↔ Go 差异：Go 的 struct 只要所有字段可比较（string 等），就可以直接
// 用 == 做整体比较（a == b）；TS/JS 的对象没有这个语义，得逐字段或序列化比较。
type ToolTrace struct {
	Name  string
	Input string
}

// guardLevel 是循环检测的判定结果。
type guardLevel int

const (
	guardOK    guardLevel = iota // 未命中，放行
	guardWarn                    // 第 1 次命中：注入警告，要求模型换策略
	guardAbort                   // 警告后仍重复：中止
)

// LoopGuard 是循环护栏：精确重复检测 + 失败重试预算 + 简单序列循环检测。
//
// 设计要点（对应生产 harness 的收敛检测）：
//   - 滑动窗口：只在最近 windowSize 条调用内计数，长任务的旧调用不误触发；
//   - 精确重复：同一工具 + 相同参数，命中 repeatLimit 次先警告、再犯中止；
//   - 失败重试：工具报错后的连续同参重试走独立预算 retryLimit，不混入循环检测；
//   - 分级响应：warned 置位后，模型仍重复同一调用才升级为中止，不一刀切。
type LoopGuard struct {
	windowSize  int
	repeatLimit int
	retryLimit  int

	trace      []ToolTrace // 滑动窗口内容
	retryCount int         // 连续同参失败的次数
	warned     bool        // 是否已对当前重复序列发过警告
}

// NewLoopGuard 用默认值构造护栏：窗口 8 步、精确重复阈值 2、重试预算 3。
// 这些初始值是起点，生产应按"轨迹收集 → 阈值扫描"闭环用数据调。
func NewLoopGuard() *LoopGuard {
	return &LoopGuard{windowSize: 8, repeatLimit: 2, retryLimit: 3}
}

// guard 在每次工具调用前调用：判断该调用是否命中循环。
//
// 调用方拿到 guardWarn 时注入一条 system 警告并 continue；
// 拿到 guardAbort 时终止任务（软停止，不是 panic）。
func (g *LoopGuard) guard(name, input string) guardLevel {
	t := ToolTrace{Name: name, Input: input}

	// 1. 精确重复检测：统计窗口内相同调用的出现次数（含本次）
	count := 0
	for _, tr := range g.trace {
		if tr == t {
			count++
		}
	}
	// 2. 简单序列循环检测：窗口尾部出现 [A,B,A,B] 二元周期
	cycle := false
	if n := len(g.trace); n >= 4 {
		a, b := g.trace[n-2], g.trace[n-1]
		cycle = g.trace[n-4] == a && g.trace[n-3] == b
	}

	// 3. 分级响应
	if count >= g.repeatLimit || cycle {
		if !g.warned {
			g.warned = true // 第一次命中：只警告，给模型改正机会
			return guardWarn
		}
		return guardAbort // 警告过还重复：升级为中止
	}

	// 4. 未命中：入窗口，重置警告状态（换了正常调用，之前警告作废）
	g.trace = append(g.trace, t)
	if len(g.trace) > g.windowSize {
		g.trace = g.trace[len(g.trace)-g.windowSize:] // 滑动窗口裁剪
	}
	g.warned = false
	return guardOK
}

// retryHit 在工具执行失败后调用：若与上次是同一工具同一参数，则计入重试预算。
// 返回 true 表示超过预算，应中止。
func (g *LoopGuard) retryHit(name, input string) bool {
	if n := len(g.trace); n > 0 {
		last := g.trace[n-1]
		if last.Name == name && last.Input == input {
			g.retryCount++
			if g.retryCount > g.retryLimit {
				return true
			}
			return false
		}
	}
	// 换了调用目标，重试计数归零
	g.retryCount = 1
	return false
}

// Agent 是最小运行时内核：把 LLM + 工具 + 上下文预算 + 步数限制 + 循环护栏
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

	guard *LoopGuard

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

// WithLoopGuard 替换默认循环护栏（自定义阈值用）。
func WithLoopGuard(g *LoopGuard) Option { return func(a *Agent) { a.guard = g } }

// NewAgent 构造一个 Agent，默认 maxSteps=10、budget=4096、verbose=false。
func NewAgent(llm LLM, registry *Registry, opts ...Option) *Agent {
	a := &Agent{
		llm:         llm,
		registry:    registry,
		maxSteps:    10,
		tokenBudget: 4096,
		guard:       NewLoopGuard(),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Run 执行任务并返回结果（可能完成，也可能被护栏软停止）。
//
// 这就是"agent loop"的核心，对应真实 harness 的一轮循环：
//
//	调模型 → 解析工具调用 → 循环检测 → 执行工具 → 回填结果 → 再调模型（直到结束）
func (a *Agent) Run(task string) RunResult {
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
			return RunResult{Steps: step, Status: StatusAborted, Reason: fmt.Sprintf("模型调用失败: %v", err)}
		}

		// 2a. 模型请求调用工具
		if resp.ToolCall != nil {
			tc := resp.ToolCall

			// 2a-1. 循环检测：命中先警告、再犯中止
			switch a.guard.guard(tc.Name, tc.Input) {
			case guardWarn:
				if a.verbose {
					log.Printf("[step %d] ⚠ 循环检测命中（重复调用 %s），注入警告", step, tc.Name)
				}
				a.history = append(a.history, Message{
					Role:    roleSystem,
					Content: fmt.Sprintf("检测到你重复调用工具 %s(%s) 且无进展。请换一种方式：改用其他工具、修改参数或直接给出最终回答。", tc.Name, tc.Input),
				})
				continue
			case guardAbort:
				if a.verbose {
					log.Printf("[step %d] ✗ 循环检测二次命中，中止", step)
				}
				return RunResult{Steps: step, Status: StatusAborted,
					Reason: fmt.Sprintf("循环检测：重复调用 %s(%s) 且警告后无进展", tc.Name, tc.Input)}
			}

			if a.verbose {
				log.Printf("[step %d] tool_call → %s(%s)", step, tc.Name, tc.Input)
			}

			// 2a-2. 执行工具；失败不崩掉 loop，回填错误让模型自己决定，
			// 但连续同参失败受重试预算约束（guard.retryHit）
			out, err := a.registry.Call(tc.Name, tc.Input)
			if err != nil {
				if a.guard.retryHit(tc.Name, tc.Input) {
					return RunResult{Steps: step, Status: StatusAborted,
						Reason: fmt.Sprintf("重试超预算：%s 连续失败超过 %d 次", tc.Name, a.guard.retryLimit)}
				}
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

		// 2b. 模型给出最终文本答案：追加进历史并返回（completed）
		if a.verbose {
			log.Printf("[step %d] final answer", step)
		}
		a.history = append(a.history, Message{Role: roleAssistant, Content: resp.Content})
		return RunResult{Answer: resp.Content, Steps: step, Status: StatusCompleted}
	}

	// 步数耗尽仍未收敛：软停止（不是 error）——返回进度与原因，交还用户继续指挥
	return RunResult{Steps: a.maxSteps, Status: StatusAborted, Reason: fmt.Sprintf("达到最大步数 %d，任务未完成", a.maxSteps)}
}
