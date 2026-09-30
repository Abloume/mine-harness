// mine-harness 入口：七个演示场景，覆盖"正常链路 / 循环检测 / 软停止 / 摘要压缩 / 审批点 / 真实模型 / 真实审批"。
// 用法：go run . -demo normal | loop | soft | compact | approval | real | real-approval
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"mine-harness/internal/agent"
)

func main() {
	demo := flag.String("demo", "normal", "演示场景: normal | loop | soft | compact | approval | real | real-approval")
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
	case "real":
		runDemoReal(*verbose)
	case "real-approval":
		runDemoRealApproval(*verbose)
	default:
		runDemoNormal(*verbose)
	}
}

// loadEnv 极简 .env 加载：按行解析 KEY=VALUE 写入环境变量。
// 已存在的环境变量优先（.env 只是兜底）；不处理引号/转义，够 demo 用即可，
// 生产用官方 dotenv 库。
func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 文件不存在就跳过，靠真实环境变量
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// runDemoReal 演示真实模型接入：智谱 GLM（OpenAI 兼容格式）+ 真实工具调用。
// 需要有效的 API Key：优先读环境变量 ZHIPU_API_KEY，或项目根 .env 文件
// （.env 已被 .gitignore 排除，不会进仓库）。
// 模型默认 glm-4.7-flash（智谱免费模型），可换成其他。
func runDemoReal(verbose bool) {
	loadEnv(".env")
	key := os.Getenv("ZHIPU_API_KEY")
	if key == "" {
		fmt.Println("缺少 ZHIPU_API_KEY：export ZHIPU_API_KEY=xxx 或写入项目根 .env（已被 git 忽略）")
		return
	}

	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气。参数是 JSON：{\"city\":\"城市名\"}",
		Execute: func(input string) (string, error) {
			// 演示用 mock 数据源；真实系统这里会调用天气服务
			return `{"city":"北京","weather":"晴","temp":24,"humidity":40}`, nil
		},
	})

	llm := agent.NewOpenAICompatibleProvider(
		"https://open.bigmodel.cn/api/paas/v4", // 智谱 BigModel
		"glm-4.7-flash",                        // 免费模型
		key,
	)

	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(5), agent.WithVerbose(verbose))
	showResult(a.Run("帮我查一下北京的天气，并告诉我要不要带伞"))
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

// runDemoApproval 演示分级审批点 + 模型自动判断风险：
// 同一个工具 file_op 按参数定风险——read 低风险自动放行（不打扰），
// delete 被风险模型判定为 HIGH → 进审批闸门并被拒绝；模型看到"未授权"后
// 改走安全路径。真实交互场景应换 agent.NewCLIApprover()（阻塞读 stdin）。
func runDemoApproval(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "file_op",
		Description: "文件操作：read 读文件 / delete 删除文件（基础风险 L1，实际风险由模型判定）",
		BaseRisk:    agent.RiskLow,
		Execute: func(input string) (string, error) {
			if strings.Contains(input, "delete") {
				return "file deleted", nil
			}
			return "file content: hello mini-harness", nil
		},
	})

	// 风险判断模型：独立实例，不占用主 agent 的模型预算（生产里是专用小模型/分类器）。
	// 它按"工具+参数"输出级别名，评估器只升不降——delete 判 HIGH、read 判 LOW。
	riskLLM := agent.NewMockLLM([]agent.MockDecision{
		{Content: "HIGH"}, // delete 参数 → 高风险
		{Content: "LOW"},  // read 参数 → 低风险
	})
	riskEval := agent.NewLLMRiskEvaluator(riskLLM)

	// 审批决策器：拒绝所有进闸门的调用（演示 fail-closed + 升级语义）。
	approver := agent.ApproverFunc(func(name, args string) bool { return false })

	// 主模型脚本：先尝试高风险 delete → 被拒 → 学乖走低风险 read → 完成
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "file_op", ToolInput: `{"action":"delete","file":"/tmp/report.txt"}`},
		{ToolName: "file_op", ToolInput: `{"action":"read","file":"/tmp/report.txt"}`},
		{Content: "读取成功：hello mini-harness。删除未执行（用户未授权）。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(5),
		agent.WithRiskEvaluator(riskEval),
		agent.WithApprover(approver),
		agent.WithVerbose(verbose))
	showResult(a.Run("先删除 /tmp/report.txt，再读它"))
}

// runDemoRealApproval 演示真实链路的"模型自动判断是否要审批"：
// 主模型和风险判断模型都是真实 GLM——每个工具调用前，风险模型先判断风险级别
// （参数感知：delete → HIGH / read → LOW），达到审批阈值进闸门（这里一律拒绝，
// 演示 fail-closed + 拒绝回填）。这就是"规则优先、模型辅助"的完整落地。
func runDemoRealApproval(verbose bool) {
	loadEnv(".env")
	key := os.Getenv("ZHIPU_API_KEY")
	if key == "" {
		fmt.Println("缺少 ZHIPU_API_KEY：export ZHIPU_API_KEY=xxx 或写入项目根 .env")
		return
	}

	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "file_op",
		Description: "文件操作。参数是 JSON：{\"action\":\"read|delete\",\"file\":\"路径\"}",
		BaseRisk:    agent.RiskLow, // 基础级别低；高风险由风险模型动态上调
		Execute: func(input string) (string, error) {
			if strings.Contains(input, "delete") {
				return "file deleted", nil
			}
			return "file content: hello mini-harness", nil
		},
	})

	base := "https://open.bigmodel.cn/api/paas/v4"
	// 模型可用 ZHIPU_MODEL 环境变量覆盖（默认 glm-4.7-flash）：
	// 免费模型高峰可能被限流/过载，切换其他免费模型（如 glm-4.5-flash）即可验证。
	model := os.Getenv("ZHIPU_MODEL")
	if model == "" {
		model = "glm-4.7-flash"
	}
	// 主模型：完成任务的 agent 循环
	mainLLM := agent.NewOpenAICompatibleProvider(base, model, key)
	// 风险模型：独立实例，只做风险判断（生产里应换专用小模型/分类器更省）。
	// 与主模型共享 ZHIPU_MODEL 切换；MaxTokens 给足：glm-4.7-flash 是混合思考
	// 模型，reasoning 会先吃掉预算，预算不足时 content 为空、parseRisk 失败——
	// 宁给足预算也不冒静默漏审的险。
	riskLLM := agent.NewOpenAICompatibleProvider(base, model, key)
	riskLLM.MaxTokens = 2048

	a := agent.NewAgent(mainLLM, reg,
		agent.WithMaxSteps(6),
		agent.WithRiskEvaluator(agent.NewLLMRiskEvaluator(riskLLM)),
		agent.WithApprover(agent.DenyApprover{}), // 演示：进闸门的一律拒绝
		agent.WithVerbose(verbose))
	showResult(a.Run("请对文件 /tmp/report.txt 依次执行：1) 删除它 2) 读取它"))
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
