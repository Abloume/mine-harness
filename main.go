// mine-harness 入口：五个演示场景，覆盖"正常链路 / 循环检测 / 软停止 / 摘要压缩 / 审批点"。
// 用法：go run . -demo normal | loop | soft | compact | approval
package main

import (
	"flag"
	"fmt"
	"strings"

	"mine-harness/internal/agent"
)

func main() {
	demo := flag.String("demo", "normal", "演示场景: normal | loop | soft | compact | approval")
	verbose := flag.Bool("v", true, "打印每步 trace")
	flag.Parse()

	switch *demo {
	case "loop":
		runDemoLoop(*verbose)
	case "soft":
		runDemoSoftStop(*verbose)
	case "compact":
		runDemoCompact(*verbose)
	case "approval":
		runDemoApproval(*verbose)
	default:
		runDemoNormal(*verbose)
	}
}

// runDemoNormal 演示完整成功链路：mock 模型先查天气，再基于结果总结。
func runDemoNormal(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气",
		Execute: func(input string) (string, error) {
			return `{"city":"北京","weather":"晴","temp":24}`, nil
		},
	})

	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{Content: "北京今天晴，24 度，适合出门。"},
	})

	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(5), agent.WithVerbose(verbose))
	showResult(a.Run("帮我查一下北京的天气并总结"))
}

// runDemoLoop 演示循环检测：mock 模型反复用相同参数调用同一工具。
// 期望：第 2 次命中 → 注入警告；第 3 次仍重复 → 中止（软停止，不是 error）。
func runDemoLoop(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气",
		Execute: func(input string) (string, error) {
			return `{"city":"北京","weather":"晴","temp":24}`, nil
		},
	})

	// 病态模型脚本：不换参数、不收敛，一直查同一个城市
	steps := make([]agent.MockDecision, 0, 5)
	for i := 0; i < 5; i++ {
		steps = append(steps, agent.MockDecision{ToolName: "get_weather", ToolInput: `{"city":"北京"}`})
	}
	llm := agent.NewMockLLM(steps)

	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(5), agent.WithVerbose(verbose))
	showResult(a.Run("帮我查一下北京的天气并总结"))
}

// runDemoSoftStop 演示步数软停止：maxSteps=2，但任务需要 3 步才能完成。
// 期望：第 2 步结束后撞上限 → 返回"未完成 + 原因"，而不是报错中断。
func runDemoSoftStop(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气",
		Execute: func(input string) (string, error) {
			// mock：按输入中的城市名返回对应数据，演示用
			if strings.Contains(input, "杭州") {
				return `{"city":"杭州","weather":"小雨","temp":16}`, nil
			}
			if strings.Contains(input, "上海") {
				return `{"city":"上海","weather":"多云","temp":18}`, nil
			}
			return `{"city":"未知","weather":"未知"}`, nil
		},
	})

	// 正常脚本但需要 3 步：两次工具调用 + 一次最终回答
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"上海"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"杭州"}`},
		{Content: "上海多云 18 度，杭州待补充。"},
	})

	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(2), agent.WithVerbose(verbose))
	showResult(a.Run("帮我查上海和杭州的天气并总结"))
}

// runDemoCompact 演示摘要压缩：多次工具调用产生长历史 + 很小的 token 预算。
// 期望：历史超预算后，最旧的工具结果被压缩成摘要放回（不是裸丢），任务仍能完成。
func runDemoCompact(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气（返回详细预报）",
		Execute: func(input string) (string, error) {
			// mock：返回较长文本，模拟真实工具的大结果撑爆上下文
			city := "未知"
			for _, c := range []string{"北京", "上海", "杭州", "广州"} {
				if strings.Contains(input, c) {
					city = c
					break
				}
			}
			return fmt.Sprintf(`{"city":"%s","weather":"晴转多云","temp":22,"wind":"3级东南风","humidity":45,"summary":"%s今天白天晴转多云，午后体感舒适，适合户外活动，夜间最低温18度，注意添衣。"}`, city, city), nil
		},
	})

	// 连续查询 4 个城市（参数在变，不会误触发循环检测），历史快速膨胀
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"上海"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"杭州"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"广州"}`},
		{Content: "四城天气已汇总：北京晴、上海多云、杭州有雨、广州晴热。"},
	})

	// 预算 200：远小于历史增长速度，必然触发摘要压缩
	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(8), agent.WithTokenBudget(200), agent.WithVerbose(verbose))
	showResult(a.Run("分别查北京、上海、杭州、广州的天气并汇总"))
}

// runDemoApproval 演示审批点（human-in-the-loop）：
// mock 模型先请求高风险工具 delete_file → 被策略拒绝 → 看到"未授权"回填后
// 改走安全路径 get_weather → 完成。
// 真实交互场景应换 agent.NewCLIApprover()（会阻塞读 stdin 等人工 y/n）；
// 这里用脚本化决策器，方便自动化演示与测试。
func runDemoApproval(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气",
		Execute: func(input string) (string, error) {
			return `{"city":"北京","weather":"晴","temp":24}`, nil
		},
	})
	reg.Register(agent.Tool{
		Name:             "delete_file",
		Description:      "删除指定文件（高风险操作，需审批）",
		RequiresApproval: true,
		Execute: func(input string) (string, error) {
			return "file deleted", nil
		},
	})

	// 脚本化决策器：delete_file 一律拒绝，其余放行。
	// （真实场景换成 agent.NewCLIApprover() 手动 y/n。）
	approver := agent.ApproverFunc(func(name, args string) bool {
		return name != "delete_file"
	})

	// 模型脚本：先尝试高风险动作 → 被拒 → 学乖走安全路径 → 完成
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "delete_file", ToolInput: "/tmp/report.txt"},
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{Content: "北京今天晴，24 度。未执行删除操作（用户未授权）。"},
	})

	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(5), agent.WithApprover(approver), agent.WithVerbose(verbose))
	showResult(a.Run("先删除 /tmp/report.txt，再查北京天气"))
}

// showResult 统一打印 RunResult（completed / aborted 都按"结果"展示，不当作异常）。
func showResult(r agent.RunResult) {
	fmt.Println("==== 运行结果 ====")
	fmt.Printf("状态: %s  步数: %d\n", r.Status, r.Steps)
	switch r.Status {
	case agent.StatusCompleted:
		fmt.Printf("最终回答: %s\n", r.Answer)
	case agent.StatusAborted:
		fmt.Printf("未完成原因: %s\n", r.Reason)
		// 软停止的语义：任务中断，但系统没有崩——这就是生产 Agent 的
		// "交还给用户继续指挥"，而不是把 error 抛给上层。
	}
}
