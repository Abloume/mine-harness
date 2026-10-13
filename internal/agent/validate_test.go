package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 缺口 A · 工具返回结果 schema 校验：工具声明 ResultValidator，
// 执行成功后、结果回填上下文之前校验；失败走明确错误 + 重试预算。

// jsonWithOKValidator 是测试用校验器：要求输出是合法 JSON 且含 ok=true。
// 对应生产场景"工具返回结构化结果，模型必须拿到有效数据而非半截/乱码"。
func jsonWithOKValidator(output string) error {
	var m map[string]any
	if err := json.Unmarshal([]byte(output), &m); err != nil {
		return fmt.Errorf("返回不是合法 JSON: %v", err)
	}
	if v, _ := m["ok"].(bool); !v {
		return fmt.Errorf("缺少 ok=true 字段")
	}
	return nil
}

// TestResultValidationPasses 合法输出放行：tool 消息回填原文，任务正常完成。
func TestResultValidationPasses(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name: "lookup",
		Execute: func(input string) (string, error) {
			return `{"ok":true,"data":"x"}`, nil
		},
		ResultValidator: jsonWithOKValidator,
	})
	llm := NewMockLLM([]MockDecision{
		{ToolName: "lookup", ToolInput: `{"id":1}`},
		{Content: "查到了 x。"},
	})
	a := NewAgent(llm, reg, WithMaxSteps(4))
	res := a.Run("查一下")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s: %s", res.Status, res.Reason)
	}
	found := false
	for _, m := range a.History() {
		if m.Role == roleTool && strings.Contains(m.Content, `"data":"x"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("合法结果应原样回填上下文")
	}
}

// TestResultValidationRejectsBadOutput 校验失败：原文**不进上下文**，
// 回填明确错误（模型不会"以为成功了"）；模型换路径后任务仍可完成。
func TestResultValidationRejectsBadOutput(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name: "lookup",
		Execute: func(input string) (string, error) {
			return "not-json-garbage", nil // 工具执行成功但输出不符合 schema
		},
		ResultValidator: jsonWithOKValidator,
	})
	// 模型换参数再试一次，仍失败后给出最终回答（不再空转）
	llm := NewMockLLM([]MockDecision{
		{ToolName: "lookup", ToolInput: `{"id":1}`},
		{ToolName: "lookup", ToolInput: `{"id":2}`},
		{Content: "查询接口返回异常，建议人工检查。"},
	})
	a := NewAgent(llm, reg, WithMaxSteps(6))
	res := a.Run("查一下")
	if res.Status != StatusCompleted {
		t.Fatalf("换参数后应可完成, got %s: %s", res.Status, res.Reason)
	}
	var toolMsgs []string
	for _, m := range a.History() {
		if m.Role == roleTool {
			toolMsgs = append(toolMsgs, m.Content)
		}
	}
	for _, c := range toolMsgs {
		if strings.Contains(c, "not-json-garbage") {
			t.Fatal("校验失败的结果原文不应进入上下文")
		}
		if !strings.Contains(c, "未通过校验") {
			t.Fatalf("应回填明确校验错误, got: %s", c)
		}
	}
}

// TestResultValidationBudgetAborts 同参连续校验失败：循环检测（精确重复）
// 先于重试预算兜底中止——校验失败与执行失败共用"失败即重试"语义，
// 但同参重复永远先被循环检测拦截（两者分工：循环检测拦精确重复、
// 重试预算拦"换了目标仍连续失败"，见 loop.go 与 README）。
func TestResultValidationBudgetAborts(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name: "lookup",
		Execute: func(input string) (string, error) {
			return `{"ok":false}`, nil // 每次都是"假成功"输出
		},
		ResultValidator: jsonWithOKValidator,
	})
	steps := make([]MockDecision, 0, 6)
	for i := 0; i < 6; i++ {
		steps = append(steps, MockDecision{ToolName: "lookup", ToolInput: `{"id":1}`})
	}
	a := NewAgent(NewMockLLM(steps), reg, WithMaxSteps(10))
	res := a.Run("查一下")
	if res.Status != StatusAborted {
		t.Fatalf("同参反复失败应软停止, got %s", res.Status)
	}
	// 精确重复路径：中止原因指向循环检测（校验失败触发的重试预算
	// 在换参场景才起作用，同参场景循环检测更快）
	if !strings.Contains(res.Reason, "循环检测") {
		t.Fatalf("同参重复应由循环检测中止, got: %s", res.Reason)
	}
}

// TestValidateResultUnknownTool 未注册工具校验直接放行（反正 Call 已拦截，
// 不会走到这里；返回 nil 保持语义一致）。
func TestValidateResultUnknownTool(t *testing.T) {
	reg := NewRegistry()
	if err := reg.ValidateResult("ghost", "anything"); err != nil {
		t.Fatalf("未知工具应放行, got: %v", err)
	}
}
