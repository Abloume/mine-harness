package agent

import (
	"fmt"
	"strings"
)

// 风险分级（对应业界共识的审批判定维度：可逆性 × 影响半径 × 成本）：
//
//	RiskNone   读操作，零副作用（查天气、检索）
//	RiskLow    可逆、影响小（写临时文件、试运行）
//	RiskMedium 有副作用但可撤销（改配置，有备份/回滚）
//	RiskHigh   不可逆或影响他人（删除/覆盖/发送/支付/发布）
//
// 分级取代了早期的 bool 标记（RequiresApproval）——理由是"参数感知"：
// 同一工具不同参数风险可能完全不同（读文件 vs 删文件、发草稿 vs 直接发），
// 静态 bool 表达不了这种差异；分级 + 运行时修正（RiskEvaluator）可以。
//
// 对应生产判断：OpenAI Agents SDK / LangGraph interrupt_on 是"工具声明 + 参数
// 级审批"；Claude Code auto mode 用分类器动态标记危险动作——这里的
// FuncRiskEvaluator 就是这个"动态判定"的最小可插拔形态。
type RiskLevel int

const (
	RiskNone RiskLevel = iota
	RiskLow
	RiskMedium
	RiskHigh
)

func (r RiskLevel) String() string {
	switch r {
	case RiskNone:
		return "L0-none"
	case RiskLow:
		return "L1-low"
	case RiskMedium:
		return "L2-medium"
	case RiskHigh:
		return "L3-high"
	}
	return "unknown"
}

// RiskEvaluator 判定一次调用的最终风险：工具基础级别 + 参数/上下文修正。
//
// JS/TS ↔ Go 差异：这本质是"策略注入"——TS 里常传回调/高阶函数；
// Go 用 interface + 函数适配器（FuncRiskEvaluator），和 http.HandlerFunc 同套路。
type RiskEvaluator interface {
	Evaluate(toolName, args string, base RiskLevel) RiskLevel
}

// StaticRiskEvaluator 默认实现：不做修正，直接返回工具声明的基础级别。
// 相当于"全部风险由工具作者静态声明"——最简单也最常见的生产做法。
type StaticRiskEvaluator struct{}

func (StaticRiskEvaluator) Evaluate(_ string, _ string, base RiskLevel) RiskLevel {
	return base
}

// FuncRiskEvaluator 函数适配器：让普通函数实现 RiskEvaluator。
// 参数感知示例：delete 关键词 → 升到 RiskHigh，其余保持基础级别。
type FuncRiskEvaluator func(toolName, args string, base RiskLevel) RiskLevel

func (f FuncRiskEvaluator) Evaluate(toolName, args string, base RiskLevel) RiskLevel {
	return f(toolName, args, base)
}

// LLMRiskEvaluator 用 LLM 做动态风险判断——"模型自动判断审批点"的最小形态
// （对应 Claude Code auto mode 的 transcript classifier 思路：模型/分类器
// 动态标记危险动作，而不是全靠人写死）。
//
// 安全语义（关键，规则优先、模型辅助的落地）：
//   - **模型判断只能上调风险，不能下调**：上调 = 把规则没标出来的"灰色地带"
//     动作挑出来送审；下调 = 禁止——低风险自动放行仍由规则（审批阈值）决定。
//     这是防"自利偏差"（模型判断自己的动作危不危险会系统性低估）的手段；
//   - **解析失败不静默放行**：模型调用失败或返回无法解析 → 保守升级到
//     maxRisk(基础风险, RiskMedium)。原因是真实踩坑：混合思考模型（如
//     glm-4.7-flash）在 max_tokens 太小时 reasoning 吃掉全部预算，content
//     为空 → parseRisk 失败；如果此时退回基础风险（常是 LOW），删除这类
//     高风险动作会被静默放行——宁多审、勿漏审（不对称风险论）。
//
// 注意：风险评估用**独立的 LLM 实例**，不占用主 agent 的模型调用预算——
// 生产里这一步通常是专用小模型/分类器（便宜、快、确定性）。
type LLMRiskEvaluator struct {
	llm LLM
}

// NewLLMRiskEvaluator 构造基于 LLM 的动态风险评估器。
func NewLLMRiskEvaluator(llm LLM) *LLMRiskEvaluator {
	return &LLMRiskEvaluator{llm: llm}
}

// Evaluate 调模型判断风险，返回"基础风险 与 模型判断"中较高者（只升不降）。
func (e *LLMRiskEvaluator) Evaluate(toolName, args string, base RiskLevel) RiskLevel {
	resp, err := e.llm.Chat([]Message{
		{
			Role: roleSystem,
			Content: "你是工具调用风险评估器。只输出风险级别名：NONE / LOW / MEDIUM / HIGH。规则：" +
				"不可逆或影响他人的操作（删除、覆盖、发送消息、支付、发布、写生产数据）为 HIGH；" +
				"有副作用但可撤销的为 MEDIUM；只读或零副作用的为 NONE 或 LOW。",
		},
		{
			Role:    roleUser,
			Content: fmt.Sprintf("工具：%s\n参数：%s\n请判断这次调用的风险级别（只输出级别名）。", toolName, args),
		},
	}, nil)
	if err != nil {
		return maxRisk(base, RiskMedium) // 模型不可用：保守升级，宁可送审不可漏审
	}
	if lv, ok := parseRisk(resp.Content); ok {
		return maxRisk(base, lv) // 只升不降
	}
	return maxRisk(base, RiskMedium) // 解析失败（如 content 为空）：同样保守升级
}

// parseRisk 把模型返回的文本解析成 RiskLevel。无法识别返回 ok=false，
// 由调用方决定兜底策略（LLMRiskEvaluator 退回基础风险）。
func parseRisk(s string) (RiskLevel, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "NONE", "L0", "L0-NONE":
		return RiskNone, true
	case "LOW", "L1", "L1-LOW":
		return RiskLow, true
	case "MEDIUM", "MED", "L2", "L2-MEDIUM":
		return RiskMedium, true
	case "HIGH", "L3", "L3-HIGH":
		return RiskHigh, true
	}
	return RiskNone, false
}

// maxRisk 取两个风险级别中较高者——"模型只能上调"的落地。
func maxRisk(a, b RiskLevel) RiskLevel {
	if a > b {
		return a
	}
	return b
}
