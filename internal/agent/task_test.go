package agent

import (
	"strings"
	"testing"
)

// 增量 H · 任务系统 + 依赖图：DAG 注册、环路防护、依赖门控、
// 完成态传播、可调度查询，以及与 Agent.Run 的打通（TraceTask 事件）。

// 场景：父任务 release 拆 3 个子任务——t1（改代码）、t2（写文档）可并行，
// t3（跑测试）依赖 t1+t2；release 在所有子任务 done 后自动完成。
func newDemoGraph(t *testing.T) *TaskGraph {
	t.Helper()
	g := NewTaskGraph()
	if err := g.Register("release", "发布 v2.0", ""); err != nil {
		t.Fatalf("注册父任务失败: %v", err)
	}
	if err := g.Register("t1", "改代码", "release"); err != nil {
		t.Fatalf("注册 t1 失败: %v", err)
	}
	if err := g.Register("t2", "写文档", "release"); err != nil {
		t.Fatalf("注册 t2 失败: %v", err)
	}
	if err := g.Register("t3", "跑测试", "release", "t1", "t2"); err != nil {
		t.Fatalf("注册 t3 失败: %v", err)
	}
	return g
}

func TestTaskGraphRegisterAndCycle(t *testing.T) {
	g := NewTaskGraph()
	if err := g.Register("a", "A", ""); err != nil {
		t.Fatalf("注册 a 失败: %v", err)
	}
	// 依赖未注册 → 拒绝
	if err := g.Register("b", "B", "", "no-such"); err == nil {
		t.Fatal("依赖不存在应拒绝")
	}
	// 父任务未注册 → 拒绝
	if err := g.Register("c", "C", "no-parent"); err == nil {
		t.Fatal("父任务不存在应拒绝")
	}
	// 依赖自身 → 被"依赖不存在"拦截（自环不可达）
	if err := g.Register("x", "X", "", "x"); err == nil {
		t.Fatal("依赖自身应拒绝")
	}
	if err := g.Register("b", "B", "", "a"); err != nil {
		t.Fatalf("注册 b 失败: %v", err)
	}
	// 重复注册 → 拒绝
	if err := g.Register("a", "A-dup", ""); err == nil {
		t.Fatal("重复注册应拒绝")
	}
	// 真正可达的环：a 已依赖 b（b 依赖 a）？——先正常无环，再动态改依赖触发。
	// 当前：a 无依赖、b 依赖 a（无环）。把 a 的依赖改为 b → a→b→a 环 → 拒绝。
	if err := g.UpdateDeps("a", "b"); err == nil {
		t.Fatal("a 依赖 b（b 依赖 a）构成环，UpdateDeps 应拒绝")
	}
	// 拒绝后状态不被写入：a 仍无依赖
	if st, _ := g.Status("a"); st != TaskPending {
		t.Fatalf("a 状态不应被破坏，实际 %s", st)
	}
	// 依赖自身（UpdateDeps）→ 拒绝
	if err := g.UpdateDeps("a", "a"); err == nil {
		t.Fatal("UpdateDeps 依赖自身应拒绝")
	}
	// 合法更新：b 的依赖改为空（解除对 a 的依赖）→ 成功
	if err := g.UpdateDeps("b"); err != nil {
		t.Fatalf("合法更新依赖失败: %v", err)
	}
}

func TestTaskGraphBlockedAndSchedulable(t *testing.T) {
	g := newDemoGraph(t)

	sched := g.Schedulable()
	if len(sched) != 2 {
		t.Fatalf("初始可调度应为 t1,t2（t3 依赖未就绪），实际 %v", sched)
	}
	has := func(id string) bool {
		for _, s := range sched {
			if s == id {
				return true
			}
		}
		return false
	}
	if !has("t1") || !has("t2") || has("t3") {
		t.Fatalf("初始可调度列表错误: %v", sched)
	}
	if !g.IsBlocked("t3") {
		t.Fatal("t3 依赖未就绪应被门控为 blocked")
	}
	if g.IsBlocked("t1") {
		t.Fatal("t1 无依赖不应 blocked")
	}
}

