package agent

import (
	"strings"
	"testing"
)

// 多 tool_call（并行工具调用）端到端测试：
// 模型一轮返回多个 tool_calls → harness 逐个过护栏/审批/执行 → 历史按协议闭合
//（1 条 assistant 带全部 tool_calls + N 条 tool 结果消息）。

func TestLoopExecutesMultipleToolCalls(t *testing.T) {
	executed := map[string]int{}
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "get_weather",
		BaseRisk: RiskNone,
		Execute: func(input string) (string, error) {
			executed["get_weather"]++
			return "晴 24 度", nil
		},
	})
	reg.Register(Tool{
		Name:     "get_time",
		BaseRisk: RiskNone,
		Execute: func(input string) (string, error) {
			executed["get_time"]++
			return "20:30", nil
		},
	})

	// mock 一轮并行调用两个工具，下一轮给最终回答
	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "c1", Name: "get_weather", Input: `{"city":"北京"}`},
			{ID: "c2", Name: "get_time", Input: `{"zone":"Asia/Shanghai"}`},
		}},
		{Content: "北京晴 24 度，现在是 20:30"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(4))
	res := a.Run("查北京天气和当前时间")

	if res.Status != StatusCompleted {
		t.Fatalf("应 completed，实际 %s（%s）", res.Status, res.Reason)
	}
	if executed["get_weather"] != 1 || executed["get_time"] != 1 {
		t.Errorf("两个工具应各执行 1 次，实际 %v", executed)
	}

	// 历史协议检查：1 条 assistant（2 个 tool_calls）+ 2 条 tool（各自关联 id）
	var assistant *Message
	toolIDs := map[string]bool{}
	for i := range a.history {
		m := a.history[i]
		if m.Role == roleAssistant && len(m.ToolCalls) > 0 {
			if assistant != nil {
				t.Fatal("应只有 1 条带 tool_calls 的 assistant 消息")
			}
			assistant = &a.history[i]
		}
		if m.Role == roleTool {
			toolIDs[m.ToolCallID] = true
		}
	}
	if assistant == nil {
		t.Fatal("历史中没有带 tool_calls 的 assistant 消息")
	}
	if len(assistant.ToolCalls) != 2 {
		t.Errorf("assistant 应带 2 个 tool_calls，实际 %d", len(assistant.ToolCalls))
	}
	if !toolIDs["c1"] || !toolIDs["c2"] {
		t.Errorf("tool 结果消息应关联 c1/c2，实际 %v", toolIDs)
	}
}

func TestLoopMultipleToolCallsOneRejected(t *testing.T) {
	// 并行调用中"一个被审批拒绝、另一个放行"：
	// delete 判 HIGH → 拒绝（不执行）；read 判 LOW → 放行执行。
	// 两者都在同一轮，拒绝不阻断其他调用（各 tool_call 独立过闸门）。
	executed := map[string]int{}
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "file_op",
		BaseRisk: RiskLow,
		Execute: func(input string) (string, error) {
			executed["file_op"]++
			if strings.Contains(input, "delete") {
				return "deleted", nil
			}
			return "content: hi", nil
		},
	})

	// 参数感知风险：delete → HIGH，read → LOW（对应 FuncRiskEvaluator 用法）
	risk := FuncRiskEvaluator(func(_ string, args string, base RiskLevel) RiskLevel {
		if strings.Contains(args, "delete") {
			return RiskHigh
		}
		return base
	})

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "d1", Name: "file_op", Input: `{"action":"delete","file":"/tmp/a"}`},
			{ID: "r1", Name: "file_op", Input: `{"action":"read","file":"/tmp/a"}`},
		}},
		{Content: "删除未授权，读取成功"},
	})

	a := NewAgent(llm, reg,
		WithMaxSteps(4),
		WithRiskEvaluator(risk),
		WithApprover(DenyApprover{}),
	)
	res := a.Run("先删除再读取 /tmp/a")

	if res.Status != StatusCompleted {
		t.Fatalf("应 completed，实际 %s（%s）", res.Status, res.Reason)
	}
	if executed["file_op"] != 1 {
		t.Errorf("应只执行 1 次（read 放行、delete 拒绝），实际 %d", executed["file_op"])
	}

	// 历史里应有：1 条拒绝 tool 消息（关联 d1）+ 1 条结果 tool 消息（关联 r1）
	rejected, executedCount := 0, 0
	for _, m := range a.history {
		if m.Role == roleTool && strings.Contains(m.Content, "调用被拒绝") {
			if m.ToolCallID != "d1" {
				t.Errorf("拒绝消息应关联 d1，实际 %s", m.ToolCallID)
			}
			rejected++
		}
		if m.Role == roleTool && m.ToolCallID == "r1" {
			executedCount++
		}
	}
	if rejected != 1 || executedCount != 1 {
		t.Errorf("应有 1 条拒绝 + 1 条执行结果，实际拒绝=%d 执行=%d", rejected, executedCount)
	}
}
