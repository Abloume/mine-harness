package agent

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// ---- 多 tool_call 并发执行 ----

// mkConcurrentTracker 构造一个带并发度统计的工具工厂：
// inflight 当前并发数、max 历史峰值（atomic，工具本身是并发安全声明方）。
func mkConcurrentTracker(inflight, max *atomic.Int32) func(name string, sleep time.Duration) Tool {
	return func(name string, sleep time.Duration) Tool {
		return Tool{
			Name:           name,
			BaseRisk:       RiskNone,
			ConcurrentSafe: true,
			Execute: func(input string) (string, error) {
				cur := inflight.Add(1)
				for {
					m := max.Load()
					if cur <= m || max.CompareAndSwap(m, cur) {
						break
					}
				}
				time.Sleep(sleep)
				inflight.Add(-1)
				return name + " done", nil
			},
		}
	}
}

// TestParallelConcurrentSafeToolsRunConcurrently 验证核心行为：
// 一轮内两个声明并发安全的工具被 goroutine 并行执行（峰值并发 ≥ 2），
// 且两条 tool 结果都按协议闭合回填。
func TestParallelConcurrentSafeToolsRunConcurrently(t *testing.T) {
	var inflight, max atomic.Int32
	mk := mkConcurrentTracker(&inflight, &max)

	reg := NewRegistry()
	reg.Register(mk("tool_a", 100*time.Millisecond))
	reg.Register(mk("tool_b", 100*time.Millisecond))

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "a", Name: "tool_a", Input: `{}`},
			{ID: "b", Name: "tool_b", Input: `{}`},
		}},
		{Content: "两个都完成了"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(4))
	res := a.Run("并行执行")
	if res.Status != StatusCompleted {
		t.Fatalf("应 completed，实际 %s（%s）", res.Status, res.Reason)
	}
	if max.Load() < 2 {
		t.Errorf("并发安全工具应并行执行（峰值并发 ≥ 2），实际峰值 %d", max.Load())
	}

	// 两条结果都回填且关联正确 id
	toolMsgs := map[string]bool{}
	for _, m := range a.history {
		if m.Role == roleTool {
			toolMsgs[m.ToolCallID] = true
		}
	}
	if !toolMsgs["a"] || !toolMsgs["b"] {
		t.Errorf("两条 tool 结果都应回填，实际 %v", toolMsgs)
	}
}

// TestParallelBackfillsInOriginalOrder 验证：并行执行完成，但回填顺序保持
// 原始 tool_calls 顺序（慢工具在前也按原序落历史，便于审计/复现）。
func TestParallelBackfillsInOriginalOrder(t *testing.T) {
	var inflight, max atomic.Int32
	mk := mkConcurrentTracker(&inflight, &max)

	reg := NewRegistry()
	reg.Register(mk("slow", 200*time.Millisecond)) // 慢工具排在前
	reg.Register(mk("fast", 10*time.Millisecond))

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "s", Name: "slow", Input: `{}`},
			{ID: "f", Name: "fast", Input: `{}`},
		}},
		{Content: "完成"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(4))
	if res := a.Run("执行"); res.Status != StatusCompleted {
		t.Fatalf("应 completed，实际 %s（%s）", res.Status, res.Reason)
	}

	var order []string
	for _, m := range a.history {
		if m.Role == roleTool {
			order = append(order, m.ToolCallID)
		}
	}
	if len(order) != 2 || order[0] != "s" || order[1] != "f" {
		t.Errorf("回填应保持原始顺序 [s f]，实际 %v", order)
	}
}

