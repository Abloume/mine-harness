package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 协作式取消契约（缺口 D）测试：覆盖外部实践的四层验收——
//  1. 取消不是强杀（安全检查点停止，最多多完成一个可控单元）；
//  2. 副作用分类 + 逆序补偿（可补偿步骤按执行日志逆序收尾）；
//  3. 不可逆动作执行前解决、执行后如实上报（partially_completed）；
//  4. 取消竞态卡在最尴尬时刻（请求发出后响应返回前取消；
//     外部成功后本地落账前取消——由记账闭合；补偿执行一半失败）。

// TestCancelBeforeTool 任务开始前已取消：无任何工具执行，账本为空。
func TestCancelBeforeTool(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{Name: "read_a", Execute: func(string) (string, error) { return "a", nil }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 任务开始前用户已取消

	a := NewAgent(NewMockLLM([]MockDecision{{ToolName: "read_a", ToolInput: `{}`}}), reg, WithMaxSteps(5))
	res := a.RunWithContext(ctx, "读 a")
	if res.Status != StatusCanceled {
		t.Fatalf("开始前取消应 canceled, got %s (%s)", res.Status, res.Reason)
	}
	if len(res.Ledger) != 0 {
		t.Fatalf("开始前取消不应有任何工具执行（账本应为空）, got %d 条", len(res.Ledger))
	}
}

// TestCancelMidRequest 竞态①：请求发出后响应返回前取消——
// 工具照常完成并立即记账（闭合"外部成功后本地落账前"缝隙），
// 之后检查点命中：可补偿步骤逆序收尾，状态 canceled。
func TestCancelMidRequest(t *testing.T) {
	reg := NewRegistry()
	var cancel context.CancelFunc
	reg.Register(Tool{
		Name: "reserve_stock", Description: "预占库存",
		SideEffect: SideCompensatable,
		Compensate: &Compensation{Action: "释放预占库存",
			Exec: func(input string) (string, error) { return "库存已释放", nil }},
		Execute: func(input string) (string, error) { return "库存预占成功", nil },
	})
	reg.Register(Tool{
		Name: "read_order", Description: "查询订单",
		SideEffect: SideReadonly,
		Execute: func(input string) (string, error) {
			cancel() // 竞态：响应返回前用户点了取消（信号在此置位）
			return "订单信息…", nil
		},
	})
	ctx, c := context.WithCancel(context.Background())
	cancel = c

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "c1", Name: "reserve_stock", Input: `{"sku":"A"}`},
			{ID: "c2", Name: "read_order", Input: `{"id":"1"}`},
		}},
	})
	a := NewAgent(llm, reg, WithMaxSteps(5))
	res := a.RunWithContext(ctx, "预占库存并查询订单")

	if res.Status != StatusCanceled {
		t.Fatalf("应 canceled（无可逆残留）, got %s (%s)", res.Status, res.Reason)
	}
	// 两个工具都执行并记账（最多多完成一个可控单元：read_order 是最后单元）
	if len(res.Ledger) != 2 {
		t.Fatalf("账本应有 2 条（reserve + read）, got %d", len(res.Ledger))
	}
	// reserve_stock 可补偿已补偿
	if res.Ledger[0].SideEffect != SideCompensatable || !res.Ledger[0].Compensated {
		t.Fatalf("reserve_stock 应可补偿且已补偿, got %+v", res.Ledger[0])
	}
	if len(res.Compensations) != 1 || !res.Compensations[0].OK {
		t.Fatalf("应有 1 条成功补偿记录, got %+v", res.Compensations)
	}
}

