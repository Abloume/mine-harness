package agent

import (
	"strings"
	"testing"
)

// 本文件是 token 统计与压缩行为的自查测试。
//
// JS/TS ↔ Go 差异：测试文件约定以 _test.go 结尾、与被测代码同包；
// 用 go test 运行，函数名必须是 TestXxx(t *testing.T)。
// TS 侧对应 jest/vitest 的 describe+it，Go 用"表驱动测试"（cases 数组循环断言）。

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"空串", "", 0},
		{"纯中文 6 字", "今天天气很好", 6},                                   // CJK 1 字 ≈ 1 token
		{"纯英文 hello", "hello", 1},                                  // 5 字符 / 4 = 1
		{"纯英文 hello world", "hello world", 2},                      // 11 字符 / 4 = 2（空格也计入 rest）
		{"混合中英", "北京 Beijing 2026", 5},                             // 2 CJK + 12/4 = 5
		{"工具结果 JSON", `{"city":"北京","weather":"晴","temp":24}`, 11}, // 3 CJK + 32/4 = 11
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EstimateTokens(c.in); got != c.want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestMessagesTokens(t *testing.T) {
	msgs := []Message{
		{Role: roleUser, Content: "今天天气很好"}, // 6
		{Role: roleTool, Content: "hello"},  // 1
	}
	if got := MessagesTokens(msgs); got != 7 {
		t.Errorf("MessagesTokens = %d, want 7", got)
	}
}

// TestCompactContextDropsTokens 验证压缩后 token 显著下降，且 system 首条永不被压。
func TestCompactContextDropsTokens(t *testing.T) {
	// 构造：system + 6 条长工具结果（每条都超预算）
	msgs := []Message{{Role: roleSystem, Content: "system 指令"}}
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			Message{Role: roleAssistant, Content: "调用工具 get_weather"},
			Message{Role: roleTool, Content: `{"city":"北京","weather":"晴转多云","temp":22,"summary":"今天白天晴转多云，午后体感舒适，适合户外活动，夜间最低温18度，注意添衣。"}`},
		)
	}

	before := MessagesTokens(msgs)
	after, compacted := CompactContext(msgs, 150, NewHeuristicSummarizer())

	if !compacted {
		t.Fatalf("预算 150 应触发压缩, compacted=false, before=%d", before)
	}
	if MessagesTokens(after) >= before {
		t.Errorf("压缩后 token 未下降: before=%d after=%d", before, MessagesTokens(after))
	}
	if len(after) == 0 || after[0].Role != roleSystem {
		t.Errorf("第 0 条 system 必须保留, got %+v", after[0])
	}
	// 压缩后应保留一条摘要消息（角色 system，带【历史摘要】前缀）
	hasSummary := false
	for _, m := range after {
		if m.Role == roleSystem && len(m.Content) > 10 && containsSummary(m.Content) {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Errorf("压缩后应存在摘要消息, got %+v", after)
	}
}

func containsSummary(s string) bool {
	return len(s) > 12 // 摘要消息明显长于普通标记
}

// ---- 生成式 LLM 摘要器 ----

// recordingLLM 是记录型 LLM stub：记录每次收到的 messages/tools，
// 按配置返回固定输出或错误（用于验证"喂给模型的输入长什么样"）。
type recordingLLM struct {
	got     [][]Message
	gotTool [][]Tool
	out     string
	err     error
}

func (r *recordingLLM) Chat(messages []Message, tools []Tool) (LLMResponse, error) {
	r.got = append(r.got, append([]Message(nil), messages...))
	r.gotTool = append(r.gotTool, tools)
	if r.err != nil {
		return LLMResponse{}, r.err
	}
	return LLMResponse{Content: r.out}, nil
}

// TestLLMSummarizerCallsModel 验证生成式摘要：原对话（角色: 内容）被拼进
// 一条 user 消息喂给模型，tools 为 nil，输出直接透传。
func TestLLMSummarizerCallsModel(t *testing.T) {
	llm := &recordingLLM{out: "用户要查北京天气，已完成查询，结果晴 24 度。"}
	s := NewLLMSummarizer(llm)

	msgs := []Message{
		{Role: roleUser, Content: "查北京天气"},
		{Role: roleTool, Content: `{"city":"北京","weather":"晴","temp":24}`},
	}
	got, err := s.Summarize(msgs)
	if err != nil {
		t.Fatalf("Summarize 失败: %v", err)
	}
	if got != llm.out {
		t.Errorf("应透传模型输出, got %q", got)
	}
	if len(llm.got) != 1 {
		t.Fatalf("应只调用模型 1 次, got %d", len(llm.got))
	}
	req := llm.got[0]
	if len(req) != 1 || req[0].Role != roleUser {
		t.Errorf("摘要请求应为 1 条 user 消息, got %+v", req)
	}
	if !strings.Contains(req[0].Content, "查北京天气") || !strings.Contains(req[0].Content, "temp") {
		t.Errorf("摘要输入应包含原对话内容, got: %q", req[0].Content)
	}
	if llm.gotTool[0] != nil {
		t.Errorf("摘要请求 tools 应为 nil, got %+v", llm.gotTool[0])
	}
}

