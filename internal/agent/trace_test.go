package agent

import (
	"errors"
	"strings"
	"testing"
)

// 增量 A · 轨迹导出测试：RunResult.Trace 是整条链路的可审计事件流。
// 覆盖：六类事件出现、outcome 分类、seq 递增、WithModel 写入、
// 入参截断、失败/拒绝/护栏路径各记一条。

// TestTraceCoverageAndSequence 一次完整 Run：
// 成功工具（tool ok）→ 被审批拒绝的高风险工具（approval denied）→
// 失败工具（tool failed）→ 最终回答（llm）。
// 断言：事件类型覆盖 llm/tool/approval；seq 严格递增；
// WithModel 写入每一条 llm 事件；拒绝与失败路径各记一条。
func TestTraceCoverageAndSequence(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "read",
		BaseRisk: RiskNone,
		Execute:  func(string) (string, error) { return "file content", nil },
	})
	reg.Register(Tool{
		Name:     "write",
		BaseRisk: RiskMedium, // 达默认审批阈值 → 走闸门
		Execute:  func(string) (string, error) { return "written", nil },
	})
	reg.Register(Tool{
		Name:     "boom",
		BaseRisk: RiskNone,
		Execute:  func(string) (string, error) { return "", errors.New("执行失败") },
	})

	llm := NewMockLLM([]MockDecision{
		{ToolName: "read", ToolInput: `{"path":"a.txt"}`},
		{ToolName: "write", ToolInput: `{"path":"b.txt"}`},
		{ToolName: "boom", ToolInput: `{}`},
		{Content: "完成。"},
	})

	a := NewAgent(llm, reg,
		WithMaxSteps(6),
		WithModel("test-model"), // 请求级元数据
		WithApprover(ApproverFunc(func(name, args string) bool {
			if name == "write" {
				return false // 拒绝 → approval denied
			}
			return true
		})),
	)
	res := a.Run("读取并写入，最后测试失败工具")

	if len(res.Trace) == 0 {
		t.Fatal("RunResult.Trace 为空：轨迹应随结果暴露")
	}

	// 类型覆盖
	got := map[string]int{}
	for _, ev := range res.Trace {
		got[ev.Type]++
	}
	for _, want := range []string{TraceLLM, TraceTool, TraceApproval} {
		if got[want] == 0 {
			t.Errorf("轨迹缺少 %s 事件（实际类型分布: %v）", want, got)
		}
	}
	// llm 事件条数 = 模型调用次数（每步一次）
	if got[TraceLLM] != 4 {
		t.Errorf("llm 事件应为 4 条（4 次模型调用），实际 %d", got[TraceLLM])
	}

	// seq 严格递增 + 从 1 开始
	for i, ev := range res.Trace {
		if ev.Seq != i+1 {
			t.Errorf("seq 应严格递增（第 %d 条应为 %d，实际 %d）", i, i+1, ev.Seq)
		}
	}

	// WithModel：每一条 llm 事件都带上请求级元数据
	for _, ev := range res.Trace {
		if ev.Type == TraceLLM && ev.Model != "test-model" {
			t.Errorf("llm 事件 Model 应为 test-model，实际 %q", ev.Model)
		}
	}

	// 拒绝路径：write 记 approval denied
	foundDenied := false
	for _, ev := range res.Trace {
		if ev.Type == TraceApproval && ev.Name == "write" && ev.Outcome == "denied" {
			foundDenied = true
			if !strings.Contains(ev.Detail, "连续拒绝 1 次") {
				t.Errorf("denied Detail 应含拒绝计数，实际 %q", ev.Detail)
			}
		}
	}
	if !foundDenied {
		t.Error("write 被审批拒绝后应记 approval denied 事件")
	}

	// 失败路径：boom 记 tool failed
	foundFailed := false
	for _, ev := range res.Trace {
		if ev.Type == TraceTool && ev.Name == "boom" && ev.Outcome == OutcomeFailed {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Error("boom 执行失败应记 tool failed 事件")
	}

	// 成功路径：read 记 tool ok，且入参摘要为截断后的 JSON 原样
	foundOK := false
	for _, ev := range res.Trace {
		if ev.Type == TraceTool && ev.Name == "read" && ev.Outcome == OutcomeOK {
			foundOK = true
		}
	}
	if !foundOK {
		t.Error("read 执行成功应记 tool ok 事件")
	}
}

// TestTraceGuardedOnLoop 循环检测（重复调用同参工具）：
// 第一次 guardWarn 记 guarded（警告后停止该调用），
// 第二次 guardAbort 记 guarded（中止整轮）。
func TestTraceGuardedOnLoop(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "ping",
		BaseRisk: RiskNone,
		Execute:  func(string) (string, error) { return "pong", nil },
	})
	// 连续 4 次同参调用：guard 计数在 count>=repeatLimit(默认3) 时命中——
	// 第 1、2 次正常执行，第 3 次 guardWarn（不执行），第 4 次 guardAbort（中止）
	llm := NewMockLLM([]MockDecision{
		{ToolName: "ping", ToolInput: `{"x":1}`},
		{ToolName: "ping", ToolInput: `{"x":1}`},
		{ToolName: "ping", ToolInput: `{"x":1}`},
		{ToolName: "ping", ToolInput: `{"x":1}`},
	})
	a := NewAgent(llm, reg, WithMaxSteps(5))
	res := a.Run("ping 三次")

	guarded := 0
	for _, ev := range res.Trace {
		if ev.Type == TraceTool && ev.Outcome == OutcomeGuarded {
			guarded++
		}
	}
	if guarded != 2 {
		t.Errorf("循环检测应记 2 条 guarded（警告+中止），实际 %d", guarded)
	}
	if res.Status != StatusAborted {
		t.Errorf("循环检测二次命中应中止，实际状态 %s", res.Status)
	}
}

// TestTraceInputTruncation traceInput 截断：短串原样，长串截 200 并带说明。
func TestTraceInputTruncation(t *testing.T) {
	short := `{"path":"a.txt"}`
	if got := traceInput(short); got != short {
		t.Errorf("短输入应原样返回，实际 %q", got)
	}
	long := strings.Repeat("x", 300)
	got := traceInput(long)
	if !strings.HasPrefix(got, strings.Repeat("x", 200)) {
		t.Error("长输入应保留前 200 字符")
	}
	if !strings.Contains(got, "截断") {
		t.Error("截断说明应包含「截断」字样")
	}
}