// TestCompensateReverse 逆序补偿：步骤1 占库存、步骤2 建草稿，
// 取消后应先撤销草稿、再释放库存（撤销栈，后发生先撤）。
func TestCompensateReverse(t *testing.T) {
	reg := NewRegistry()
	var cancel context.CancelFunc
	compOrder := []string{}
	reg.Register(Tool{
		Name: "reserve_stock", SideEffect: SideCompensatable,
		Compensate: &Compensation{Action: "释放库存", Exec: func(string) (string, error) {
			compOrder = append(compOrder, "release_stock")
			return "ok", nil
		}},
		Execute: func(string) (string, error) { return "ok", nil },
	})
	reg.Register(Tool{
		Name: "create_draft", SideEffect: SideCompensatable,
		Compensate: &Compensation{Action: "删除草稿", Exec: func(string) (string, error) {
			compOrder = append(compOrder, "delete_draft")
			return "ok", nil
		}},
		Execute: func(string) (string, error) { return "ok", nil },
	})
	reg.Register(Tool{
		Name: "read_order", SideEffect: SideReadonly,
		Execute: func(string) (string, error) { cancel(); return "订单", nil },
	})
	ctx, c := context.WithCancel(context.Background())
	cancel = c

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "c1", Name: "reserve_stock", Input: `{}`},
			{ID: "c2", Name: "create_draft", Input: `{}`},
		}},
		{ToolCalls: []ToolCall{{ID: "c3", Name: "read_order", Input: `{}`}}},
	})
	a := NewAgent(llm, reg, WithMaxSteps(5))
	res := a.RunWithContext(ctx, "占库存→建草稿→查订单")

	if res.Status != StatusCanceled {
		t.Fatalf("应 canceled, got %s", res.Status)
	}
	// 逆序：先 delete_draft（后发生先撤），再 release_stock
	if len(compOrder) != 2 || compOrder[0] != "delete_draft" || compOrder[1] != "release_stock" {
		t.Fatalf("补偿应逆序 [delete_draft release_stock], got %v", compOrder)
	}
}

// TestIrreversiblePartial 不可逆步骤已执行并提交：后续停止，
// 状态如实为 partially_completed（不把"进程停了"冒充"退款已撤销"）。
func TestIrreversiblePartial(t *testing.T) {
	reg := NewRegistry()
	var cancel context.CancelFunc
	reg.Register(Tool{
		Name: "create_refund", Description: "创建退款单",
		SideEffect: SideIrreversible,
		Execute: func(input string) (string, error) {
			cancel() // 竞态②：外部成功后（退款单已创建）本地落账前用户取消
			return "退款单 REF-001 已创建", nil
		},
	})
	ctx, c := context.WithCancel(context.Background())
	cancel = c

	llm := NewMockLLM([]MockDecision{
		{ToolName: "create_refund", ToolInput: `{"order":"1"}`},
	})
	a := NewAgent(llm, reg, WithMaxSteps(5))
	res := a.RunWithContext(ctx, "创建退款")

	if res.Status != StatusPartial {
		t.Fatalf("不可逆已提交应 partially_completed, got %s (%s)", res.Status, res.Reason)
	}
	if len(res.Ledger) != 1 || res.Ledger[0].SideEffect != SideIrreversible || !res.Ledger[0].OK {
		t.Fatalf("账本应记录不可逆步骤已执行, got %+v", res.Ledger)
	}
	if len(res.Compensations) != 0 {
		t.Fatalf("不可逆步骤不应尝试补偿（不假装回滚）, got %+v", res.Compensations)
	}
}

// TestIrreversibleNotExecutedAfterCancel 取消已到达时，不可逆动作
// 执行前被二次检查拦截：不会发出新的外部副作用。
func TestIrreversibleNotExecutedAfterCancel(t *testing.T) {
	reg := NewRegistry()
	var cancel context.CancelFunc
	executed := false
	reg.Register(Tool{
		Name: "send_sms", Description: "发送短信", SideEffect: SideIrreversible,
		Execute: func(string) (string, error) { executed = true; return "已发送", nil },
	})
	// 取消发生在执行前（通过只读工具置位，与不可逆在同一轮、顺序在前）
	reg.Register(Tool{
		Name: "read_x", SideEffect: SideReadonly,
		Execute: func(string) (string, error) { cancel(); return "x", nil },
	})
	ctx, c := context.WithCancel(context.Background())
	cancel = c

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "c1", Name: "read_x", Input: `{}`},
			{ID: "c2", Name: "send_sms", Input: `{}`},
		}},
	})
	a := NewAgent(llm, reg, WithMaxSteps(5))
	res := a.RunWithContext(ctx, "读 x 后发短信")

	if executed {
		t.Fatal("取消已到达时不可逆动作不应执行（二次检查拦截）")
	}
	if res.Status != StatusCanceled {
		t.Fatalf("无不可逆执行应 canceled, got %s", res.Status)
	}
	// 账本只记 read_x（send_sms 未执行不入账）
	if len(res.Ledger) != 1 || res.Ledger[0].Name != "read_x" {
		t.Fatalf("账本应只有 read_x, got %+v", res.Ledger)
	}
}

