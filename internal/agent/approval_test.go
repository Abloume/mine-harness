package agent

import (
	"strings"
	"testing"
)

// 分级审批点（human-in-the-loop）行为测试，覆盖：
//  1. 达到阈值的调用被决策器拒绝 → 工具不执行
//  2. 达到阈值的调用被放行 → 工具正常执行
//  3. fail-closed：达到阈值但没配 approver → 默认拒绝（安全默认）
//  4. 低于阈值的调用自动放行：无需 approver 也正常执行（不打扰用户）
//  5. 参数感知：同一工具不同参数定不同风险（read 放行 / delete 审批）
//  6. 升级兜底：连续拒绝达上限 → 中止（对应 Claude Code 3 连拒升级）
//  7. 模型自动判断风险（LLMRiskEvaluator）：只升不降 + 失败兜底

func TestApprovalDeniedSkipsExecution(t *testing.T) {
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "delete_file",
		BaseRisk: RiskHigh, // 达到默认阈值 RiskMedium，必须审批
		Execute: func(input string) (string, error) {
			executed++
			return "deleted", nil
		},
	})

	// 模型先请求高风险工具，被拒后直接回答（换策略）
	llm := NewMockLLM([]MockDecision{
		{ToolName: "delete_file", ToolInput: "/tmp/a"},
		{Content: "无法删除：用户未授权。"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(3), WithApprover(DenyApprover{}))
	r := a.Run("删除 /tmp/a")

	if r.Status != StatusCompleted {
		t.Fatalf("期望 completed，实际 %s（原因 %s）", r.Status, r.Reason)
	}
	if executed != 0 {
		t.Errorf("审批拒绝后工具仍被执行 %d 次", executed)
	}
}

func TestApprovalGrantedExecutes(t *testing.T) {
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "delete_file",
		BaseRisk: RiskHigh,
		Execute: func(input string) (string, error) {
			executed++
			return "deleted", nil
		},
	})

	llm := NewMockLLM([]MockDecision{
		{ToolName: "delete_file", ToolInput: "/tmp/a"},
		{Content: "已删除。"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(3), WithApprover(AutoApprover{}))
	r := a.Run("删除 /tmp/a")

	if r.Status != StatusCompleted {
		t.Fatalf("期望 completed，实际 %s（原因 %s）", r.Status, r.Reason)
	}
	if executed != 1 {
		t.Errorf("审批通过后应执行 1 次，实际 %d", executed)
	}
}

func TestApprovalFailClosedWithoutApprover(t *testing.T) {
	// fail-closed：达到阈值的工具没配 approver → 默认拒绝，而不是"没配置就放行"。
	// 宁可误杀，不可越权。
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "delete_file",
		BaseRisk: RiskHigh,
		Execute: func(input string) (string, error) {
			executed++
			return "deleted", nil
		},
	})

	llm := NewMockLLM([]MockDecision{
		{ToolName: "delete_file", ToolInput: "/tmp/a"},
		{Content: "无法删除：未授权。"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(3)) // 不传 approver
	r := a.Run("删除 /tmp/a")

	if r.Status != StatusCompleted {
		t.Fatalf("期望 completed，实际 %s（原因 %s）", r.Status, r.Reason)
	}
	if executed != 0 {
		t.Errorf("fail-closed 未生效：工具被执行 %d 次", executed)
	}
}

func TestApprovalAutoAllowsBelowThreshold(t *testing.T) {
	// 低于审批阈值的调用自动放行：无需 approver、无需任何审批开销，
	// 这是"分级"相对 bool 的关键收益——低风险操作不打扰用户。
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "get_weather",
		BaseRisk: RiskNone, // 低于默认阈值 RiskMedium
		Execute: func(input string) (string, error) {
			executed++
			return `{"city":"北京","weather":"晴"}`, nil
		},
	})

	llm := NewMockLLM([]MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{Content: "北京晴。"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(3)) // 无 approver
	r := a.Run("查北京天气")

	if r.Status != StatusCompleted {
		t.Fatalf("期望 completed，实际 %s（原因 %s）", r.Status, r.Reason)
	}
	if executed != 1 {
		t.Errorf("低风险工具应自动放行并执行 1 次，实际 %d", executed)
	}
}

func TestApprovalParameterAwareRisk(t *testing.T) {
	// 参数感知：同一工具按参数定风险——read 自动放行、delete 审批拒绝。
	// 这是"分级 + RiskEvaluator"相对静态 bool 的核心改进。
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "file_op",
		BaseRisk: RiskLow,
		Execute: func(input string) (string, error) {
			executed++
			return "op done", nil
		},
	})

	riskEval := FuncRiskEvaluator(func(name, args string, base RiskLevel) RiskLevel {
		if strings.Contains(args, "delete") {
			return RiskHigh
		}
		return base // RiskLow < 阈值，自动放行
	})

	llm := NewMockLLM([]MockDecision{
		{ToolName: "file_op", ToolInput: `{"action":"delete","file":"/tmp/a"}`},
		{ToolName: "file_op", ToolInput: `{"action":"read","file":"/tmp/a"}`},
		{Content: "读取成功，删除未执行。"},
	})

	a := NewAgent(llm, reg,
		WithMaxSteps(4),
		WithRiskEvaluator(riskEval),
		WithApprover(DenyApprover{})) // 只拒绝进闸门的（delete）
	r := a.Run("删除再读取文件")

	if r.Status != StatusCompleted {
		t.Fatalf("期望 completed，实际 %s（原因 %s）", r.Status, r.Reason)
	}
	if executed != 1 {
		t.Errorf("delete 应被拒（0 次）、read 应放行（1 次），实际总执行 %d 次", executed)
	}
}