func TestTaskGraphPropagation(t *testing.T) {
	g := newDemoGraph(t)

	// t1 完成 → t3 仍 blocked（t2 未完成）
	g.MarkDone("t1")
	if !g.IsBlocked("t3") {
		t.Fatal("t2 未完成，t3 应仍 blocked")
	}
	if st, _ := g.Status("release"); st != TaskPending {
		t.Fatalf("子任务未全完成，release 应 pending，实际 %s", st)
	}
	// t2 完成 → t3 可调度
	g.MarkDone("t2")
	if g.IsBlocked("t3") {
		t.Fatal("t1,t2 均完成，t3 依赖已就绪")
	}
	sched := g.Schedulable()
	if len(sched) != 1 || sched[0] != "t3" {
		t.Fatalf("此时可调度应只有 t3，实际 %v", sched)
	}
	// t3 完成 → 传播：release 自动 done
	g.MarkRunning("t3")
	g.MarkDone("t3")
	st, ok := g.Status("release")
	if !ok || st != TaskDone {
		t.Fatalf("所有子任务完成，release 应自动 done，实际 %s（ok=%v）", st, ok)
	}
}

func TestTaskGraphMarkRunningIdempotent(t *testing.T) {
	g := newDemoGraph(t)
	g.MarkRunning("t1")
	g.MarkRunning("t1") // 幂等：重复领取不报错
	if st, _ := g.Status("t1"); st != TaskRunning {
		t.Fatalf("t1 应为 running，实际 %s", st)
	}
	g.MarkRunning("no-such") // 未注册：忽略
	g.MarkDone("no-such")    // 未注册：忽略
}

func TestTaskGraphAgentHooks(t *testing.T) {
	// 打通测试：Agent 带任务图跑一次 Run（completed）→ 任务自动 done +
	// TraceTask 事件进入轨迹。
	g := NewTaskGraph()
	if err := g.Register("t1", "查询天气", ""); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "get_weather",
		BaseRisk: RiskNone,
		Execute:  func(input string) (string, error) { return "晴 24 度", nil },
	})
	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Input: `{"city":"北京"}`}}},
		{Content: "北京晴 24 度"},
	})
	a := NewAgent(llm, reg, WithMaxSteps(3), WithTaskGraph(g))
	res := a.Run("t1")
	if res.Status != StatusCompleted {
		t.Fatalf("Run 应 completed，实际 %s", res.Status)
	}
	if st, _ := g.Status("t1"); st != TaskDone {
		t.Fatalf("Run 完成后任务应 done，实际 %s", st)
	}
	// 轨迹应含 task 事件：registered + running + done
	var taskEvents []TraceEvent
	for _, ev := range res.Trace {
		if ev.Type == TraceTask {
			taskEvents = append(taskEvents, ev)
		}
	}
	if len(taskEvents) != 3 {
		t.Fatalf("应有 3 条 TraceTask 事件（registered/running/done），实际 %d: %+v", len(taskEvents), taskEvents)
	}
	if taskEvents[1].Outcome != "running" || taskEvents[2].Outcome != "done" {
		t.Fatalf("TraceTask 事件顺序错误: %+v", taskEvents)
	}
}

func TestTaskGraphAgentAbortKeepsPending(t *testing.T) {
	// 中止（非 completed）不标 done：任务保持原状态，宿主可重试。
	g := NewTaskGraph()
	if err := g.Register("t1", "查询天气", ""); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "get_weather",
		BaseRisk: RiskNone,
		Execute:  func(input string) (string, error) { return "晴 24 度", nil },
	})
	// 脚本耗尽：第一步调用工具成功，第二步 mock 返回 error → LLM 调用失败 → aborted
	llm := NewMockLLM([]MockDecision{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Input: `{"city":"北京"}`}}},
	})
	a := NewAgent(llm, reg, WithMaxSteps(3), WithTaskGraph(g))
	res := a.Run("t1")
	if res.Status != StatusAborted {
		t.Fatalf("预期中止（脚本耗尽 → LLM error），实际 %s", res.Status)
	}
	st, _ := g.Status("t1")
	if st == TaskDone {
		t.Fatal("中止的任务不应标 done")
	}
	if st != TaskRunning {
		t.Fatalf("中止的任务保持领取状态（running），实际 %s", st)
	}
	// 中止场景不产生 done 轨迹事件
	for _, ev := range res.Trace {
		if ev.Type == TraceTask && ev.Outcome == "done" {
			t.Fatalf("中止不应产生 done 事件: %+v", ev)
		}
	}
	_ = strings.Builder{}
}
