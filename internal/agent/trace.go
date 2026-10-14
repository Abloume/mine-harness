package agent

import (
	"fmt"
	"time"
)

// 结构化轨迹导出（增量 A · 可观测性前置）。
//
// 定位：评估与监测不能拖到项目最后临时补。轨迹事件流让每次 Run 都能回答
// 「模型为何选/不选某 skill、审批在哪一步通过/拒绝、Token 与延迟多少」，
// 也喂第 8 步的回放工具与评测门。
//
// 与步骤账本（StepRecord）的分工：
//   - 账本：已执行工具调用的"事实记录"（请求标识/副作用/补偿），取消收尾用；
//   - 轨迹：整条链路的"过程记录"（模型调用、护栏、审批、Skill 决策、压缩），
//     监测与回放用。轨迹包含账本没有的环节（llm 调用、审批决策、skill 加载、
//     上下文压缩），账本包含轨迹没有的细节（幂等键、补偿状态）——两者互补。
//
// 字段按监测清单预留（请求级 / 链路级 / 质量与成本）：
//   - 请求级：Model、PromptTokens/CompletionTokens/TotalTokens、CacheHit、
//     FirstTokenMs/LatencyMs（真实模型才有值，mock 留空——字段先占位）；
//   - 链路级：Type 区分环节、Step/Name/Input/Outcome/Detail 定位决策；
//   - 质量与成本：Token 汇总可算单次任务成本，Outcome=denied/guarded 可数拒答率。
//
// JS/TS ↔ Go 差异：JS/TS 常用 console.log 打散日志或第三方 tracing SDK；
// Go 里用类型化 struct + 有序切片做事件流，JSON 序列化后天然可落盘/可回放
// （json.Marshal 直接出结构化日志，无需 SDK）。

// 轨迹事件类型（TraceEvent.Type 取值）。
const (
	TraceLLM      = "llm"      // 模型调用：usage / 延迟 / 缓存（请求级元数据）
	TraceTool     = "tool"     // 工具执行结果：outcome = ok/failed/invalid/denied/guarded
	TraceApproval = "approval" // 审批决策：outcome = approved/denied，Detail 含风险级别
	TraceSkill    = "skill"    // load_skill / read_skill_ref 决策：outcome = loaded/rejected/...
	TraceCompact  = "compact"  // 上下文压缩：Detail 含触发原因与压缩前后估算
	TraceCancel   = "cancel"   // 取消/补偿收尾：Detail 含终态
)

// 工具调用结果分类（TraceTool.Outcome 取值）。
const (
	OutcomeOK         = "ok"         // 执行成功且通过结果校验
	OutcomeFailed     = "failed"     // 执行失败（工具报错）
	OutcomeInvalid    = "invalid"    // 结果未通过 schema 校验（缺口 A）
	OutcomeDenied     = "denied"     // 被审批拒绝（未执行）
	OutcomeGuarded    = "guarded"    // 被循环检测拦截（未执行）
	OutcomeRetryLimit = "retry-limit" // 重试超预算中止（未执行）
)

// TraceEvent 是一条轨迹事件：某个环节发生了什么、结果如何。
// 全部字段 JSON 友好（小写 tag），直接序列化落盘为 traces/<run>.json。
type TraceEvent struct {
	Seq   int    `json:"seq"`             // 事件序号（时间顺序，审计/回放稳定）
	At    string `json:"at"`              // 发生时刻（RFC3339）
	Type  string `json:"type"`            // 事件类型（见 Trace* 常量）
	Step  int    `json:"step,omitempty"`  // 主循环步号（非循环环节为 0）
	Name  string `json:"name,omitempty"`  // 涉及的工具 / 技能名
	Input string `json:"input,omitempty"` // 入参摘要（截断 200 字符，防爆轨迹体积）

	// Outcome 按类型含义不同：tool=结果分类；approval=approved/denied；
	// skill=loaded/rejected/not-found/already-loaded；compact=cause。
	Outcome string `json:"outcome,omitempty"`
	Detail  string `json:"detail,omitempty"` // 补充说明（风险级别、失败原因、压缩前后）

	// 请求级元数据（仅 TraceLLM 事件有意义；mock 无真实值则留空/0）：
	Model            string `json:"model,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	TotalTokens      int    `json:"total_tokens,omitempty"`
	CacheHit         bool   `json:"cache_hit,omitempty"`   // 上下文缓存命中（真实 API 才可判）
	FirstTokenMs     int64  `json:"first_token_ms,omitempty"` // 首字延迟（真实链路预留）
	LatencyMs        int64  `json:"latency_ms,omitempty"`    // 总延迟（真实链路预留）
}

// WithModel 设置模型名（轨迹与评测的请求级元数据；真实 demo 传模型 ID，
// mock demo 可传 "mock"）。不设置则轨迹里 Model 为空。
func WithModel(name string) Option {
	return func(a *Agent) { a.model = name }
}

// appendTrace 追加一条轨迹事件（自动编号、打时间戳）。
// 所有埋点共用它，保证 seq 有序递增——回放/审计按序消费即可。
func (a *Agent) appendTrace(ev TraceEvent) {
	ev.Seq = len(a.trace) + 1
	if ev.At == "" {
		ev.At = time.Now().Format(time.RFC3339)
	}
	a.trace = append(a.trace, ev)
}

// traceInput 截断入参摘要：轨迹只记录"调了什么"，完整入参在账本（StepRecord.Input）里。
func traceInput(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("…（截断，共 %d 字符）", len(s))
}