// TestParallelNonSafeFallsBackToSerial 验证：混入未声明并发安全的工具时，
// 整批回退顺序执行（峰值并发 = 1），正确性优先于并行收益。
func TestParallelNonSafeFallsBackToSerial(t *testing.T) {
	var inflight, max atomic.Int32
	mk := mkConcurrentTracker(&inflight, &max)

	reg := NewRegistry()
	reg.Register(mk("safe_tool", 50*time.Millisecond))
	reg.Register(Tool{ // 未声明并发安全
		Name:     "legacy_tool",
		BaseRisk: RiskNone,
		Execute: func(input string) (string, error) {
			cur := inflight.Add(1)
			for {
				m := max.Load()
				if cur <= m || max.CompareAndSwap(m, cur) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			inflight.Add(-1)
			return "legacy done", nil
		},
	})

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "s1", Name: "safe_tool", Input: `{}`},
			{ID: "l1", Name: "legacy_tool", Input: `{}`},
		}},
		{Content: "完成"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(4))
	if res := a.Run("执行"); res.Status != StatusCompleted {
		t.Fatalf("应 completed，实际 %s（%s）", res.Status, res.Reason)
	}
	if max.Load() > 1 {
		t.Errorf("含非并发安全工具时应顺序执行（峰值并发 ≤ 1），实际 %d", max.Load())
	}
}

// TestParallelOneFailsOthersProceed 验证：并发中一个工具失败，回填错误消息
// 让模型自己决定，另一个成功不受影响（"一个失败不影响其他"）。
func TestParallelOneFailsOthersProceed(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name:           "good",
		BaseRisk:       RiskNone,
		ConcurrentSafe: true,
		Execute: func(input string) (string, error) {
			return "good result", nil
		},
	})
	reg.Register(Tool{
		Name:           "bad",
		BaseRisk:       RiskNone,
		ConcurrentSafe: true,
		Execute: func(input string) (string, error) {
			return "", errorsNew("boom")
		},
	})

	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "g1", Name: "good", Input: `{}`},
			{ID: "b1", Name: "bad", Input: `{}`},
		}},
		{Content: "good 成功，bad 失败"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(4))
	res := a.Run("执行")
	if res.Status != StatusCompleted {
		t.Fatalf("单次失败不应 abort loop，实际 %s（%s）", res.Status, res.Reason)
	}

	var goodSeen, failSeen bool
	for _, m := range a.history {
		if m.Role == roleTool && m.ToolCallID == "g1" && strings.Contains(m.Content, "good result") {
			goodSeen = true
		}
		if m.Role == roleTool && m.ToolCallID == "b1" && strings.Contains(m.Content, "工具调用失败") {
			failSeen = true
		}
	}
	if !goodSeen || !failSeen {
		t.Errorf("成功与失败都应回填，good=%v fail=%v", goodSeen, failSeen)
	}
}

// TestParallelSpecialChannelStaysSerial 验证：load_skill 特殊通道与普通工具
// 同批时整体顺序执行（特殊通道写内核 map，不参与并发），且都正常处理。
func TestParallelSpecialChannelStaysSerial(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewLoadSkillTool())
	reg.Register(Tool{
		Name:           "get_weather",
		BaseRisk:       RiskNone,
		ConcurrentSafe: true,
		Execute: func(input string) (string, error) {
			return "晴", nil
		},
	})

	sp := &stubSkillProvider{skills: map[string]string{"policy": "# 规范\n正文"}}
	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{
			{ID: "l1", Name: LoadSkillToolName, Input: `{"name":"policy"}`},
			{ID: "w1", Name: "get_weather", Input: `{"city":"北京"}`},
		}},
		{Content: "完成"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(4), WithSkillProvider(sp))
	res := a.Run("执行")
	if res.Status != StatusCompleted {
		t.Fatalf("应 completed，实际 %s（%s）", res.Status, res.Reason)
	}

	var sysInjected, weatherDone bool
	for _, m := range a.history {
		if m.Role == roleSystem && strings.Contains(m.Content, "已加载技能规范 policy") {
			sysInjected = true
		}
		if m.Role == roleTool && m.ToolCallID == "w1" && strings.Contains(m.Content, "晴") {
			weatherDone = true
		}
	}
	if !sysInjected || !weatherDone {
		t.Errorf("特殊通道与普通工具都应处理，skill=%v weather=%v", sysInjected, weatherDone)
	}
}

// errorsNew 是测试内的 error 快捷构造（避免顶层污染）。
func errorsNew(s string) error { return &testError{s} }

type testError struct{ s string }

func (e *testError) Error() string { return e.s }
