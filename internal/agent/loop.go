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
	summarizer  Summarizer
	approver    Approver // 审批决策器（nil = 未启用，fail-closed 见 Run）

	// 审批点分级配置：
	riskEvaluator     RiskEvaluator // 算本次调用最终风险（基础 + 参数修正）
	approvalThreshold RiskLevel     // 达到该级别才进审批闸门（低于则自动放行）
	denialLimit       int           // 连续拒绝上限，超过则升级中止
	denials           int           // 当前连续拒绝计数（一次通过/低风险放行即清零）

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

// WithSummarizer 替换默认摘要压缩器（如换成调 LLM 的生成式摘要）。
func WithSummarizer(s Summarizer) Option { return func(a *Agent) { a.summarizer = s } }

// WithApprover 设置审批决策器（human-in-the-loop）。
// 达到审批阈值的调用若无 approver 会被默认拒绝（fail-closed）。
func WithApprover(ap Approver) Option { return func(a *Agent) { a.approver = ap } }

// WithRiskEvaluator 替换默认风险评估器（默认静态：按工具声明级别，无参数修正）。
func WithRiskEvaluator(re RiskEvaluator) Option { return func(a *Agent) { a.riskEvaluator = re } }

// WithApprovalThreshold 设置审批阈值：达到该级别（含）的调用才进审批闸门。
func WithApprovalThreshold(lv RiskLevel) Option {
	return func(a *Agent) { a.approvalThreshold = lv }
}

// WithDenialLimit 设置连续拒绝升级上限（默认 3：对应 Claude Code 的 3 连拒升级）。
func WithDenialLimit(n int) Option { return func(a *Agent) { a.denialLimit = n } }

// NewAgent 构造一个 Agent，默认 maxSteps=10、budget=4096、verbose=false。
func NewAgent(llm LLM, registry *Registry, opts ...Option) *Agent {
	a := &Agent{
		llm:               llm,
		registry:          registry,
		maxSteps:          10,
		tokenBudget:       4096,
		guard:             NewLoopGuard(),
		summarizer:        NewHeuristicSummarizer(),
		riskEvaluator:     StaticRiskEvaluator{},
		approvalThreshold: RiskMedium, // 默认：Medium 及以上需要审批
		denialLimit:       3,
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
//	调模型 → 解析工具调用 → 循环检测 → 审批点 → 执行工具 → 回填结果 → 再调模型（直到结束）
//
// 审批点在循环检测之后、工具执行之前：先防"重复空转"，再防"越权动作"。
func (a *Agent) Run(task string) RunResult {
	a.history = append(a.history,
		Message{Role: roleSystem, Content: "You are a helpful agent. 可以调用工具完成任务，拿到结果后给出最终回答。"},
		Message{Role: roleUser, Content: task},
	)

	for step := 1; step <= a.maxSteps; step++ {
		// 0. 上下文预算检查：超预算先做摘要压缩（context.go 的 CompactContext），
		//    压缩不了才走 FIFO 兜底——对齐真实 harness 的 context compaction
		var compacted bool
		a.history, compacted = CompactContext(a.history, a.tokenBudget, a.summarizer)
		if compacted && a.verbose {
			log.Printf("[step %d] 上下文压缩：旧消息已压缩为摘要，当前 token=%d", step, MessagesTokens(a.history))
		}

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

			// 2a-1.5 审批点（human-in-the-loop）：按风险分级决定走不走闸门。
			//
			// 流程：算本次最终风险（基础级别 + 参数修正）→ 低于阈值自动放行 →
			// 达到阈值必须审批 → 拒绝计数，连续超限升级中止（软停止，不是 error）。
			// fail-closed：达到阈值但没配 approver（或拒绝）→ 不执行工具。
			risk := a.riskEvaluator.Evaluate(tc.Name, tc.Input, a.registry.BaseRiskOf(tc.Name))
			if risk >= a.approvalThreshold {
				approved := a.approver != nil && a.approver.Approve(tc.Name, tc.Input)
				if !approved {
					a.denials++
					if a.verbose {
						log.Printf("[step %d] ⛔ 审批拒绝（连续 %d 次）：%s(%s)", step, a.denials, tc.Name, tc.Input)
					}
					// 升级兜底：连续被拒说明模型在反复越权，停止并交还用户
					// （对应 Claude Code auto mode 的"3 次连续拒绝 → 升级给人"）。
					// 这是软停止不是 error，调用方可以展示进度并接手指挥。
					if a.denials >= a.denialLimit {
						return RunResult{Steps: step, Status: StatusAborted,
							Reason: fmt.Sprintf("审批升级：连续 %d 次高风险调用被拒绝，停止并交还人工", a.denials)}
					}
					// 拒绝结果以 tool 角色回填给模型（不是终止）：
					// 模型看到"未授权"可以换路径、改参数或直接回答。
					// 若模型反复请求同一动作，循环检测（上方）也会先警告再中止兜底。
					a.history = append(a.history, Message{
						Role:    roleTool,
						Content: fmt.Sprintf("调用被拒绝：%s(%s)。用户未授权此操作，请改用其他方式完成目标，或直接给出当前可完成的部分。", tc.Name, tc.Input),
					})
					continue
				}
				a.denials = 0 // 审批通过：模型收敛了，清空拒绝计数
				if a.verbose {
					log.Printf("[step %d] ✅ 审批通过（风险 %s）：%s(%s)", step, risk, tc.Name, tc.Input)
				}
			} else {
				a.denials = 0 // 低风险自动放行：模型已换路径，清空拒绝计数
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