// TestCompensateFailNeedsReview 补偿执行一半失败：如实记录尝试次数，
// 状态 needs_review（交还人工，不假装收敛）。
func TestCompensateFailNeedsReview(t *testing.T) {
	reg := NewRegistry()
	var cancel context.CancelFunc
	reg.Register(Tool{
		Name: "reserve_stock", SideEffect: SideCompensatable,
		Compensate: &Compensation{Action: "释放库存", Exec: func(string) (string, error) {
			return "", errors.New("外部系统超时，释放失败")
		}},
		Execute: func(string) (string, error) { return "ok", nil },
	})
	reg.Register(Tool{
		Name: "read_order", SideEffect: SideReadonly,
		Execute: func(string) (string, error) { cancel(); return "订单", nil },
	})
	ctx, c := context.WithCancel(context.Background())
	cancel = c

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "c1", Name: "reserve_stock", Input: `{}`},
			{ID: "c2", Name: "read_order", Input: `{}`},
		}},
	})
	a := NewAgent(llm, reg, WithMaxSteps(5))
	res := a.RunWithContext(ctx, "占库存")

	if res.Status != StatusNeedsReview {
		t.Fatalf("补偿失败应 needs_review, got %s (%s)", res.Status, res.Reason)
	}
	if len(res.Compensations) != 1 || res.Compensations[0].OK {
		t.Fatalf("应有 1 条失败补偿记录, got %+v", res.Compensations)
	}
	if !strings.Contains(res.Ledger[0].CompensateErr, "已尝试 1 次") {
		t.Fatalf("补偿错误应记录尝试次数, got %q", res.Ledger[0].CompensateErr)
	}
}

// TestCompensatableWithoutAction 声明可补偿却没给补偿动作：
// 无法自动收尾 → needs_review（如实告知，不假装回滚）。
func TestCompensatableWithoutAction(t *testing.T) {
	reg := NewRegistry()
	var cancel context.CancelFunc
	reg.Register(Tool{
		Name: "lock_res", SideEffect: SideCompensatable, // 故意不声明 Compensate
		Execute: func(string) (string, error) { return "ok", nil },
	})
	reg.Register(Tool{
		Name: "read_order", SideEffect: SideReadonly,
		Execute: func(string) (string, error) { cancel(); return "订单", nil },
	})
	ctx, c := context.WithCancel(context.Background())
	cancel = c

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "c1", Name: "lock_res", Input: `{}`},
			{ID: "c2", Name: "read_order", Input: `{}`},
		}},
	})
	a := NewAgent(llm, reg, WithMaxSteps(5))
	res := a.RunWithContext(ctx, "锁定资源")

	if res.Status != StatusNeedsReview {
		t.Fatalf("未声明补偿动作应 needs_review, got %s", res.Status)
	}
	if !strings.Contains(res.Ledger[0].CompensateErr, "未声明补偿动作") {
		t.Fatalf("应记录未声明补偿动作, got %q", res.Ledger[0].CompensateErr)
	}
}

// TestNoCancelNormalComplete 无取消信号：正常完成，账本照常记录
// （账本不是取消专属——第 8 步轨迹导出的数据源）。
func TestNoCancelNormalComplete(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{Name: "read_a", Execute: func(string) (string, error) { return "a", nil }})
	llm := NewMockLLM([]MockDecision{
		{ToolName: "read_a", ToolInput: `{}`},
		{Content: "完成。"},
	})
	a := NewAgent(llm, reg, WithMaxSteps(5))
	res := a.RunWithContext(context.Background(), "读 a")

	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s (%s)", res.Status, res.Reason)
	}
	if len(res.Ledger) != 1 {
		t.Fatalf("账本应记录 1 次调用, got %d", len(res.Ledger))
	}
	if res.Ledger[0].InputHash == "" || !strings.HasPrefix(res.Ledger[0].At, "20") {
		t.Fatalf("账本应含请求标识与完成时间, got %+v", res.Ledger[0])
	}
}

// TestLedgerExposedOnAbort 护栏中止（非取消）也暴露账本：停止≠外部无记录。
func TestLedgerExposedOnAbort(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{Name: "read_a", Execute: func(string) (string, error) { return "a", nil }})
	// 永远调工具不收敛：步数耗尽 → StatusAborted
	steps := make([]MockDecision, 0, 3)
	for i := 0; i < 3; i++ {
		steps = append(steps, MockDecision{ToolName: "read_a", ToolInput: `{"i":"` + string(rune('0'+i)) + `"}`})
	}
	a := NewAgent(NewMockLLM(steps), reg, WithMaxSteps(3))
	res := a.Run("循环")
	if res.Status != StatusAborted {
		t.Fatalf("应 aborted, got %s", res.Status)
	}
	if len(res.Ledger) == 0 {
		t.Fatal("护栏中止也应暴露账本（已发生动作有记录）")
	}
}
