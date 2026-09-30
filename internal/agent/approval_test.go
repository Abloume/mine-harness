package agent

import "testing"

// 审批点（human-in-the-loop）行为测试，覆盖四个核心语义：
//  1. 决策器拒绝 → 工具不执行（即使 Execute 被调用也会计数）
//  2. 决策器放行 → 工具正常执行
//  3. 未配置 approver → fail-closed 默认拒绝（安全默认）
//  4. 非高风险工具 → 不受审批机制影响（无需 approver 也正常执行）

func TestApprovalDeniedSkipsExecution(t *testing.T) {
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:             "delete_file",
		RequiresApproval: true,
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
		Name:             "delete_file",
		RequiresApproval: true,
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
	// fail-closed：工具标记 RequiresApproval 但 Agent 没配 approver → 默认拒绝，
	// 而不是"没配置就放行"。宁可误杀，不可越权。
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name:             "delete_file",
		RequiresApproval: true,
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

func TestApprovalDoesNotAffectNormalTools(t *testing.T) {
	// 非高风险工具不经过审批闸门：没有 approver 也照常执行
	executed := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Name: "get_weather",
		Execute: func(input string) (string, error) {
			executed++
			return `{"city":"北京","weather":"晴"}`, nil
		},
	})

	llm := NewMockLLM([]MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{Content: "北京晴。"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(3))
	r := a.Run("查北京天气")

	if r.Status != StatusCompleted {
		t.Fatalf("期望 completed，实际 %s（原因 %s）", r.Status, r.Reason)
	}
	if executed != 1 {
		t.Errorf("普通工具应正常执行 1 次，实际 %d", executed)
	}
}
