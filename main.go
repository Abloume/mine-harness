// mine-harness 入口：组装一个最小 Agent，跑通"工具调用 → 回填 → 最终回答"的完整链路。
package main

import (
	"flag"
	"fmt"
	"log"

	"mine-harness/internal/agent"
)

func main() {
	verbose := flag.Bool("v", true, "打印每步 trace")
	flag.Parse()

	// 1. 工具注册：mini-harness 里只有一个 mock 天气工具（不联网）
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气",
		Execute: func(input string) (string, error) {
			// mock 实现：固定返回，演示用。真实工具会在这里调外部 API。
			return `{"city":"北京","weather":"晴","temp":24}`, nil
		},
	})

	// 2. mock 模型脚本：先请求查天气，再基于工具结果给出总结。
	// 这模拟了真实模型"感知工具结果后收敛"的行为，让链路可复现。
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{Content: "北京今天晴，24 度，适合出门。"},
	})

	// 3. 组装内核并运行
	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(5),
		agent.WithTokenBudget(2000),
		agent.WithVerbose(*verbose),
	)

	answer, err := a.Run("帮我查一下北京的天气并总结")
	if err != nil {
		log.Fatalf("运行失败: %v", err)
	}
	fmt.Println("==== 最终回答 ====")
	fmt.Println(answer)
}