// TestLLMSummarizerTrimsSource 验证输入预算裁剪：超长历史先丢最旧，
// 不会把超预算原文原样喂给摘要模型（防"压缩器自己撑爆窗口"）。
func TestLLMSummarizerTrimsSource(t *testing.T) {
	llm := &recordingLLM{out: "摘要"}
	s := NewLLMSummarizer(llm)
	s.maxSourceTokens = 100 // 故意设小预算触发裁剪

	long := strings.Repeat("天气预报内容很长很长", 30) // 约 300 字，单条本身超预算
	msgs := []Message{
		{Role: roleUser, Content: "任务"},
		{Role: roleTool, Content: long},
		{Role: roleTool, Content: long},
		{Role: roleTool, Content: long},
	}
	original := MessagesTokens(msgs) // 原文 ≈ 900+ token
	if _, err := s.Summarize(msgs); err != nil {
		t.Fatalf("Summarize 失败: %v", err)
	}
	req := llm.got[0][0].Content
	// 裁剪后喂给模型的文本应远小于原文总量（原文一半以下即视为裁剪生效）
	if EstimateTokens(req) > original/2 {
		t.Errorf("喂给摘要模型的输入应被裁剪：原文估算 %d，实际喂入 %d", original, EstimateTokens(req))
	}
}

// TestLLMSummarizerError 验证模型失败时返回 error（由 CompactContext 兜底 FIFO）。
func TestLLMSummarizerError(t *testing.T) {
	llm := &recordingLLM{err: stringsReaderErr}
	s := NewLLMSummarizer(llm)
	if _, err := s.Summarize([]Message{{Role: roleUser, Content: "x"}}); err == nil {
		t.Fatal("模型失败时 Summarize 应返回 error")
	}
}

// stringsReaderErr 是测试内 error 快捷构造（避免顶层污染）。
var stringsReaderErr = errT{}

type errT struct{}

func (errT) Error() string { return "model down" }

// TestCompactUsesLLMSummarizer 端到端：Agent 配生成式摘要器 + 小预算 →
// 超预算触发压缩，历史里出现【历史摘要】且内容来自摘要模型。
func TestCompactUsesLLMSummarizer(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "get_weather",
		BaseRisk: RiskNone,
		Execute: func(input string) (string, error) {
			return `{"city":"北京","weather":"晴","temp":22,"summary":"今天白天晴转多云，午后体感舒适，适合户外活动，夜间最低温18度，注意添衣。"}`, nil
		},
	})

	// 4 次工具调用撑大历史，mock 摘要模型固定返回一句话
	llm := NewMockLLM([]MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"上海"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"杭州"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"广州"}`},
		{Content: "四城天气已汇总。"},
	})
	summ := &recordingLLM{out: "摘要模型说：已查北京/上海/杭州/广州四城天气，均为晴。"}

	a := NewAgent(llm, reg,
		WithMaxSteps(8),
		WithTokenBudget(200), // 远小于历史增长速度，必然触发压缩
		WithSummarizer(NewLLMSummarizer(summ)),
	)
	res := a.Run("分别查四城天气并汇总")
	if res.Status != StatusCompleted {
		t.Fatalf("应 completed，实际 %s（%s）", res.Status, res.Reason)
	}
	if len(summ.got) == 0 {
		t.Fatal("生成式摘要器应被实际调用")
	}

	seen := false
	for _, m := range a.History() {
		if m.Role == roleSystem && strings.Contains(m.Content, "【历史摘要】") && strings.Contains(m.Content, "摘要模型说") {
			seen = true
		}
	}
	if !seen {
		t.Error("历史应包含来自摘要模型的【历史摘要】消息")
	}
}

// TestCompactUnconvergedSummaryFallsBackFIFO 验证收敛保证：生成式摘要器
// 返回超长摘要（比它替换的旧批次还大）时，插入摘要会让 while 循环永不
// 收敛（每次压缩 token 不降）——内核必须回退 FIFO 丢弃，保证必然收敛。
func TestCompactUnconvergedSummaryFallsBackFIFO(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{
		Name:     "get_weather",
		BaseRisk: RiskNone,
		Execute: func(input string) (string, error) {
			return `{"city":"北京","weather":"晴","temp":22}`, nil
		},
	})

	// 摘要模型固定返回超长文本：token 大于被压缩的旧批次
	huge := strings.Repeat("摘要内容非常非常长", 100) // 约 1000 字
	llm := NewMockLLM([]MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"上海"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"杭州"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"广州"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"广州"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"广州"}`},
		{Content: "完成"},
	})
	summ := &recordingLLM{out: huge}

	// 预算 120：历史快速超预算；摘要比旧批次大 → 全部走 FIFO
	a := NewAgent(llm, reg,
		WithMaxSteps(8),
		WithTokenBudget(120),
		WithSummarizer(NewLLMSummarizer(summ)),
	)
	res := a.Run("查天气")
	if res.Status != StatusCompleted {
		t.Fatalf("应 completed（循环必须收敛），实际 %s（%s）", res.Status, res.Reason)
	}
	if MessagesTokens(a.History()) > 120 {
		t.Errorf("压缩后历史应 ≤ 预算 120，实际 %d", MessagesTokens(a.History()))
	}
	// 摘要未被插入（回退了 FIFO）
	for _, m := range a.History() {
		if strings.Contains(m.Content, "摘要内容非常非常长") {
			t.Fatal("超长摘要不应被插入历史（应回退 FIFO）")
		}
	}
}
