package agent

import "testing"

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