func TestApprovalEscalationAfterDenials(t *testing.T) {
	// 升级兜底：连续拒绝达上限 → 中止（对应 Claude Code 3 连拒升级给人）。
	// 防的是"模型反复越权不改路径"——比循环检测更早、更明确的信号。
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "delete_file",
		BaseRisk: RiskHigh,
		Execute: func(input string) (string, error) {
			executed++
			return "deleted", nil
		},
	})

	// 病态模型：死不悔改，连试 5 次 delete
	steps := make([]MockDecision, 0, 5)
	for i := 0; i < 5; i++ {
		steps = append(steps, MockDecision{ToolName: "delete_file", ToolInput: "/tmp/a"})
	}
	llm := NewMockLLM(steps)

	a := NewAgent(llm, reg, WithMaxSteps(5), WithApprover(DenyApprover{}), WithDenialLimit(2))
	r := a.Run("删除 /tmp/a")

	if r.Status != StatusAborted {
		t.Fatalf("期望 aborted（升级中止），实际 %s", r.Status)
	}
	if !strings.Contains(r.Reason, "审批升级") {
		t.Errorf("中止原因应含'审批升级'，实际: %s", r.Reason)
	}
	if executed != 0 {
		t.Errorf("升级中止前不应有任何工具执行，实际 %d", executed)
	}
}

// ===== LLMRiskEvaluator：模型自动判断风险 =====

func TestLLMRiskEvaluatorUpscalesRisk(t *testing.T) {
	// 模型把灰色地带动作判为 HIGH → 最终风险上调（触发审批）。
	riskLLM := NewMockLLM([]MockDecision{{Content: "HIGH"}})
	e := NewLLMRiskEvaluator(riskLLM)

	got := e.Evaluate("file_op", `{"action":"delete"}`, RiskLow)
	if got != RiskHigh {
		t.Errorf("期望模型判定后风险升级为 HIGH，实际 %s", got)
	}
}

func TestLLMRiskEvaluatorCannotDowngrade(t *testing.T) {
	// 关键安全语义：模型判断只能上调，不能下调。
	// 基础风险 HIGH 的工具，即使模型误判为 NONE，最终仍是 HIGH（不放松）。
	riskLLM := NewMockLLM([]MockDecision{{Content: "NONE"}})
	e := NewLLMRiskEvaluator(riskLLM)

	got := e.Evaluate("delete_file", "/tmp/a", RiskHigh)
	if got != RiskHigh {
		t.Errorf("模型不能下调基础风险：期望 HIGH，实际 %s", got)
	}
}

func TestLLMRiskEvaluatorFallbackOnError(t *testing.T) {
	// 模型不可用（脚本耗尽报错）→ 退回基础风险（保守兜底）。
	riskLLM := NewMockLLM(nil) // 无脚本，Chat 必然报"脚本已耗尽"
	e := NewLLMRiskEvaluator(riskLLM)

	got := e.Evaluate("file_op", `{"action":"delete"}`, RiskMedium)
	if got != RiskMedium {
		t.Errorf("模型失败应退回基础风险 MEDIUM，实际 %s", got)
	}
}

func TestLLMRiskEvaluatorFallbackOnUnparsable(t *testing.T) {
	// 模型返回无法解析的文本 → 同样退回基础风险（fail-closed 方向）。
	riskLLM := NewMockLLM([]MockDecision{{Content: "这个操作很危险我觉得"}})
	e := NewLLMRiskEvaluator(riskLLM)

	got := e.Evaluate("file_op", `{"action":"delete"}`, RiskLow)
	if got != RiskLow {
		t.Errorf("无法解析应退回基础风险 LOW，实际 %s", got)
	}
}

func TestApprovalWithLLMRiskEvaluatorEndToEnd(t *testing.T) {
	// 端到端：主 agent + 独立风险模型。delete 被风险模型判 HIGH → 审批拒绝
	// （工具不执行）；read 判 LOW → 自动放行（执行 1 次）。
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "file_op",
		BaseRisk: RiskLow,
		Execute: func(input string) (string, error) {
			executed++
			return "op done", nil
		},
	})

	riskLLM := NewMockLLM([]MockDecision{
		{Content: "HIGH"}, // delete → 高风险
		{Content: "LOW"},  // read → 低风险
	})
	riskEval := NewLLMRiskEvaluator(riskLLM)

	llm := NewMockLLM([]MockDecision{
		{ToolName: "file_op", ToolInput: `{"action":"delete","file":"/tmp/a"}`},
		{ToolName: "file_op", ToolInput: `{"action":"read","file":"/tmp/a"}`},
		{Content: "读取成功，删除未执行。"},
	})

	a := NewAgent(llm, reg,
		WithMaxSteps(4),
		WithRiskEvaluator(riskEval),
		WithApprover(DenyApprover{}))
	r := a.Run("删除再读取文件")

	if r.Status != StatusCompleted {
		t.Fatalf("期望 completed，实际 %s（原因 %s）", r.Status, r.Reason)
	}
	if executed != 1 {
		t.Errorf("delete 应被模型判高险并拒绝（0 次）、read 应放行（1 次），实际总执行 %d 次", executed)
	}
}
