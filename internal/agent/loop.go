package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
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
// 缺口 D 新增取消三态（见 cancel.go）：canceled / partially_completed / needs_review。
const (
	StatusCompleted = "completed"
	StatusAborted   = "aborted"
)

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

// execItem 是阶段1（顺序护栏）产出的待执行项：安全判定在阶段1完成，
// 阶段2只负责执行与回填。
type execItem struct {
	tc    ToolCall
	name  string
	input string
	safe  bool // 普通工具且声明 ConcurrentSafe（可参与并行执行）
}

// execResult 是并行执行的单条结果（按索引写入，避免共享内存竞争）。
type execResult struct {
	out string
	err error
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

	// lastUsageTokens 是最近一次模型响应的服务端真实 total_tokens
	// （OpenAI 兼容协议 usage 字段；mock 无 usage 时为 0 → 回退本地估算触发压缩）
	lastUsageTokens int

	// 审批点分级配置：
	riskEvaluator     RiskEvaluator // 算本次调用最终风险（基础 + 参数修正）
	approvalThreshold RiskLevel     // 达到该级别才进审批闸门（低于则自动放行）
	denialLimit       int           // 连续拒绝上限，超过则升级中止
	denials           int           // 当前连续拒绝计数（一次通过/低风险放行即清零）

	guard *LoopGuard

	// B 方案真渐进式 Skill：load_skill 特殊通道 + system 注入。
	skillProvider SkillProvider   // 宿主提供的技能发现器（nil = 未启用 B 方案）
	loadedSkills  map[string]bool // 已注入过的技能名（去重：同一规范只进一次上下文）
	loadedRefs    map[string]bool // 已注入过的引用 key（"skill/ref" 去重）
	contextPath   string          // 当前任务的上下文路径（缺口 C：skill 路径作用域判定基准）

	// Skill History 短期工作记忆（缺口 B）：环形 buffer 记录最近工具调用，
	// 压缩时注入快照 system 消息，防「语义重复」的失败路径。
	skillHistory    *SkillHistory
	lastHistorySnap string // 上次注入的快照（相同则不重复注入，防刷屏）

	// initialHistory 是断点续跑（第 5 步阶段一）的初始上下文：
	// 由会话持久化恢复（internal/memory），Run 时只追加本次 user 消息，
	// 不再重建 system 首条——模型上下文延续上次 Run 的探索与结论。
	initialHistory []Message

	// 协作式取消契约（缺口 D）：
	// ctx 是外部取消信号源（context 取消 → cancel_requested）；ledger 是
	// 步骤账本（已执行工具调用的完整记录）；compensations 是取消收尾时
	// 的补偿执行记录。三者只在 RunWithContext 生命周期内有效。
	ctx              context.Context
	cancelRequested  bool
	ledger           []StepRecord
	compensations    []CompensateRecord

	// 第 5 步③ · 记忆分层：长期记忆层（跨会话关键记忆）。
	// memoryStore/memoryExtractor 为窄接口（见 memory.go），memory 包实现；
	// sessionID 是来源标注（记忆可追溯回原始会话）。
	memoryStore       MemoryStore
	memoryExtractor   MemoryExtractor
	memoryRecallLimit int
	sessionID         string

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

// WithSkillProvider 启用 B 方案真渐进式 Skill（宿主提供技能发现器）。
// 开启后，模型调用 load_skill 会被内核拦截：SKILL.md 正文注入 system 消息
// （而非普通工具结果），同一技能只注入一次。
func WithSkillProvider(sp SkillProvider) Option {
	return func(a *Agent) { a.skillProvider = sp }
}

// WithContextPath 设置当前任务的上下文路径（缺口 C：Skill 路径作用域判定基准）。
// 声明了 Paths 的技能只在其适用目录内可加载；空串 = 不限制（向后兼容）。
func WithContextPath(p string) Option {
	return func(a *Agent) { a.contextPath = p }
}

// WithSkillHistory 替换默认的短期工作记忆（自定义容量；nil 或 size<=0 禁用）。
func WithSkillHistory(h *SkillHistory) Option {
	return func(a *Agent) { a.skillHistory = h }
}

// WithInitialHistory 设置断点续跑的初始上下文（第 5 步阶段一）：
// 传入的 history 必须已含 system 首条（即会话持久化恢复出的完整历史）。
// Run 时不再重建 system 消息，只追加本次 user 任务——上下文延续上次 Run。
// 约定：guard（循环检测）/审批计数等"本轮护栏"状态不随历史恢复
// （跨会话不该继承，新 Run 是新护栏）；B 方案已加载技能可从 history
// 的 system 消息推导（[已加载技能规范 X] 前缀），无需单独序列化。
func WithInitialHistory(msgs []Message) Option {
	return func(a *Agent) { a.initialHistory = msgs }
}

// WithMemoryStore 启用长期记忆层（第 5 步③ 记忆分层）：
// Run 结束时把本次会话关键信息沉淀进 store（Extractor 提取）；
// 新 Run 开始时按任务召回相关记忆注入 system 上下文——只召回关键信息，
// 不恢复全部历史（与 WithInitialHistory 的"整历史恢复"形成分层对照）。
func WithMemoryStore(s MemoryStore) Option {
	return func(a *Agent) { a.memoryStore = s }
}

// WithMemoryExtractor 替换默认记忆提取器（默认 memory 包的启发式实现；
// 可换成调 LLM 的版本，同 Summarizer 模式）。
func WithMemoryExtractor(ex MemoryExtractor) Option {
	return func(a *Agent) { a.memoryExtractor = ex }
}

// WithMemoryRecallLimit 设置每次 Run 的记忆召回上限（默认 3 条）。
func WithMemoryRecallLimit(n int) Option {
	return func(a *Agent) { a.memoryRecallLimit = n }
}

// WithSessionID 设置本次会话 ID（记忆来源标注：Source = 会话 ID + 定位，
// 召回后模型可追溯回原始会话）。
func WithSessionID(id string) Option {
	return func(a *Agent) { a.sessionID = id }
}

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
		loadedSkills:      map[string]bool{},
		loadedRefs:        map[string]bool{},
		skillHistory:      NewSkillHistory(8),
		memoryRecallLimit: recallLimitDefault,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Run 执行任务并返回结果（可能完成，也可能被护栏软停止）。
// 便捷入口：不支持外部取消；需要取消语义（协作式取消契约）用 RunWithContext。
func (a *Agent) Run(task string) RunResult {
	return a.RunWithContext(context.Background(), task)
}

// RunWithContext 执行任务并支持外部取消（协作式取消契约 · 缺口 D）。
//
// ctx.Done() 触发 cancel_requested：任务在下一个安全检查点停止，不领取
// 新步骤；随后按执行日志逆序补偿可补偿步骤，不可逆步骤如实上报
// （partially_completed / needs_review，不把"进程已停"冒充"外部世界已恢复"）。
// ctx 为 nil 时等效 Run。
//
// 安全检查点位置（最多多完成一个可控单元，不截断在任意一行代码）：
//   ① 每步顶部：取消后不开始新步骤；
//   ② 工具执行前：取消后不领取新步骤（不可逆动作额外二次检查——执行前解决）；
//   ③ 工具返回、记账之后：结果已落账，取消收尾不会遗漏（闭合
//      "外部成功后本地落账前"的缝隙）。
//
// 这就是"agent loop"的核心，对应真实 harness 的一轮循环：
//
//	调模型 → 解析工具调用 → 循环检测 → 审批点 → 执行工具 → 回填结果 → 再调模型（直到结束）
//
// 审批点在循环检测之后、工具执行之前：先防"重复空转"，再防"越权动作"。
func (a *Agent) RunWithContext(ctx context.Context, task string) (res RunResult) {
	// 统一收尾（所有返回路径）：
	//   - 注入步骤账本与补偿记录（缺口 D）——停止≠外部无记录；
	//   - completed 时把本次会话关键信息沉淀进长期记忆（第 5 步③）——
	//     中止/取消的会话没有可靠结论，不污染长期记忆。
	defer func() {
		res.Ledger = a.ledger
		res.Compensations = a.compensations
		a.settleMemory(res.Status)
	}()

	if ctx == nil {
		ctx = context.Background()
	}
	a.ctx = ctx
	a.cancelRequested = false
	a.ledger = nil
	a.compensations = nil

	if len(a.initialHistory) > 0 {
		// 断点续跑：恢复的初始历史已含 system 首条与上次对话，
		// 只追加本次 user 消息（复制防外部修改影响运行状态）。
		a.history = append([]Message{}, a.initialHistory...)
		a.history = append(a.history, Message{Role: roleUser, Content: task})
	} else {
		a.history = append(a.history,
			Message{Role: roleSystem, Content: "You are a helpful agent. 可以调用工具完成任务，拿到结果后给出最终回答。"},
			Message{Role: roleUser, Content: task},
		)
	}

	// 第 5 步③：按任务召回长期记忆（关键信息优先），注入 system 上下文。
	// 与 WithInitialHistory 对照：阶段一恢复"整历史"，本步只召回"关键记忆"——
	// 跨会话的轻量延续，token 成本与历史长度无关。
	a.recallMemory(task)

	for step := 1; step <= a.maxSteps; step++ {
		// 安全检查点①：外部取消后不开始新步骤（LLM 调用期间到达的
		// 取消信号在此消费——模型调用本身不是副作用步骤，无需中断）。
		if a.checkCancel() {
			return a.finishCancel(step)
		}
		// 0. 上下文预算检查：超预算先做摘要压缩（context.go 的 CompactContext），
		//    压缩不了才走 FIFO 兜底——对齐真实 harness 的 context compaction。
		//    触发依据优先用**服务端真实 usage**（最近一次响应的 total_tokens，
		//    OpenAI 兼容协议标准字段）：本地估算（MessagesTokens）漏掉角色标记、
		//    工具 schema 等结构性开销，真实 API 计费更高（fast-agent 等行业实现
		//    同样用服务端 usage 触发）。mock 模型不返回 usage → 回退本地估算兜底。
		over := a.lastUsageTokens > a.tokenBudget
		if a.lastUsageTokens == 0 {
			over = MessagesTokens(a.history) > a.tokenBudget // mock 无 usage 时估算兜底
		}
		if over {
			// 压缩目标：usage 触发时压到预算的 1/2——真实 usage 恒大于本地估算
			// （角色/schema 等结构开销），只压到估算 ≤ 预算的话，下一轮真实
			// usage 仍可能超；估算触发（mock）保持原预算即可。
			target := a.tokenBudget
			if a.lastUsageTokens > 0 {
				target = a.tokenBudget / 2
			}
			a.history, _ = CompactContext(a.history, target, a.summarizer)
			if a.verbose {
				if a.lastUsageTokens > 0 {
					log.Printf("[step %d] 上下文压缩：服务端 usage=%d > 预算 %d，压缩后估算 token=%d",
						step, a.lastUsageTokens, a.tokenBudget, MessagesTokens(a.history))
				} else {
					log.Printf("[step %d] 上下文压缩：估算 token=%d > 预算 %d（mock 无 usage，估算兜底），压缩后估算 token=%d",
						step, MessagesTokens(a.history), a.tokenBudget, MessagesTokens(a.history))
				}
			}
			// 缺口 B：压缩后注入「已尝试」短期工作记忆快照——模型看到
			// 之前试过哪些操作、结果如何，就不会语义重复同一失败路径
			// （循环检测只拦精确重复，这里拦"换参数/换工具再来一遍"）。
			// 快照作为独立 system 消息（不改 Summarizer 接口），只在新
			// 快照与上次不同时注入，防刷屏。
			if a.skillHistory != nil {
				if snap := a.skillHistory.String(); snap != "" && snap != a.lastHistorySnap {
					a.history = append(a.history, Message{
						Role:    roleSystem,
						Content: "【已尝试】最近工具调用历史（含失败，供参考避免重复）：\n" + snap,
					})
					a.lastHistorySnap = snap
					if a.verbose {
						log.Printf("[step %d] 注入 Skill History 快照（%d 条调用记录）", step, len(a.skillHistory.Snapshot()))
					}
				}
			}
		}

		// 1. 调模型（把工具清单一起给它，对应 function calling 的 tools 参数）
		resp, err := a.llm.Chat(a.history, a.registry.List())
		if err != nil {
			return RunResult{Steps: step, Status: StatusAborted, Reason: fmt.Sprintf("模型调用失败: %v", err)}
		}
		// 记录服务端真实用量：下一轮循环用它判断是否压缩（真实模型才有效）
		if resp.Usage != nil {
			a.lastUsageTokens = resp.Usage.TotalTokens
		}

		// 2a. 模型请求调用工具（一轮可并行多个，真实模型的多 tool_calls 行为）
		if len(resp.ToolCalls) > 0 {
			// 先把 assistant 消息（含全部 tool_calls）写入历史——
			// 真实 API 要求 assistant 的 tool_calls 数组先于各条 tool 结果出现，
			// 且每条 tool 消息用 tool_call_id 关联。mock 读文本记录即可。
			names := make([]string, 0, len(resp.ToolCalls))
			for _, tc := range resp.ToolCalls {
				names = append(names, tc.Name)
			}
			a.history = append(a.history, Message{
				Role:      roleAssistant,
				Content:   fmt.Sprintf("调用工具 %d 个: %s", len(resp.ToolCalls), strings.Join(names, ", ")),
				ToolCalls: resp.ToolCalls,
			})

			// 处理本轮 tool_calls：两阶段——
			//  阶段1（顺序）：循环检测 + 审批点。两者都有共享状态
			//               （guard 滑动窗口 / denials 计数），必须串行；
			//  阶段2（执行）：全部声明并发安全的普通工具 → goroutine 并行；
			//              否则逐个顺序执行（含 load_skill/read_skill_ref 特殊通道，
			//              它们写内核 map，不并发）。
			// 执行失败的重试预算（guard.retryHit）也集中回主 goroutine 判断，
			// 避免 LoopGuard 状态被并发访问。
			var items []execItem
			for _, tc := range resp.ToolCalls {
				// 2a-1. 循环检测：命中先警告、再犯中止
				switch a.guard.guard(tc.Name, tc.Input) {
				case guardWarn:
					if a.verbose {
						log.Printf("[step %d] ⚠ 循环检测命中（重复调用 %s），该调用被停止", step, tc.Name)
					}
					a.history = append(a.history,
						Message{Role: roleSystem,
							Content: fmt.Sprintf("检测到你重复调用工具 %s(%s) 且无进展。请换一种方式：改用其他工具、修改参数或直接给出最终回答。", tc.Name, tc.Input)},
						// 该调用不执行，但补一条 tool 消息让 tool_calls 协议闭合（真实 API 要求）
						Message{Role: roleTool, Content: "该调用因循环检测被停止，未执行。", ToolCallID: tc.ID},
					)
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
							log.Printf("[step %d] ⛔ 审批拒绝（风险 %s，连续 %d 次）：%s(%s)", step, risk, a.denials, tc.Name, tc.Input)
						}
						// 升级兜底：连续被拒说明模型在反复越权，停止并交还用户
						// （对应 Claude Code auto mode 的"3 次连续拒绝 → 升级给人"）。
						// 这是软停止不是 error，调用方可以展示进度并接手指挥。
						if a.denials >= a.denialLimit {
							return RunResult{Steps: step, Status: StatusAborted,
								Reason: fmt.Sprintf("审批升级：连续 %d 次高风险调用被拒绝，停止并交还人工", a.denials)}
						}
						// 拒绝结果以 tool 角色回填给模型（不是终止）：
						// 模型看到"未授权"可以停止该动作、汇报进展或询问用户。
						// 文案刻意反绕过：明确禁止"效果等价的替代操作"——真实模型
						// 会把"换个方式达成同样目的"理解为合法路径（如删不掉就覆盖
						// 清空），只有把路径②显式封死，才只留下安全分支。
						// 若模型反复请求同一动作，循环检测（上方）也会先警告再中止兜底。
						a.history = append(a.history, Message{
							Role:       roleTool,
							Content:    fmt.Sprintf("调用被拒绝：%s(%s)。用户未授权此操作。请停止该动作，不要尝试效果等价的其他操作或组合方式绕过审批；你可以汇报当前进展、说明受限原因，或询问用户。", tc.Name, tc.Input),
							ToolCallID: tc.ID, // 真实 API 需要 tool 消息关联 tool_call_id
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

				// 判定并发资格：特殊通道（写内核 map）与非并发安全工具 → safe=false。
				// JS/TS ↔ Go 差异：TS 无共享内存并发模型（单线程事件循环），
				// 不需要"并发安全"声明；Go 里 goroutine 共享内存，工具能否并行
				// 执行取决于它是否线程安全——这个声明把责任交给工具作者。
				safe := false
				if tc.Name != LoadSkillToolName && tc.Name != ReadSkillRefToolName {
					safe = a.registry.IsConcurrentSafe(tc.Name)
				}
				items = append(items, execItem{tc: tc, name: tc.Name, input: tc.Input, safe: safe})
			}

			// 阶段2：执行。全部可并发才并行，否则逐个顺序（正确性优先）。
			canParallel := len(items) > 1
			for _, it := range items {
				if !it.safe {
					canParallel = false
					break
				}
			}

			if canParallel {
				// 安全检查点②：执行前检查取消——取消后不启动并行批次
				// （并行批次整体作为一个可控单元；批次内逐工具检查在
				// 顺序路径，并发工具无共享状态，批次边界检查已足够）。
				if a.checkCancel() {
					return a.finishCancel(step)
				}
				if a.verbose {
					log.Printf("[step %d] ⚡ 并行执行 %d 个工具调用", step, len(items))
				}
				results := make([]execResult, len(items))
				var wg sync.WaitGroup
				for i, it := range items {
					wg.Add(1)
					go func(idx int, it execItem) {
						defer wg.Done()
						out, err := a.registry.Call(it.name, it.input)
						results[idx] = execResult{out: out, err: err} // 按索引写，无竞争
					}(i, it)
				}
				wg.Wait()
				for i, it := range items {
					res := results[i]
					ok := res.err == nil
					if res.err != nil {
						if a.guard.retryHit(it.name, it.input) {
							return RunResult{Steps: step, Status: StatusAborted,
								Reason: fmt.Sprintf("重试超预算：%s 连续失败超过 %d 次", it.name, a.guard.retryLimit)}
						}
						res.out = fmt.Sprintf("工具调用失败: %v", res.err)
					} else if verr := a.registry.ValidateResult(it.name, res.out); verr != nil {
						// 缺口 A：结果未通过 schema 校验——不进上下文原文，
						// 回填明确错误并计入重试预算（防模型「以为成功了」）
						ok = false
						if a.guard.retryHit(it.name, it.input) {
							return RunResult{Steps: step, Status: StatusAborted,
								Reason: fmt.Sprintf("重试超预算：%s 结果校验连续失败超过 %d 次", it.name, a.guard.retryLimit)}
						}
						res.out = fmt.Sprintf("工具返回结果未通过校验: %v", verr)
					}
					// 缺口 D：执行后立即记账（请求标识/结果/副作用类型）——
					// 即使取消信号此刻到达，这一步已落账，收尾不会遗漏。
					a.recordStep(step, it.name, it.input, ok)
					if a.verbose {
						log.Printf("[step %d] tool_result ← %s", step, res.out)
					}
					a.recordHistory(step, it.name, it.input, ok)
					a.history = append(a.history,
						Message{Role: roleTool, Content: res.out, ToolCallID: it.tc.ID})
				}
				// 安全检查点③：批次结果全部记账后再查取消——最多多完成
				// 一个可控单元（本批次），不把状态截断在任意一行代码。
				if a.checkCancel() {
					return a.finishCancel(step)
				}
				continue
			}

			// 顺序执行（含特殊通道与未声明并发安全的工具）
			for _, it := range items {
				// 安全检查点②：执行前检查取消——取消后不领取新步骤。
				// 注意在特殊通道判断之前：load_skill/read_skill_ref 虽无
				// 外部副作用（只注入上下文），但取消时也不该继续加载。
				if a.checkCancel() {
					return a.finishCancel(step)
				}
				// 不可逆动作执行前二次检查（执行前解决原则）：即使刚查过，
				// 进入不可逆动作前再确认一次——信号可能在两次检查之间到达。
				if a.registry.SideEffectOf(it.name) == SideIrreversible && a.checkCancel() {
					return a.finishCancel(step)
				}
				if it.tc.Name == LoadSkillToolName {
					a.runLoadSkill(step, it.tc)
					continue
				}
				if it.tc.Name == ReadSkillRefToolName {
					a.runReadSkillRef(step, it.tc)
					continue
				}
				out, err := a.registry.Call(it.name, it.input)
				ok := err == nil
				if err != nil {
					if a.guard.retryHit(it.name, it.input) {
						return RunResult{Steps: step, Status: StatusAborted,
							Reason: fmt.Sprintf("重试超预算：%s 连续失败超过 %d 次", it.name, a.guard.retryLimit)}
					}
					out = fmt.Sprintf("工具调用失败: %v", err)
				} else if verr := a.registry.ValidateResult(it.name, out); verr != nil {
					// 缺口 A：结果未通过 schema 校验（同上，顺序执行路径）
					ok = false
					if a.guard.retryHit(it.name, it.input) {
						return RunResult{Steps: step, Status: StatusAborted,
							Reason: fmt.Sprintf("重试超预算：%s 结果校验连续失败超过 %d 次", it.name, a.guard.retryLimit)}
					}
					out = fmt.Sprintf("工具返回结果未通过校验: %v", verr)
				}
				// 缺口 D：执行后立即记账——"外部成功后本地落账前"的缝隙
				// 由记账闭合：即使此刻取消，账本已有该步骤，收尾不会遗漏。
				a.recordStep(step, it.name, it.input, ok)
				if a.verbose {
					log.Printf("[step %d] tool_result ← %s", step, out)
				}
				a.recordHistory(step, it.name, it.input, ok)
				a.history = append(a.history,
					Message{Role: roleTool, Content: out, ToolCallID: it.tc.ID})
				// 安全检查点③：记录结果后再检查取消——最多多完成一个可控单元。
				if a.checkCancel() {
					return a.finishCancel(step)
				}
			}
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

// runLoadSkill 处理 load_skill 特殊通道（B 方案真渐进式披露）。
//
// 与 A 方案（正文作为工具结果回填）的关键差异：
//  1. 正文以 system 角色进入上下文——优先级高于普通对话/tool 消息，
//     且语义是"技能规范"，模型按规则执行而非当作"一条返回数据"；
//  2. 同一技能去重：只注入一次，防模型反复加载刷上下文；
//  3. 协议仍闭合：无论成功/失败/重复，都补一条 tool 消息关联 tool_call_id，
//     满足真实 API "assistant 的 tool_calls 必须有对应 tool 结果"的要求。
func (a *Agent) runLoadSkill(step int, tc ToolCall) {
	if a.skillProvider == nil {
		a.appendTool(tc.ID, "未配置技能加载器（未调用 WithSkillProvider），无法加载技能。")
		return
	}
	name := parseLoadSkillInput(tc.Input)
	if name == "" {
		a.appendTool(tc.ID, "load_skill 参数无效：应为 {\"name\":\"技能名\"} 或直接给技能名。")
		return
	}
	// 缺口 C：路径作用域校验。技能声明了适用目录而当前上下文路径不在其中
	// → 拒绝加载（即使宿主侧候选清单被绕过/描述过期，内核仍兜底拦截）。
	// 同时区分"技能不存在"与"不适用"两类错误，模型得到的指引不同。
	if a.contextPath != "" {
		catalog := a.skillProvider.List()
		var meta *SkillMeta
		for i := range catalog {
			if catalog[i].Name == name {
				meta = &catalog[i]
				break
			}
		}
		if meta == nil {
			a.appendTool(tc.ID, fmt.Sprintf("技能 %s 不存在或不在可用清单中，请改用其他技能或直接完成任务。", name))
			return
		}
		if !SkillAllowedForPath(*meta, a.contextPath) {
			a.appendTool(tc.ID, fmt.Sprintf("技能 %s 仅适用于目录范围 %v，当前上下文路径 %s 不在其内，请改用其他技能或直接完成任务。",
				name, meta.Paths, a.contextPath))
			return
		}
	}
	if a.loadedSkills[name] {
		a.appendTool(tc.ID, fmt.Sprintf("技能 %s 已加载过，其规范已在系统上下文中，无需重复加载。", name))
		return
	}
	body, ok := a.skillProvider.LoadSkill(name)
	if !ok {
		a.appendTool(tc.ID, fmt.Sprintf("技能 %s 不存在或无法加载，请改用其他技能或直接完成任务。", name))
		return
	}
	a.loadedSkills[name] = true
	if a.verbose {
		log.Printf("[step %d] skill → %s（正文 %d 字符注入 system 消息）", step, name, len(body))
	}
	// 正文进 system（B 方案的标志性行为）；skill 名打标记便于模型/审计识别来源。
	a.history = append(a.history, Message{
		Role:    roleSystem,
		Content: fmt.Sprintf("[已加载技能规范 %s]\n%s", name, body),
	})
	a.appendTool(tc.ID, fmt.Sprintf("技能 %s 已加载，规范已注入系统上下文，请严格按规范执行后续操作。", name))
}

// appendTool 以 tool 角色回填一条结果（协议闭合用，对应真实 API 的 tool 消息）。
func (a *Agent) appendTool(toolCallID, content string) {
	a.history = append(a.history, Message{Role: roleTool, Content: content, ToolCallID: toolCallID})
}

// recordHistory 把一次普通工具调用记入短期工作记忆（缺口 B）。
// 特殊通道（load_skill / read_skill_ref）不记录：它们有独立的去重机制
// （loadedSkills / loadedRefs），重复防护已覆盖，无需再进历史快照。
func (a *Agent) recordHistory(step int, name, input string, ok bool) {
	if a.skillHistory != nil {
		a.skillHistory.Record(step, name, input, ok)
	}
}

// runReadSkillRef 处理 read_skill_ref 特殊通道（L3 按需引用）。
//
// 设计约束（防呆 + 防滥用）：
//  1. 顺序依赖：必须先 load_skill 加载技能正文，才能读它的引用——
//     引用脱离正文没有意义，也防止模型绕过 L2 直接读任意资源；
//  2. 去重：同一 (skill, ref) 只注入一次（key 用 "skill/ref"）；
//  3. 协议闭合：成功/失败/重复都回填 tool 消息。
func (a *Agent) runReadSkillRef(step int, tc ToolCall) {
	if a.skillProvider == nil {
		a.appendTool(tc.ID, "未配置技能加载器（未调用 WithSkillProvider），无法读取技能引用。")
		return
	}
	skill, ref, ok := parseReadSkillRefInput(tc.Input)
	if !ok {
		a.appendTool(tc.ID, "read_skill_ref 参数无效：应为 {\"skill\":\"技能名\",\"ref\":\"引用路径\"}。")
		return
	}
	if !a.loadedSkills[skill] {
		a.appendTool(tc.ID, fmt.Sprintf("技能 %s 尚未加载，请先调用 load_skill 加载其正文，再读取引用。", skill))
		return
	}
	key := skill + "/" + ref
	if a.loadedRefs[key] {
		a.appendTool(tc.ID, fmt.Sprintf("技能 %s 的引用 %s 已加载过，内容已在系统上下文中，无需重复读取。", skill, ref))
		return
	}
	body, ok := a.skillProvider.LoadReference(skill, ref)
	if !ok {
		a.appendTool(tc.ID, fmt.Sprintf("技能 %s 的引用 %s 不存在或无法加载。", skill, ref))
		return
	}
	a.loadedRefs[key] = true
	if a.verbose {
		log.Printf("[step %d] skill_ref → %s/%s（%d 字符注入 system 消息）", step, skill, ref, len(body))
	}
	a.history = append(a.history, Message{
		Role:    roleSystem,
		Content: fmt.Sprintf("[技能 %s 引用 %s]\n%s", skill, ref, body),
	})
	a.appendTool(tc.ID, fmt.Sprintf("已加载技能 %s 的引用 %s，内容已注入系统上下文，请按其中规则执行。", skill, ref))
}

// History 返回本轮对话轨迹（只读引用，调用方不得修改）。
// 对应生产 harness 的 trajectory 暴露：评测门（eval）、审计、回放都依赖它——
// 只给 RunResult 而不给轨迹，外部无法判断"为什么成功/失败"。
func (a *Agent) History() []Message {
	return a.history
}
