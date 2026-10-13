package agent

import "fmt"

// SideEffect 描述工具调用的副作用类型（协作式取消契约 · 缺口 D）。
//
// 设计对齐外部实践的三分类：
//   - Readonly：查询/读取，取消后直接结束，无需收尾；
//   - Compensatable：会留下可撤销的影响（创建草稿、占用临时资源），
//     必须声明 Compensation——任务取消时内核按执行日志逆序调用补偿；
//   - Irreversible：影响外部系统且无法撤销（创建退款、发送消息），
//     执行前内核做取消二次检查，执行后如实上报（不假装回滚成功）。
//
// 与 BaseRisk 分工：风险分级管「该不该做」（审批闸门，前置护栏），
// 副作用类型管「做了之后怎么撤」（取消补偿，事后契约）——互不重叠。
type SideEffect int

const (
	SideReadonly      SideEffect = iota // 只读：取消后直接结束
	SideCompensatable                   // 可补偿：需声明 Compensation
	SideIrreversible                    // 不可逆：执行前二次检查，执行后如实上报
)

// Compensation 是可补偿工具的补偿动作声明（副作用契约 · 缺口 D）。
// Exec 与原 Execute 同签名（input 进、string 出）；任务取消时内核按
// 执行日志逆序调用。补偿本身可能失败——内核记录尝试与错误，
// 最终状态 needs_review 交还人工（不是假装回滚成功）。
type Compensation struct {
	Action string                             // 补偿动作的人类可读描述（账本/审计可见）
	Exec   func(input string) (string, error) // 实际补偿执行
}

// Tool 描述一个可被模型调用的工具。
//
// BaseRisk 声明工具的静态基础风险（人定规则），运行时可按参数修正
// （同一工具不同参数风险不同：读文件 vs 删文件）——见 risk.go 的 RiskEvaluator。
// 审批闸门按"基础级别 + 参数修正"的最终风险决定是否触发。
//
// ConcurrentSafe 声明工具是否线程安全：同一轮多 tool_call 时，只有全部
// 声明并发安全的工具才会被 goroutine 并行执行；未声明（false）的工具
// 保持顺序执行——并发是优化，正确性优先（工具可能共享可变状态）。
//
// ResultValidator 可选地声明"返回结果校验器"（缺口 A：工具返回结果 schema
// 校验）：工具执行成功后、结果回填上下文之前，内核调用它校验输出
// （如必须是合法 JSON 且含关键字段）。返回非 nil 表示校验失败——该结果
// **不进上下文**，回填明确错误并计入重试预算（防模型"以为成功了"）。
// nil = 不校验（向后兼容：现有工具默认不声明，行为不变）。
//
// SideEffect 声明副作用类型（缺口 D，默认 SideReadonly 向后兼容）；
// Compensate 声明补偿动作（仅 SideCompensatable 有意义，nil = 未声明，
// 取消时该步骤记入 needs_review 而非自动补偿）；
// Idempotent 声明工具幂等（账本以 input 哈希为请求标识，超时/取消后可
// 安全重试或查结果——真实实现由工具侧生成外部单号并支持按单号查询）。
type Tool struct {
	Name            string
	Description     string
	BaseRisk        RiskLevel                          // 基础风险级别（RiskNone..RiskHigh），默认 0 即 RiskNone
	ConcurrentSafe  bool                               // 声明并发安全后参与并行执行
	Execute         func(input string) (string, error) // 简化：JSON 字符串进、字符串出
	ResultValidator func(output string) error          // 可选：返回结果 schema 校验（nil = 不校验）
	SideEffect      SideEffect                         // 副作用类型（默认 SideReadonly）
	Compensate      *Compensation                      // 补偿动作（仅 Compensatable；nil = 未声明）
	Idempotent      bool                               // 声明幂等（账本请求标识可复用）
}

// Registry 是工具注册表：模型只能调用已注册的工具——这本身就是一层护栏
// （对应生产 harness 中"工具白名单 + 权限控制"的最小雏形）。
//
// JS/TS ↔ Go 差异：TS 常用 Map<string, Tool>；Go 用 map[string]Tool 语法近似。
// 但 Go 的 map 读不存在的 key 返回零值（不报错），必须查 ok 才知道是否存在，
// 这一点和 TS 的 Map.has() 语义对应。
type Registry struct {
	tools map[string]Tool
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册一个工具，同名工具后注册的覆盖先注册的。
func (r *Registry) Register(t Tool) {
	r.tools[t.Name] = t
}

// List 返回全部已注册工具（供 LLM 接口感知可用工具）。
func (r *Registry) List() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// BaseRiskOf 返回工具声明的静态基础风险；未注册工具返回 RiskNone
// （反正会被 Call 护栏拦住，这里不额外报错）。
func (r *Registry) BaseRiskOf(name string) RiskLevel {
	t, ok := r.tools[name]
	if !ok {
		return RiskNone
	}
	return t.BaseRisk
}

// IsConcurrentSafe 查询工具是否声明并发安全（可参与并行执行）。
func (r *Registry) IsConcurrentSafe(name string) bool {
	t, ok := r.tools[name]
	return ok && t.ConcurrentSafe
}

// SideEffectOf 返回工具声明的副作用类型；未注册/未声明返回 SideReadonly
// （默认只读最安全：取消时无需收尾）。
func (r *Registry) SideEffectOf(name string) SideEffect {
	t, ok := r.tools[name]
	if !ok {
		return SideReadonly
	}
	return t.SideEffect
}

// CompensateOf 返回工具的补偿动作声明；未注册/未声明返回 (nil,false)。
// 声明了 SideCompensatable 却没给 Compensate 的工具，取消时该步骤
// 无法自动收尾 → 状态 needs_review（如实告知，不假装回滚）。
func (r *Registry) CompensateOf(name string) (*Compensation, bool) {
	t, ok := r.tools[name]
	if !ok || t.Compensate == nil {
		return nil, false
	}
	return t.Compensate, true
}

// IsIdempotent 查询工具是否声明幂等（账本请求标识可安全复用）。
func (r *Registry) IsIdempotent(name string) bool {
	t, ok := r.tools[name]
	return ok && t.Idempotent
}

// Call 执行指定工具并返回结果。
//
// JS/TS ↔ Go 差异：JS/TS 习惯 throw 异常；Go 把错误作为返回值显式传递，
// 调用方必须处理 err——这就是 Go 著名的"错误显式"哲学。
func (r *Registry) Call(name, input string) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("未知工具: %s（护栏拦截，模型不能调用未注册工具）", name)
	}
	return t.Execute(input)
}

// ValidateResult 在工具执行成功后调用：校验返回结果是否满足工具声明的
// schema（ResultValidator）。未注册工具 / 未声明校验器返回 nil（放行）。
//
// JS/TS ↔ Go 差异：TS 常在 async 工具内部自行 throw 校验错误；这里刻意把
// "工具只管产出、校验是内核通用层"拆开——校验失败与执行失败是两类错误，
// 前者提示"结果格式不对"，后者提示"动作没做成"，模型收到的文案不同。
func (r *Registry) ValidateResult(name, output string) error {
	t, ok := r.tools[name]
	if !ok || t.ResultValidator == nil {
		return nil
	}
	return t.ResultValidator(output)
}
