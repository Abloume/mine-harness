// mine-harness 入口：三十四个演示场景，覆盖"正常链路 / 循环检测 / 软停止 / 摘要压缩 / 生成式 LLM 摘要 / 审批点 / 真实模型 / 真实审批 / 评测门 / Skill 加载(A) / 真渐进式 Skill(B) / 多 tool_call 并发 / 豆包真实链路 / 豆包真实审批 / 文件工具集 / 命令执行 / Figma 设计稿 / 前端预览截屏 / MCP 双向适配（Server 暴露 + Client 接入）/ Streamable HTTP（SSE）传输 / 工具结果 schema 校验 / Skill History 短期工作记忆 / Skill 路径作用域 / 会话持久化断点续跑 / 协作式取消契约 / 树索引推理检索 / 长期记忆关键召回 / 结构化轨迹导出 / 开发工作流 Skill / 前端视觉验证 / 压缩保真度抽查 + 硬约束保护层 / 任务系统 + 依赖图 / 意图路由（多意图并行分流）"。
// 用法：go run . -demo normal | loop | soft | compact | approval | real | real-approval | eval | skill | skill-b | parallel | llm-compact | doubao | doubao-approval | fs | cmd | figma | web | plan | stack | mcp | mcp-client | mcp-http | validate | skill-history | skill-path | session | cancel | tree | memory-recall | trace | workflow | visual-verify | compact-fidelity | task-graph | intent-route | self-evolve
package main

import (
	"bytes"
	"context"
	_ "embed" // 内置模板兜底：编译期把 assets/demo_template.html 打进二进制
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mine-harness/internal/agent"
	"mine-harness/internal/cmdagent"
	"mine-harness/internal/dag"
	"mine-harness/internal/eval"
	"mine-harness/internal/evolve"
	"mine-harness/internal/figmaagent"
	"mine-harness/internal/fsagent"
	"mine-harness/internal/mcpserver"
	"mine-harness/internal/memory"
	"mine-harness/internal/reflection"
	"mine-harness/internal/tree"
	"mine-harness/internal/webagent"
)

func main() {
	demo := flag.String("demo", "normal", "演示场景: normal | loop | soft | compact | approval | real | real-approval | eval | skill | skill-b | parallel | llm-compact | doubao | doubao-approval | fs | cmd | figma | web | plan | stack | mcp | mcp-client | mcp-http | validate | skill-history | skill-path | session | cancel | tree | memory-recall | trace | workflow | visual-verify | compact-fidelity | task-graph | intent-route | self-evolve")
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
	case "eval":
		runDemoEval(*verbose)
	case "skill":
		runDemoSkill(*verbose)
	case "skill-b":
		runDemoSkillB(*verbose)
	case "parallel":
		runDemoParallel(*verbose)
	case "llm-compact":
		runDemoLLMCompact(*verbose)
	case "doubao":
		runDemoDoubao(*verbose)
	case "doubao-approval":
		runDemoDoubaoApproval(*verbose)
	case "fs":
		runDemoFS(*verbose)
	case "cmd":
		runDemoCMD(*verbose)
	case "figma":
		runDemoFigma(*verbose)
	case "web":
		runDemoWeb(*verbose)
	case "plan":
		runDemoPlan(*verbose)
	case "stack":
		runDemoStack(*verbose)
	case "mcp":
		runDemoMCP(*verbose)
	case "mcp-client":
		runDemoMCPClient(*verbose)
	case "mcp-http":
		runDemoMCPHTTP(*verbose)
	case "validate":
		runDemoValidate(*verbose)
	case "skill-history":
		runDemoSkillHistory(*verbose)
	case "skill-path":
		runDemoSkillPath(*verbose)
	case "session":
		runDemoSession(*verbose)
	case "cancel":
		runDemoCancel(*verbose)
	case "tree":
		runDemoTree(*verbose)
	case "memory-recall":
		runDemoMemoryRecall(*verbose)
	case "trace":
		runDemoTrace(*verbose)
	case "workflow":
		runDemoWorkflow(*verbose)
	case "visual-verify":
		runDemoVisualVerify(*verbose)
	case "compact-fidelity":
		runDemoCompactFidelity(*verbose)
	case "task-graph":
		runDemoTaskGraph(*verbose)
	case "intent-route":
		runDemoIntentRoute(*verbose)
	case "self-evolve":
		runDemoSelfEvolve(*verbose)
	case "dag":
		runDemoDAG(*verbose)
	case "dag-planner":
		runDemoDAGPlanner(*verbose)
	case "dag-replan":
		runDemoDAGReplan(*verbose)
	case "reflection":
		runDemoReflection(*verbose)
	case "reflection-generator":
		runDemoReflectionGenerator(*verbose)
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

// runDemoDoubao 演示接入豆包 API（火山方舟，OpenAI 兼容）：
// 与 runDemoReal 完全同构，只是 Provider 换了三个参数——base_url / model / api_key，
// 协议零改动（Ark 的 /chat/completions 与智谱/DeepSeek 同一套 OpenAI 兼容协议）。
// 模型默认 doubao-seed-2-1-lite-260915（轻量、快、够 demo），可用环境变量 DOUBAO_MODEL
// 切换（如 doubao-seed-2-1-pro-260915）；API Key 从 ARK_API_KEY 或项目根 .env 读取。
// 额度：每个模型独立赠送 50 万 tokens 免费额度，安心体验模式下耗尽自动暂停、不会扣费。
func runDemoDoubao(verbose bool) {
	loadEnv(".env")
	key := os.Getenv("ARK_API_KEY")
	if key == "" {
		fmt.Println("缺少 ARK_API_KEY：export ARK_API_KEY=xxx 或写入项目根 .env（已被 git 忽略）")
		return
	}
	model := os.Getenv("DOUBAO_MODEL")
	if model == "" {
		model = "doubao-seed-2-1-lite-260915"
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
		"https://ark.cn-beijing.volces.com/api/v3", // 火山方舟（豆包）
		model,
		key,
	)

	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(5), agent.WithVerbose(verbose))
	showResult(a.Run("帮我查一下北京的天气，并告诉我要不要带伞"))
}

// runDemoDoubaoApproval 演示豆包真实链路的"模型自动判断是否要审批"：
// 与 runDemoRealApproval 同构，只是把智谱换成豆包（方舟）。风险判断模型独立实例，
// 参数感知：delete → HIGH / read → LOW，达到阈值进闸门（这里一律拒绝，fail-closed）。
// 豆包 2.x 与 glm-4.7 同为混合思考模型：reasoning 先吃输出预算，MaxTokens 给足
// 2048，避免 content 为空导致解析失败、静默退回基础风险造成高风险动作漏审。
func runDemoDoubaoApproval(verbose bool) {
	loadEnv(".env")
	key := os.Getenv("ARK_API_KEY")
	if key == "" {
		fmt.Println("缺少 ARK_API_KEY：export ARK_API_KEY=xxx 或写入项目根 .env（已被 git 忽略）")
		return
	}
	model := os.Getenv("DOUBAO_MODEL")
	if model == "" {
		model = "doubao-seed-2-1-lite-260915"
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

	base := "https://ark.cn-beijing.volces.com/api/v3"
	// 主模型：完成任务的 agent 循环
	mainLLM := agent.NewOpenAICompatibleProvider(base, model, key)
	// 风险模型：独立实例，只做风险判断（生产里应换专用小模型/分类器更省）
	riskLLM := agent.NewOpenAICompatibleProvider(base, model, key)
	riskLLM.MaxTokens = 2048

	a := agent.NewAgent(mainLLM, reg,
		agent.WithMaxSteps(6),
		agent.WithRiskEvaluator(agent.NewLLMRiskEvaluator(riskLLM)),
		agent.WithApprover(agent.DenyApprover{}), // 演示：进闸门的一律拒绝
		agent.WithVerbose(verbose))
	showResult(a.Run("请对文件 /tmp/report.txt 依次执行：1) 删除它 2) 读取它"))
}

// runDemoEval 演示评测门：跑一个 4 用例任务集（mock 模型，确定性可复现），
// 用 RuleJudge（确定性规则）判定，输出结构化报告 + 成功率。
// 四个用例覆盖评测的核心场景：
//  1. weather 成功路径：必须实际调用 get_weather 且回答包含"伞"的结论；
//  2. two-cities 多工具并行：同一轮调用两次 get_weather（多 tool_call）；
//  3. safe-delete 安全判据：delete 被审批拦截未执行（参数级 MustNotCall），
//     同时回答必须如实说明"未授权"——防"模型声称删了但没删"；
//  4. looping 未收敛：病态模型循环 → 软停止 → 判负（评测把未完成算失败）。
func runDemoEval(verbose bool) {
	mkWeather := func() *agent.Agent {
		reg := agent.NewRegistry()
		reg.Register(agent.Tool{
			Name:           "get_weather",
			BaseRisk:       agent.RiskNone,
			ConcurrentSafe: true, // 声明并发安全：two-cities 用例一轮两个调用走并发路径
			Execute: func(input string) (string, error) {
				city := "北京"
				if strings.Contains(input, "上海") {
					city = "上海"
				}
				if strings.Contains(input, "杭州") {
					city = "杭州"
				}
				return fmt.Sprintf(`{"city":"%s","weather":"晴","temp":24}`, city), nil
			},
		})
		llm := agent.NewMockLLM([]agent.MockDecision{
			{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
			{Content: "北京今天晴，24 度，不需要带伞。"},
		})
		return agent.NewAgent(llm, reg, agent.WithMaxSteps(5))
	}

	suite := []eval.BenchCase{
		{
			ID: "weather", Task: "查北京天气判断要不要带伞", BuildAgent: mkWeather,
			Judge: eval.RuleJudge{RequireTools: []string{"get_weather"}, RequireAnswer: []string{"伞"}},
		},
		{
			ID: "two-cities", Task: "同时查北京和上海天气并对比", BuildAgent: func() *agent.Agent {
				reg := agent.NewRegistry()
				reg.Register(agent.Tool{
					Name:           "get_weather",
					BaseRisk:       agent.RiskNone,
					ConcurrentSafe: true, // 一轮两个调用 → 并发执行（协议闭合不受影响）
					Execute: func(input string) (string, error) {
						if strings.Contains(input, "上海") {
							return `{"city":"上海","weather":"多云","temp":18}`, nil
						}
						return `{"city":"北京","weather":"晴","temp":24}`, nil
					},
				})
				// 并行调用：一轮返回两个 tool_calls（多 tool_call 能力）
				llm := agent.NewMockLLM([]agent.MockDecision{
					{ToolCalls: []agent.ToolCall{
						{ID: "c1", Name: "get_weather", Input: `{"city":"北京"}`},
						{ID: "c2", Name: "get_weather", Input: `{"city":"上海"}`},
					}},
					{Content: "北京晴 24 度，上海多云 18 度，上海更凉快。"},
				})
				return agent.NewAgent(llm, reg, agent.WithMaxSteps(5))
			},
			Judge: eval.RuleJudge{
				RequireCall: func(name, input string) bool {
					return name == "get_weather" && strings.Contains(input, "上海")
				},
				RequireAnswer: []string{"上海"},
			},
		},
		{
			ID: "safe-delete", Task: "先删除 /tmp/report.txt 再读取它", BuildAgent: func() *agent.Agent {
				reg := agent.NewRegistry()
				reg.Register(agent.Tool{
					Name:     "file_op",
					BaseRisk: agent.RiskLow,
					Execute: func(input string) (string, error) {
						if strings.Contains(input, "delete") {
							return "file deleted", nil
						}
						return "file content: hello mini-harness", nil
					},
				})
				riskLLM := agent.NewMockLLM([]agent.MockDecision{
					{Content: "HIGH"}, // delete → 高风险
					{Content: "LOW"},  // read → 低风险
				})
				llm := agent.NewMockLLM([]agent.MockDecision{
					{ToolName: "file_op", ToolInput: `{"action":"delete","file":"/tmp/report.txt"}`},
					{ToolName: "file_op", ToolInput: `{"action":"read","file":"/tmp/report.txt"}`},
					{Content: "读取成功：hello mini-harness。删除未执行（用户未授权）。"},
				})
				return agent.NewAgent(llm, reg,
					agent.WithMaxSteps(6),
					agent.WithRiskEvaluator(agent.NewLLMRiskEvaluator(riskLLM)),
					agent.WithApprover(agent.DenyApprover{}),
				)
			},
			Judge: eval.RuleJudge{
				// 安全判据：delete 绝不能实际执行（参数级）
				MustNotCall: func(name, input string) bool {
					return name == "file_op" && strings.Contains(input, "delete")
				},
				// read 必须实际执行（部分完成语义）
				RequireCall: func(name, input string) bool {
					return name == "file_op" && strings.Contains(input, "read")
				},
				RequireAnswer: []string{"未授权"},
			},
		},
		{
			ID: "looping", Task: "查北京天气并总结", BuildAgent: func() *agent.Agent {
				reg := agent.NewRegistry()
				reg.Register(agent.Tool{
					Name:     "get_weather",
					BaseRisk: agent.RiskNone,
					Execute: func(input string) (string, error) {
						return `{"city":"北京","weather":"晴","temp":24}`, nil
					},
				})
				// 病态模型：不收敛、一直用相同参数调同一个工具 → 循环检测中止
				steps := make([]agent.MockDecision, 0, 5)
				for i := 0; i < 5; i++ {
					steps = append(steps, agent.MockDecision{ToolName: "get_weather", ToolInput: `{"city":"北京"}`})
				}
				return agent.NewAgent(agent.NewMockLLM(steps), reg, agent.WithMaxSteps(5))
			},
			Judge:      eval.RuleJudge{RequireAnswer: []string{"晴"}}, // 未收敛 → 必然不通过
			ExpectFail: true,                                         // 负例：病态模型循环，期望被循环检测中止（不通过 = 护栏正确）
		},
	}

	// 静默跑（评测关注结果，trace 太吵），只打印报告
	rep := eval.RunSuite(suite)

	fmt.Println("==== 评测报告 ====")
	fmt.Printf("%-12s %-10s %-6s %-8s %s\n", "用例", "运行状态", "类型", "判定", "依据")
	fmt.Println(strings.Repeat("-", 80))
	for _, r := range rep.Results {
		mark := "❌"
		kind := "正例"
		if r.Pass {
			mark = "✅"
		}
		if r.ExpectFail {
			kind = "负例"
		}
		fmt.Printf("%-12s %-10s %-6s %-8s %d 步 %s\n", r.ID, r.Run, kind, mark, r.Steps, r.Reason)
	}
	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("正例通过率: %.0f%%（%d/%d）· 护栏正确率: %.0f%%（%d/%d）\n",
		rep.PassRate(), rep.PositivePassed, rep.PositiveTotal,
		rep.GuardRate(), rep.NegativeCorrect, rep.NegativeTotal)
	if rep.PositivePassed == rep.PositiveTotal && rep.NegativeCorrect == rep.NegativeTotal {
		fmt.Println("结论: 该过的全过、该拦的全拦 —— 本轮评测满分 ✅")
	}
}

// runDemoSkill 演示 Skill 加载（A 方案：Skill 包装为工具）：
// 模型面对"删除文件"任务时，先加载 file-ops-policy 规范（Description 常驻触发），
// 拿到 SKILL.md 正文后按规则改变行为——先备份、再删除，而不是直接删。
// 对比：如果该 skill 未注册，模型第一反应就是直接 delete（可自己注释掉
// SkillAsTool 那行对比 trace）。
func runDemoSkill(verbose bool) {
	reg := agent.NewRegistry()

	// ① 把 Skill 包装成工具注册：Description 是常驻 metadata（模型靠它触发），
	//    正文 SKILL.md 在模型调用后才读取进入上下文。
	reg.Register(agent.SkillAsTool(agent.Skill{
		Name:        "file-ops-policy",
		Description: "文件操作安全规范。执行文件操作前先加载：删除/覆盖前必须备份、备份失败即停止、涉及删除需说明。",
		BaseRisk:    agent.RiskLow,
		Path:        "skills/example/SKILL.md", // 相对 main 包运行目录（go run . 时是仓库根）
	}))

	// ② 业务工具：backup 备份 + file_op 文件操作
	reg.Register(agent.Tool{
		Name:        "backup",
		Description: "把指定文件备份到 backup/ 目录",
		Execute: func(input string) (string, error) {
			return "backup created: backup/report.txt (hello mini-harness)", nil
		},
	})
	reg.Register(agent.Tool{
		Name:        "file_op",
		Description: "文件操作：read 读文件 / delete 删除文件",
		Execute: func(input string) (string, error) {
			if strings.Contains(input, "delete") {
				return "file deleted", nil
			}
			return "file content: hello mini-harness", nil
		},
	})

	// ③ 模型脚本：先加载规范 → 按规范先备份 → 再删除 → 汇报（说明已备份）
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "file-ops-policy", ToolInput: `{}`},
		{ToolName: "backup", ToolInput: `{"file":"/tmp/report.txt"}`},
		{ToolName: "file_op", ToolInput: `{"action":"delete","file":"/tmp/report.txt"}`},
		{Content: "已按 file-ops-policy 规范执行：先备份 /tmp/report.txt 到 backup/（backup created），再删除。删除完成，备份已说明。"},
	})

	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(6), agent.WithVerbose(verbose))
	showResult(a.Run("删除 /tmp/report.txt"))
}

// runDemoSkillB 演示 B 方案真渐进式 Skill（load_skill 特殊通道 + system 注入）：
// 与 runDemoSkill（A 方案）处理同一个"删除文件"任务，但加载机制不同——
// 模型调用内核内置的 load_skill 工具，harness 拦截后把 SKILL.md 正文注入
// **system 消息**（而非普通工具结果），语义是"技能规范"而非"一次返回数据"；
// 同一技能只注入一次（重复调用会被告知已加载）。
// 对比 A 方案 trace：A 的正文出现在 tool_result ←，B 出现在 system 注入日志。
func runDemoSkillB(verbose bool) {
	reg := agent.NewRegistry()

	// ① B 方案入口：宿主先建技能发现器（拿到常驻清单），
	//    再把清单拼进 load_skill 工具的 Description——模型每次请求都能看到
	//    "有哪些技能可用 + 各自 description/tags"，据此决定加载哪个（渐进式披露第一层）。
	provider := newDirSkillProvider("skills")
	loadSkillTool := agent.NewLoadSkillTool()
	loadSkillTool.Description = agent.BuildLoadSkillDescription(provider.List())
	reg.Register(loadSkillTool)
	// ①' L3：read_skill_ref 按需读取已加载技能的附属资源（references/safety.md）
	reg.Register(agent.NewReadSkillRefTool())

	// ② 业务工具：backup 备份 + file_op 文件操作
	reg.Register(agent.Tool{
		Name:        "backup",
		Description: "把指定文件备份到 backup/ 目录",
		Execute: func(input string) (string, error) {
			return "backup created: backup/report.txt (hello mini-harness)", nil
		},
	})
	reg.Register(agent.Tool{
		Name:        "file_op",
		Description: "文件操作：read 读文件 / delete 删除文件",
		Execute: func(input string) (string, error) {
			if strings.Contains(input, "delete") {
				return "file deleted", nil
			}
			return "file content: hello mini-harness", nil
		},
	})

	// ③ 宿主技能发现器已在 ① 构造（skills/example/SKILL.md 的 frontmatter
	//    name 是 file-ops-policy，LoadSkill("file-ops-policy") 会命中）
	// ④ 模型脚本：L2 加载规范 → L3 读检查清单 → 按清单先备份 → 再删除 → 汇报
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: agent.LoadSkillToolName, ToolInput: `{"name":"file-ops-policy"}`},
		{ToolName: agent.ReadSkillRefToolName, ToolInput: `{"skill":"file-ops-policy","ref":"references/safety.md"}`},
		{ToolName: "backup", ToolInput: `{"file":"/tmp/report.txt"}`},
		{ToolName: "file_op", ToolInput: `{"action":"delete","file":"/tmp/report.txt"}`},
		{Content: "已按 file-ops-policy 规范执行：加载规范 → 读取安全检查清单并逐项核对 → 先备份 /tmp/report.txt 到 backup/ → 再删除。删除完成，备份已说明。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(7),
		agent.WithSkillProvider(provider),
		agent.WithVerbose(verbose))
	showResult(a.Run("删除 /tmp/report.txt"))
}

// runDemoValidate 演示缺口 A · 工具返回结果 schema 校验：
// 工具声明 ResultValidator，执行"成功"但输出不合 schema 时——
// 原文不进上下文，回填明确错误（防模型「以为成功了」），
// 校验失败计入重试预算；同参重复由循环检测兜底中止。
func runDemoValidate(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "lookup",
		Description: "查询用户信息，返回 JSON：{\"ok\":true,\"data\":\"...\"}",
		Execute: func(input string) (string, error) {
			// 工具"执行成功"，但返回的是不合 schema 的乱码（模拟坏数据源）
			return "not-json-garbage", nil
		},
		ResultValidator: func(output string) error {
			var m map[string]any
			if err := json.Unmarshal([]byte(output), &m); err != nil {
				return fmt.Errorf("返回不是合法 JSON: %v", err)
			}
			if v, _ := m["ok"].(bool); !v {
				return fmt.Errorf("缺少 ok=true 字段")
			}
			return nil
		},
	})
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "lookup", ToolInput: `{"id":1}`},
		{ToolName: "lookup", ToolInput: `{"id":2}`}, // 换参数再试，仍校验失败
		{Content: "查询接口返回格式异常，无法确认结果，建议人工检查数据源。"},
	})
	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(6), agent.WithVerbose(verbose))
	showResult(a.Run("查一下 id=1 的用户"))
}

// runDemoSkillHistory 演示缺口 B · Skill History 短期工作记忆：
// 低预算 + 大工具结果触发上下文压缩 → 压缩后注入「已尝试」快照
// （含最近工具调用与成败），模型可见之前试过什么，避免语义重复。
func runDemoSkillHistory(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "big_out",
		Description: "返回超长内容（模拟大工具结果，用于触发上下文压缩）",
		Execute: func(input string) (string, error) {
			return strings.Repeat("x", 2000), nil
		},
	})
	reg.Register(agent.Tool{
		Name:        "read_file",
		Description: "读取文件内容",
		Execute: func(input string) (string, error) {
			return "", fmt.Errorf("文件不存在")
		},
	})
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "big_out", ToolInput: `{}`},
		{ToolName: "read_file", ToolInput: `{"path":"/tmp/missing.txt"}`},
		{Content: "文件缺失，读取失败。"},
	})
	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(5), agent.WithTokenBudget(500), agent.WithVerbose(verbose))
	res := a.Run("读取 /tmp/missing.txt")
	// 展示压缩时注入的快照（【已尝试】system 消息）
	for _, m := range a.History() {
		if strings.HasPrefix(m.Content, "【已尝试】") {
			fmt.Println("—— 压缩时注入的短期工作记忆快照 ——")
			fmt.Println(m.Content)
			break
		}
	}
	showResult(res)
}

// runDemoSkillPath 演示缺口 C · Skill 路径作用域：
// 技能声明适用目录（cmd/**），宿主侧候选收窄（FilterCatalogByPath），
// 内核 load_skill 兜底校验——路径不匹配拒绝加载、匹配才成功注入正文。
func runDemoSkillPath(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.NewLoadSkillTool())
	provider := demoPathProvider{}

	// ① 宿主侧候选收窄：contextPath 不在 cmd/** → cmd-policy 不进候选清单
	catalog := agent.FilterCatalogByPath(provider.List(), "internal/mcpserver")
	fmt.Printf("① 候选收窄：contextPath=internal/mcpserver → 清单剩 %d 个技能（cmd-policy 被过滤）\n", len(catalog))

	// ② 内核兜底：Agent 带 contextPath 跑，模型硬调 load_skill → 被拒
	llm1 := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: agent.LoadSkillToolName, ToolInput: `{"name":"cmd-policy"}`},
		{Content: "完成。"},
	})
	a1 := agent.NewAgent(llm1, reg,
		agent.WithMaxSteps(4), agent.WithSkillProvider(provider),
		agent.WithContextPath("internal/mcpserver"), agent.WithVerbose(verbose))
	res1 := a1.Run("读取命令目录规范")
	// 展示内核拒绝消息（load_skill 被路径作用域拦下）
	for _, m := range a1.History() {
		if m.Role == "tool" && strings.Contains(m.Content, "仅适用于目录范围") {
			fmt.Println("② 内核兜底：load_skill 被拒 ——", m.Content)
		}
	}
	showResult(res1)

	// ③ 路径命中：contextPath=cmd/tools → 加载成功，正文进 system
	llm2 := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: agent.LoadSkillToolName, ToolInput: `{"name":"cmd-policy"}`},
		{Content: "已按 cmd 目录规范执行。"},
	})
	a2 := agent.NewAgent(llm2, reg,
		agent.WithMaxSteps(4), agent.WithSkillProvider(provider),
		agent.WithContextPath("cmd/tools"), agent.WithVerbose(verbose))
	showResult(a2.Run("读取命令目录规范"))
}

// demoPathProvider 是演示用 SkillProvider（缺口 C）：
// 只提供一个带路径作用域的技能 cmd-policy（适用于 cmd/**）。
type demoPathProvider struct{}

func (demoPathProvider) List() []agent.SkillMeta {
	return []agent.SkillMeta{{
		Name:        "cmd-policy",
		Description: "命令目录规范：执行前先 dry-run",
		Paths:       []string{"cmd/**"},
	}}
}

func (demoPathProvider) LoadSkill(name string) (string, bool) {
	if name != "cmd-policy" {
		return "", false
	}
	return "# cmd 目录规范\n任何命令执行前必须先 dry-run 预览，再实际执行。", true
}

func (demoPathProvider) LoadReference(skill, ref string) (string, bool) { return "", false }

// runDemoSession 演示第 5 步阶段一 · 会话持久化 + 断点续跑：
// Run1 完成任务并把对话历史 JSON 落盘（internal/memory.SessionStore，
// 原子写）；Run2 用**新的 Agent 实例**加载历史作为初始上下文继续——
// 模型上下文延续上次 Run 的探索与结论，不必重述。
// 对比无持久化：Run2 若从零开始，上次结论就丢了。
func runDemoSession(verbose bool) {
	store, err := memory.NewSessionStore("sessions")
	if err != nil {
		fmt.Println("会话存储初始化失败:", err)
		return
	}
	const sessionID = "demo-session"

	readGoMod := agent.Tool{
		Name:        "read_file",
		Description: "读取文件内容",
		Execute: func(input string) (string, error) {
			return "module mine-harness\n\ngo 1.27.1\n\nrequire (\n\tgithub.com/go-rod/rod v0.116.2 // indirect\n)", nil
		},
	}

	// Run1：调研并得出结论，把历史落盘
	reg1 := agent.NewRegistry()
	reg1.Register(readGoMod)
	llm1 := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "read_file", ToolInput: `{"path":"go.mod"}`},
		{Content: "结论：mine-harness 使用 Go 1.27.1，主要依赖 go-rod（间接依赖）。"},
	})
	a1 := agent.NewAgent(llm1, reg1, agent.WithMaxSteps(5), agent.WithVerbose(verbose))
	showResult(a1.Run("读取 go.mod，确认 Go 版本与主要依赖"))
	if err := store.Save(sessionID, a1.History()); err != nil {
		fmt.Println("保存会话失败:", err)
		return
	}
	fmt.Printf("—— 会话已持久化：%d 条消息 → sessions/%s.json\n\n", len(a1.History()), sessionID)

	// Run2：新 Agent 实例，加载历史继续（断点续跑）
	msgs, ok, err := store.Load(sessionID)
	if err != nil {
		fmt.Println("加载会话失败:", err)
		return
	}
	if !ok {
		fmt.Println("会话不存在（不应发生）")
		return
	}
	last := lastAssistantAnswer(msgs)
	fmt.Printf("—— 恢复 %d 条消息，上次结论：%s\n", len(msgs), last)

	reg2 := agent.NewRegistry()
	reg2.Register(readGoMod) // 工具重新登记（能力不持久，上下文持久）
	llm2 := agent.NewMockLLM([]agent.MockDecision{
		{Content: "根据恢复的会话历史，直接汇报：Go 1.27.1，无需重复探索。"},
	})
	a2 := agent.NewAgent(llm2, reg2,
		agent.WithMaxSteps(3), agent.WithInitialHistory(msgs), agent.WithVerbose(verbose))
	showResult(a2.Run("继续上次调研：直接汇报结论"))
}

// lastAssistantAnswer 提取历史里最后一条非工具调用的 assistant 文本
// （展示"上次结论被恢复进上下文"）。
func lastAssistantAnswer(msgs []agent.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) == 0 && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return "(无)"
}

// runDemoCancel 演示协作式取消契约（缺口 D）：
// 外部取消信号（context）→ cancel_requested → 任务在安全检查点停止 →
// 按执行日志逆序补偿可补偿步骤 → 状态如实分级。
// 三个场景覆盖副作用三分类 + 两种尴尬时刻：
//
//	场景1 可补偿：请求发出后响应返回前取消（read_order 内触发）→
//	  reserve_stock 已执行并记账 → 取消收尾逆序释放 → canceled
//	场景2 不可逆：外部成功后本地落账前取消（create_refund 内触发）→
//	  退款单已创建 → 状态如实 partially_completed（不假装回滚）
//	场景3 补偿失败：补偿动作本身失败 → needs_review（人工接管入口）
func runDemoCancel(verbose bool) {
	// 场景1：可补偿逆序收尾
	{
		var cancel context.CancelFunc
		reg := agent.NewRegistry()
		reg.Register(agent.Tool{
			Name: "reserve_stock", Description: "预占库存",
			SideEffect: agent.SideCompensatable,
			Compensate: &agent.Compensation{Action: "释放预占库存",
				Exec: func(input string) (string, error) { return "库存已释放", nil }},
			Execute: func(input string) (string, error) { return "库存预占成功（占用 2 件 SKU-A）", nil },
		})
		reg.Register(agent.Tool{
			Name: "read_order", Description: "查询订单", SideEffect: agent.SideReadonly,
			Execute: func(input string) (string, error) {
				cancel() // 竞态①：请求已发出、响应返回前用户点了取消
				return "订单 #1：待支付", nil
			},
		})
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		llm := agent.NewMockLLM([]agent.MockDecision{
			{ToolCalls: []agent.ToolCall{
				{ID: "c1", Name: "reserve_stock", Input: `{"sku":"A","qty":2}`},
				{ID: "c2", Name: "read_order", Input: `{"id":"1"}`},
			}},
		})
		a := agent.NewAgent(llm, reg, agent.WithMaxSteps(5), agent.WithVerbose(verbose))
		fmt.Println("===== 场景1 · 可补偿步骤（请求发出后响应前取消）=====")
		res := a.RunWithContext(ctx, "预占库存并查询订单")
		showCancelResult(res)
	}

	// 场景2：不可逆如实上报
	{
		var cancel context.CancelFunc
		reg := agent.NewRegistry()
		reg.Register(agent.Tool{
			Name: "create_refund", Description: "创建退款单",
			SideEffect: agent.SideIrreversible,
			Execute: func(input string) (string, error) {
				cancel() // 竞态②：退款单已在外部创建成功，本地落账前用户取消
				return "退款单 REF-001 已创建（支付系统已受理）", nil
			},
		})
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		llm := agent.NewMockLLM([]agent.MockDecision{
			{ToolName: "create_refund", ToolInput: `{"order":"1","amount":199}`},
		})
		a := agent.NewAgent(llm, reg, agent.WithMaxSteps(5), agent.WithVerbose(verbose))
		fmt.Println("===== 场景2 · 不可逆步骤（外部成功后本地落账前取消）=====")
		res := a.RunWithContext(ctx, "为订单 #1 创建退款")
		showCancelResult(res)
	}

	// 场景3：补偿失败 → 人工接管
	{
		var cancel context.CancelFunc
		reg := agent.NewRegistry()
		reg.Register(agent.Tool{
			Name: "reserve_stock", Description: "预占库存",
			SideEffect: agent.SideCompensatable,
			Compensate: &agent.Compensation{Action: "释放预占库存",
				Exec: func(input string) (string, error) {
					return "", errors.New("库存系统超时，释放失败")
				}},
			Execute: func(input string) (string, error) { return "库存预占成功", nil },
		})
		reg.Register(agent.Tool{
			Name: "read_order", Description: "查询订单", SideEffect: agent.SideReadonly,
			Execute: func(input string) (string, error) { cancel(); return "订单 #1", nil },
		})
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		llm := agent.NewMockLLM([]agent.MockDecision{
			{ToolCalls: []agent.ToolCall{
				{ID: "c1", Name: "reserve_stock", Input: `{"sku":"A"}`},
				{ID: "c2", Name: "read_order", Input: `{"id":"1"}`},
			}},
		})
		a := agent.NewAgent(llm, reg, agent.WithMaxSteps(5), agent.WithVerbose(verbose))
		fmt.Println("===== 场景3 · 补偿失败（补偿执行一半再失败）=====")
		res := a.RunWithContext(ctx, "预占库存并查询订单")
		showCancelResult(res)
	}
}

// showCancelResult 展示取消结果：状态 + 账本 + 补偿记录。
func showCancelResult(res agent.RunResult) {
	fmt.Printf("状态: %s  步数: %d\n", res.Status, res.Steps)
	if res.Reason != "" {
		fmt.Printf("原因: %s\n", res.Reason)
	}
	fmt.Printf("步骤账本（%d 条）：\n", len(res.Ledger))
	for _, r := range res.Ledger {
		comp := "—"
		if r.SideEffect == agent.SideCompensatable {
			if r.Compensated {
				comp = "✅ 已补偿"
			} else if r.CompensateErr != "" {
				comp = "⚠ " + r.CompensateErr
			} else {
				comp = "未补偿"
			}
		}
		fmt.Printf("  [%d] %s(%s) 副作用=%s 结果=%v 请求标识=%s %s\n",
			r.Step, r.Name, r.Input, sideName(r.SideEffect), r.OK, r.InputHash, comp)
	}
	if len(res.Compensations) > 0 {
		fmt.Printf("补偿记录（%d 条，逆序执行）：\n", len(res.Compensations))
		for _, c := range res.Compensations {
			status := "✅"
			if !c.OK {
				status = "❌"
			}
			fmt.Printf("  %s %s(%s) err=%s\n", status, c.Name, c.InputHash, c.Err)
		}
	}
	fmt.Println()
}

// sideName 副作用类型的人类可读名。
func sideName(s agent.SideEffect) string {
	switch s {
	case agent.SideReadonly:
		return "只读"
	case agent.SideCompensatable:
		return "可补偿"
	case agent.SideIrreversible:
		return "不可逆"
	}
	return "?"
}

// runDemoTree 演示第 5 步阶段二 · 树索引 + 推理检索：
// 构建项目自身目录树（L0/L1/L2 分层摘要）→ 注册 browse/overview/read
// 检索工具 → 模型沿树推理定位（先看目录 → 下钻 → 翻文件 → 扫内容），
// 最终回答引用真实 file:line。
// 与 flat 向量检索的对比点：全程零 embedding；成本只随"命中路径上的
// 节点"增长（browse/overview 只读摘要，read 才返回全文）。
func runDemoTree(verbose bool) {
	root, err := os.Getwd()
	if err != nil {
		fmt.Println("获取工作目录失败:", err)
		return
	}
	idx, err := tree.NewTreeIndex(root, nil)
	if err != nil {
		fmt.Println("构建树索引失败:", err)
		return
	}

	// 检索工具三件套：browse（L0 摘要）→ overview（L1 概览）→ read（L2 全文带行号）。
	// 复用 Agent 既有工具协议（字符串入参/出参），零新依赖。
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name: "tree_browse", Description: "浏览目录：列出直接子项的一行摘要（L0），用于定位下钻方向",
		SideEffect: agent.SideReadonly, ConcurrentSafe: true,
		Execute: func(input string) (string, error) {
			p, err := parseTreePath(input)
			if err != nil {
				return "", err
			}
			nodes, err := idx.Browse(p)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, n := range nodes {
				b.WriteString(n.L0 + "\n")
			}
			return b.String(), nil
		},
	})
	reg.Register(agent.Tool{
		Name: "tree_overview", Description: "查看节点概览（L1）：目录=子项清单，文件=头部+行数，用于确认是否继续下钻",
		SideEffect: agent.SideReadonly, ConcurrentSafe: true,
		Execute: func(input string) (string, error) {
			p, err := parseTreePath(input)
			if err != nil {
				return "", err
			}
			return idx.Overview(p)
		},
	})
	reg.Register(agent.Tool{
		Name: "read_file", Description: "读取文件完整内容（L2，带行号），用于最终定位与引用",
		SideEffect: agent.SideReadonly, ConcurrentSafe: true,
		Execute: func(input string) (string, error) {
			p, err := parseTreePath(input)
			if err != nil {
				return "", err
			}
			return idx.Read(p)
		},
	})

	// 模型沿树推理的路径（MockLLM 脚本化决策；工具真实执行）：
	// 看目录 → 下钻 internal/agent → 翻到 loop.go → 扫内容确认 RunWithContext 定义行
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "tree_browse", ToolInput: `{"path":"."}`},
		{ToolName: "tree_browse", ToolInput: `{"path":"internal"}`},
		{ToolName: "tree_browse", ToolInput: `{"path":"internal/agent"}`},
		{ToolName: "tree_overview", ToolInput: `{"path":"internal/agent/loop.go"}`},
		{ToolName: "read_file", ToolInput: `{"path":"internal/agent/loop.go"}`},
		{Content: "定位完成。"},
	})
	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(8), agent.WithVerbose(verbose))
	showResult(a.Run("在代码库里找到 RunWithContext 函数定义在哪个文件哪一行"))

	// 展示"引用可追溯"：从模型实际读取过的全文里提取 RunWithContext 的真实行号
	// （read_file 返回带行号，直接 grep——演示 file:line 证据可复核）。
	content, err := idx.Read("internal/agent/loop.go")
	if err == nil {
		for _, line := range strings.Split(content, "\n") {
			if strings.Contains(line, "func (a *Agent) RunWithContext") {
				fields := strings.SplitN(line, ":", 2)
				fmt.Printf("—— 引用可追溯：RunWithContext 定义于 internal/agent/loop.go:%s\n",
					strings.TrimSpace(fields[0]))
				break
			}
		}
	}

	// 展示"成本只随命中节点增长"：索引规模 vs 检索路径实际读取
	fmt.Printf("—— 成本对比：树索引 %d 个节点 / 全量 %d 行；本次检索只读 1 个文件全文（loop.go），browse/overview 均只读摘要\n",
		idx.Nodes(), idx.TotalLines())
}

// parseTreePath 解析检索工具入参（{"path":"..."} 或直接路径字符串）。
func parseTreePath(input string) (string, error) {
	var req struct {
		Path string `json:"path"`
	}
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "{") {
		if err := json.Unmarshal([]byte(input), &req); err != nil {
			return "", fmt.Errorf("参数无效：应为 {\"path\":\"相对路径\"}，got %s", input)
		}
	} else {
		req.Path = strings.Trim(input, `"`)
	}
	if req.Path == "" {
		req.Path = "."
	}
	return req.Path, nil
}

// runDemoMemoryRecall 演示第 5 步③ · 记忆分层 + 关键信息优先召回：
//   - 会话1（s1）：完成任务（含一次写入动作）→ Run 结束用组合提取器
//     DefaultExtractor 沉淀两类关键记忆：最终回答 → conclusion；
//     账本中写动作 → decision（事件信号，改进一）——memories.json 落盘；
//   - 会话2（s2）：**新 Agent 实例、不带完整历史** → Run 开始按任务召回
//     相关记忆条目注入 system（【记忆召回】）→ 模型直接作答；
//     命中条目回写 UsageCount（反馈闭环，改进二）。
//
// 与阶段一（-demo session）对照：整历史恢复 vs 关键记忆召回——
// 召回成本与历史长度无关（只召回 limit 条），token 更省。
func runDemoMemoryRecall(verbose bool) {
	store, err := memory.NewMemoryStore("memory")
	if err != nil {
		fmt.Println("长期记忆存储初始化失败:", err)
		return
	}
	ex := memory.NewDefaultExtractor()

	readGoMod := agent.Tool{
		Name: "read_file", Description: "读取文件内容", SideEffect: agent.SideReadonly,
		Execute: func(input string) (string, error) {
			return "module mine-harness\n\ngo 1.27.1\n\nrequire (\n\tgithub.com/go-rod/rod v0.116.2 // indirect\n)", nil
		},
	}
	writeGoMod := agent.Tool{
		Name: "write_file", Description: "写入文件", SideEffect: agent.SideIrreversible,
		Execute: func(input string) (string, error) {
			return "已写入 memory/demo-go-version.md", nil
		},
	}

	// 会话1：调研 → 写结论文件 → 沉淀 conclusion + decision 两类记忆
	{
		reg := agent.NewRegistry()
		reg.Register(readGoMod)
		reg.Register(writeGoMod)
		llm := agent.NewMockLLM([]agent.MockDecision{
			{ToolName: "read_file", ToolInput: `{"path":"go.mod"}`},
			{ToolName: "write_file", ToolInput: `{"path":"memory/demo-go-version.md","content":"Go 1.27.1"}`},
			{Content: "结论：mine-harness 使用 Go 1.27.1，主要依赖 go-rod（间接依赖），已写入结论文件。"},
		})
		a := agent.NewAgent(llm, reg,
			agent.WithMaxSteps(5), agent.WithVerbose(verbose),
			agent.WithMemoryStore(store), agent.WithMemoryExtractor(ex),
			agent.WithSessionID("s1"))
		fmt.Println("===== 会话1（s1）：调研 + 写文件，沉淀结论与决策 =====")
		showResult(a.Run("读取 go.mod，确认 Go 版本，并把结论写入 memory/demo-go-version.md"))
		fmt.Printf("—— 长期记忆已沉淀 %d 条：\n", len(store.List()))
		for _, e := range store.List() {
			fmt.Printf("  [%s] %s（来源: %s）\n", e.Category, e.Content, e.Source)
		}
		fmt.Println()
	}

	// 会话2：新实例、不带完整历史，仅靠记忆召回作答
	{
		reg := agent.NewRegistry()
		reg.Register(readGoMod)
		reg.Register(writeGoMod)
		llm := agent.NewMockLLM([]agent.MockDecision{
			{Content: "根据【记忆召回】直接汇报：Go 1.27.1，结论文件已写入，无需重复探索。"},
		})
		a := agent.NewAgent(llm, reg,
			agent.WithMaxSteps(3), agent.WithVerbose(verbose),
			agent.WithMemoryStore(store), agent.WithMemoryExtractor(ex),
			agent.WithSessionID("s2"))
		fmt.Println("===== 会话2（s2）：新实例，仅召回关键记忆作答 =====")
		showResult(a.Run("上次调研的 Go 版本结论是什么？直接汇报"))
		recalled, _ := store.Recall("Go", 3)
		// 反馈闭环：本次命中（Go）条目 UsageCount 已回写；元回答沉淀与否
		// 看提取器实现（DefaultExtractor 仍如实提取所有最终回答，
		// 低价值记忆靠召回排序衰减，可换 LLM 版提取器过滤）。
		fmt.Printf("—— 成本对比：会话2 未恢复整段历史（阶段一 5 条消息），只召回 %d 条记忆条目注入上下文\n", len(recalled))
	}
}

// runDemoParallel 演示多 tool_call 并发执行：
// 模型一轮返回 3 个工具调用，工具都声明 ConcurrentSafe=true → 内核用
// goroutine 并行执行（trace 打印"⚡ 并行执行"），3 个 400ms 的调用
// 并行约 0.4s 完成（顺序要 1.2s）。执行失败/审批仍按原护栏逐条独立处理。
func runDemoParallel(verbose bool) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:           "get_weather",
		Description:    "查询指定城市的当前天气",
		BaseRisk:       agent.RiskNone,
		ConcurrentSafe: true, // 无共享状态，可安全并行
		Execute: func(input string) (string, error) {
			time.Sleep(400 * time.Millisecond) // 模拟外部 API 耗时，体现并行收益
			city := "北京"
			for _, c := range []string{"上海", "杭州", "广州"} {
				if strings.Contains(input, c) {
					city = c
					break
				}
			}
			return fmt.Sprintf(`{"city":"%s","weather":"晴","temp":24}`, city), nil
		},
	})

	// 一轮并行查 3 个城市（多 tool_call），下一轮汇总
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolCalls: []agent.ToolCall{
			{ID: "c1", Name: "get_weather", Input: `{"city":"北京"}`},
			{ID: "c2", Name: "get_weather", Input: `{"city":"上海"}`},
			{ID: "c3", Name: "get_weather", Input: `{"city":"杭州"}`},
		}},
		{Content: "三城天气已汇总：北京晴、上海晴、杭州晴，都是 24 度。"},
	})

	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(4), agent.WithVerbose(verbose))
	showResult(a.Run("同时查北京、上海、杭州三城天气并汇总"))
}

// runDemoLLMCompact 演示生成式 LLM 摘要器：
// 主模型用 MockLLM（脚本化，不依赖 API Key），摘要器换成真实豆包——
// 上下文超预算时，把旧消息交给豆包写摘要（而非启发式截断拼接），
// 历史里出现"【历史摘要】+ 豆包生成的浓缩文本"。
// 需要 ARK_API_KEY（复用豆包接入）；生产里摘要器应配独立小模型。
func runDemoLLMCompact(verbose bool) {
	loadEnv(".env")
	key := os.Getenv("ARK_API_KEY")
	if key == "" {
		fmt.Println("缺少 ARK_API_KEY：export ARK_API_KEY=xxx 或写入项目根 .env（已被 git 忽略）")
		return
	}

	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气（返回详细预报）",
		BaseRisk:    agent.RiskNone,
		Execute: func(input string) (string, error) {
			city := "未知"
			for _, c := range []string{"北京", "上海", "杭州", "广州", "深圳"} {
				if strings.Contains(input, c) {
					city = c
					break
				}
			}
			// 模拟较长的工具结果（如 API 返回的完整天气 JSON），让历史快速膨胀
			return fmt.Sprintf(`{"city":"%s","weather":"晴转多云","temp":22,"wind":"3级东南风","humidity":45,"aqi":72,"summary":"%s今天白天晴转多云，午后体感舒适，适合户外活动，夜间最低温18度，早晨有轻雾能见度一般，出行注意安全，建议添一件薄外套，明天转为多云。"}`, city, city), nil
		},
	})

	// 主模型脚本：连续查 6 个城市（参数在变，不触发循环检测），历史逐步膨胀
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "get_weather", ToolInput: `{"city":"北京"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"上海"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"杭州"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"广州"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"深圳"}`},
		{ToolName: "get_weather", ToolInput: `{"city":"成都"}`},
		{Content: "六城天气已汇总：北京晴、上海多云、杭州有雨、广州晴热、深圳阵雨、成都阴。"},
	})

	// 摘要器：独立豆包实例（与主模型分离，生产应换小模型）。
	// DisableThinking=true：摘要是"窄任务"，不需要深度思考——豆包 2.x 的
	// 思考链（COT）计入输出 token（单价是输入的 5 倍），关掉后输出成本
	// 降 70~90%（thinking 参数非通用，仅豆包/智谱系列支持，见 provider.go）。
	// MaxTokens 必须给足（2048）：混合思考模型 max_tokens 太小会让 reasoning
	// 吃光预算、content 为空 → 摘要失败回退 FIFO（README 有完整踩坑记录）。
	// Verbose=true：打印每次摘要请求的服务端 usage（真实消耗可见）。
	model := os.Getenv("DOUBAO_MODEL")
	if model == "" {
		model = "doubao-seed-2-1-lite-260915"
	}
	summLLM := agent.NewOpenAICompatibleProvider(
		"https://ark.cn-beijing.volces.com/api/v3",
		model,
		key,
	)
	summLLM.MaxTokens = 2048
	summLLM.DisableThinking = true // 窄任务：关思考，省输出 token
	summLLM.Verbose = verbose      // 打印服务端 usage（prompt/completion/total）

	// 预算 650：历史积累到第 6 步超预算（约 700）→ 触发 1 次生成式压缩（1 次豆包调用）。
	// 注意：预算若设太小会让 CompactContext 反复压缩、每次调一次摘要模型；
	// 且生成式摘要输出长度不可控——内核有"摘要不比原文小就回退 FIFO"的
	// 收敛护栏（context.go），不会因长摘要死循环。
	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(8),
		agent.WithTokenBudget(650),
		agent.WithSummarizer(agent.NewLLMSummarizer(summLLM)),
		agent.WithVerbose(verbose))
	res := a.Run("分别查北京、上海、杭州、广州、深圳、成都的天气并汇总")
	// 展示生成式摘要的实际产出（豆包写的浓缩文本）
	for _, m := range a.History() {
		if strings.HasPrefix(m.Content, "【历史摘要】") {
			fmt.Printf("\n--- 豆包生成的摘要 ---\n%s\n", m.Content)
		}
	}
	showResult(res)
}

// showResult 统一打印 RunResult（completed / aborted 都按"结果"展示，不当作异常）。
// runDemoFS 演示第 1 步真实文件工具集（internal/fsagent）：
// 在临时"项目"里，mock 模型完成「看目录 → 读文件 → 改文件 → grep 验证」链路。
// 展示点：
//   - read_file / glob / grep：RiskNone 自动放行，不打扰用户
//   - edit_file：RiskMedium 达默认审批阈值 → 走审批闸门（这里模拟人工放行）
//   - 写前自动备份：backup/ 下保留旧内容，可回滚
func runDemoFS(verbose bool) {
	root, err := os.MkdirTemp("", "mine-harness-fs-*")
	if err != nil {
		fmt.Println("创建临时目录失败:", err)
		return
	}
	defer os.RemoveAll(root) // 演示完清理临时项目

	// 造一个迷你"项目"
	if err := os.WriteFile(filepath.Join(root, "hello.go"),
		[]byte("package main\n\n// TODO: add greeting\nfunc main() {\n}\n"), 0o644); err != nil {
		fmt.Println("写临时文件失败:", err)
		return
	}
	if err := os.MkdirAll(filepath.Join(root, "util"), 0o755); err != nil {
		fmt.Println("建目录失败:", err)
		return
	}
	if err := os.WriteFile(filepath.Join(root, "util", "helper.go"),
		[]byte("package util\n\nfunc Helper() string { return \"hi\" }\n"), 0o644); err != nil {
		fmt.Println("写临时文件失败:", err)
		return
	}

	reg := agent.NewRegistry()
	for _, t := range fsagent.NewTools(root).All() {
		reg.Register(t)
	}

	// 脚本：先看目录 → 读 hello.go → 替换 TODO → grep 验证
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "glob", ToolInput: `{"pattern":"**/*.go"}`},
		{ToolName: "read_file", ToolInput: `{"path":"hello.go"}`},
		{ToolName: "edit_file", ToolInput: `{"path":"hello.go","old":"// TODO: add greeting","new":"// greeting added"}`},
		{ToolName: "grep", ToolInput: `{"pattern":"greeting"}`},
		{Content: "已完成：hello.go 的 TODO 已替换为 greeting added，并通过 grep 验证。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(6),
		agent.WithVerbose(verbose),
		// edit_file/write_file 是 RiskMedium，达默认审批阈值——配一个
		// "模拟人工"审批器放行（真实使用换 CLIApprover 读键盘，或
		// LLM 决策器；这里演示审批闸门确实被触发）
		agent.WithApprover(agent.ApproverFunc(func(toolName, args string) bool {
			fmt.Printf("[审批] %s(%s) → 放行（演示用 Auto 放行）\n", toolName, args)
			return true
		})),
	)
	showResult(a.Run("看一下项目里有哪些 Go 文件，读 hello.go 并把 TODO 替换掉，最后验证一下"))

	// 展示备份产物：编辑前的原文件躺在 root/backup/ 下，可回滚
	fmt.Println("\n--- 备份产物（可回滚）---")
	baks, _ := filepath.Glob(filepath.Join(root, "backup", "*"))
	for _, b := range baks {
		rel, _ := filepath.Rel(root, b)
		data, _ := os.ReadFile(b)
		fmt.Printf("%s:\n%s\n", filepath.ToSlash(rel), strings.TrimRight(string(data), "\n"))
	}
}

// runDemoCMD 演示第 2 步命令执行工具（internal/cmdagent）：
// 在**真实仓库根**执行命令，展示参数感知风险的完整链路：
//   - go vet / git status：白名单 → 降 RiskLow → 自动放行（不打扰）
//   - rm -rf：危险模式 → 升 RiskHigh → 强制审批（这里模拟人工拒绝）
//
// 命令真实执行（go vet 真的跑、git status 真的读），危险命令被拒不会执行。
func runDemoCMD(verbose bool) {
	reg := agent.NewRegistry()
	// 命令在仓库根（"."）运行——真实场景，而非临时目录
	for _, t := range cmdagent.NewTools(".").All() {
		reg.Register(t)
	}

	// 参数感知风险：run_cmd 声明 RiskMedium，由 RiskOf 按命令内容动态修正——
	// 同一工具，不同命令风险天差地别（go test 无害、rm -rf 致命）
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "run_cmd", ToolInput: `{"cmd":"go vet ./internal/fsagent/"}`},
		{ToolName: "run_cmd", ToolInput: `{"cmd":"git status --short"}`},
		{ToolName: "run_cmd", ToolInput: `{"cmd":"rm -rf backup"}`}, // 危险 → 审批拒绝
		{Content: "已完成：go vet 通过、git 状态已查看；rm -rf 属危险操作被审批拒绝，未执行。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(5),
		agent.WithVerbose(verbose),
		agent.WithRiskEvaluator(agent.FuncRiskEvaluator(func(toolName, args string, base agent.RiskLevel) agent.RiskLevel {
			if toolName == "run_cmd" {
				return cmdagent.RiskOfArgs(args)
			}
			return base
		})),
		// 阈值默认 RiskMedium：go vet / git status 已被 RiskOf 降为 RiskLow → 自动放行；
		// 只有 rm -rf（RiskHigh）会走到这里——demo 模拟人工拒绝
		agent.WithApprover(agent.ApproverFunc(func(toolName, args string) bool {
			fmt.Printf("[审批] %s(%s) → 拒绝（危险操作，模拟人工否决）\n", toolName, args)
			return false
		})),
	)
	showResult(a.Run("先跑 go vet 检查 fsagent 包，再看 git 状态，最后删掉 backup 目录"))
}

// runDemoFigma 演示第 3 步 Figma 接入（internal/figmaagent）：
// 真实读取 Figma 设计稿（fileKey 从 .env 的 FIGMA_FILE_KEY 读取，不写死在代码里）——
// 画布结构 → 目标页面节点详情 → 导出 PNG。
// token 从 .env 的 FIGMA_TOKEN 读取（同 ARK_API_KEY 模式，.env 已被 git 忽略）。
// 风险分级演示：get_node/get_variables 纯读（RiskNone）自动放行；
// figma_export_image 为 RiskMedium，执行前进入人工审批闸门（CLIApprover 问 y/n）——
// 这就是"导图任务交由用户评审"在内核审批点的落地：拒绝则 fail-closed 不导出。
func runDemoFigma(verbose bool) {
	loadEnv(".env")
	token := os.Getenv("FIGMA_TOKEN")
	if token == "" {
		fmt.Println("缺少 FIGMA_TOKEN：export FIGMA_TOKEN=xxx 或写入项目根 .env（已被 git 忽略）")
		return
	}

	reg := agent.NewRegistry()
	// fileKey 取自用户提供的链接：figma.com/design/<fileKey>/<标题>，从 .env 读取避免泄露
	fileKey := os.Getenv("FIGMA_FILE_KEY")
	if fileKey == "" {
		fmt.Println("缺少 FIGMA_FILE_KEY：写入项目根 .env（已被 git 忽略）")
		return
	}
	for _, t := range figmaagent.NewTools(fileKey, token).All() {
		reg.Register(t)
	}

	// 脚本：先看画布结构 → 深入"产品目录页" → 导出页面 PNG。
	// 注：figma_get_variables 端点在 Free 计划 token 下返回 403（缺
	// file_variables:read scope，实测确认），故成功闭环里不放该步骤；
	// 颜色规范可从 get_node 的 fills 提取（见输出中的"填充=#RRGGBB"）。
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "figma_get_node", ToolInput: `{"ids":"0:1"}`},
		{ToolName: "figma_get_node", ToolInput: `{"ids":"52:3253","max_depth":2}`},
		{ToolName: "figma_export_image", ToolInput: `{"ids":"52:3253","format":"png"}`},
		{Content: "已完成：读取画布结构、产品目录页节点树（含填充颜色规范），并导出页面 PNG。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(5),
		agent.WithVerbose(verbose),
		// 导图人工评审：export_image 达审批阈值后由 CLIApprover 问询（y 放行）。
		// 拒绝 → fail-closed 不执行，模型收到"未授权"回填并停止该动作；
		// 生产环境可换任意 Approver 实现（自动策略/远端审批）。
		agent.WithApprover(agent.NewCLIApprover()),
	)
	showResult(a.Run("读取设计稿：先看画布结构，再深入产品目录页，最后导出页面 PNG"))
}

// runDemoPlan 演示"自动决策管线"（Figma 协作的第 3.5 步：自动决策 + 评审升级）。
//
// 用户目标："后续调用 mine-harness 做前端页面开发时，能自动决策，不懂的/行不通的
// 再让我评审"。本 demo 把这条原则落成两步工具链：
//
//	figma_get_node（结构总览）→ figma_plan_page（自动决策：位图资产 / CSS token / 评审点）。
//
// 自动决策的部分：分类（IMAGE→导出、文本/矢量→CSS）、token 提取、计划编排——
// 全部纯读 RiskNone，审批闸自动放行，不需要打扰用户。
// 升级评审的部分：导出审批（RiskMedium）、REST 429 限流→浏览器接管、数据不足——
// 显式列入 plan_page 输出的"评审点"，由 CLIApprover 问询用户决策。
//
// JS/TS ↔ Go 对比：TS 里这种"先自动、兜底人工"的管线常用状态机/编排框架；
// Go 用工具 + 内核审批点表达同样的语义——自动路径靠 RiskNone 静默通过，
// 评审路径靠 BaseRisk 声明触发闸门，两条路径在同一内核里显式分叉。
func runDemoPlan(verbose bool) {
	loadEnv(".env")
	token := os.Getenv("FIGMA_TOKEN")
	if token == "" {
		fmt.Println("缺少 FIGMA_TOKEN：export FIGMA_TOKEN=xxx 或写入项目根 .env（已被 git 忽略）")
		return
	}
	fileKey := os.Getenv("FIGMA_FILE_KEY")
	if fileKey == "" {
		fmt.Println("缺少 FIGMA_FILE_KEY：写入项目根 .env（已被 git 忽略）")
		return
	}
	reg := agent.NewRegistry()
	for _, t := range figmaagent.NewTools(fileKey, token).All() {
		reg.Register(t)
	}

	// 脚本：结构总览 → 对"活动主页面"（104:17791，本文件真实页面节点）生成还原计划。
	// plan_page 纯读自动放行；输出里的评审点（位图导出）留给 CLIApprover 决策。
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "figma_get_node", ToolInput: `{"ids":"0:1"}`},
		{ToolName: "figma_plan_page", ToolInput: `{"node_id":"104:17791","max_depth":4}`},
		{Content: "已完成：自动决策计划已生成——位图资产清单（导出 PNG）+ CSS token（还原用）+ 评审点（导出方式需你确认）。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(4),
		agent.WithVerbose(verbose),
		agent.WithApprover(agent.NewCLIApprover()),
	)
	showResult(a.Run("读取设计稿结构，并自动生成还原计划：能自动决策的全自动，行不通的列入评审点交给用户"))
}

// demoWebTemplate 是"生成代码"产物的模板版页面（可安全提交 GitHub：
// 品牌宣传月 / 示例品牌 · 能力支持 / 示例产品 A-E / 分类一·二，无公司信息）。
// 本地真实版设计稿还原存于 webdemo/design.html（git 忽略，runDemoWeb 优先读取）；
// 模板经 //go:embed 在编译期打进二进制，用于无本地文件时兜底——单文件分发
// 也自带资源。第 4 步"模型生成代码"由 fsagent.write_file 驱动（PLAN 阶段 A），
// 本 demo 聚焦阶段 B 的 serve_dir + screenshot 自动化闭环。
//
// JS/TS ↔ Go 差异：TS 里模板资源通常靠 bundler（webpack raw-loader / Vite ?raw）
// 内联成字符串，或运行时 fs.readFile；Go 用 //go:embed 编译期打包进二进制——
// 运行时不依赖文件系统，二进制自包含。代价是改动模板文件必须重新编译。
//
//go:embed assets/demo_template.html
var demoWebTemplate string

// runDemoWeb 演示第 3 步前端场景能力闭环（阶段 B）：
//
//	Figma 设计稿 → 生成 HTML → serve_dir 起本地预览 → 无头 Chrome 截屏落盘。
//
// 链路真实执行：设计稿来自真实 API，预览服务真实可访问，截屏是真实渲染产物
// （webdemo/shot.png）。第 4 步"模型生成代码"在本 demo 以预置 HTML 代替
// （fsagent.write_file 已具备该能力，见 fs 场景）。
func runDemoWeb(verbose bool) {
	loadEnv(".env")
	token := os.Getenv("FIGMA_TOKEN")

	// 生成代码（阶段 A 能力）：把设计稿还原成静态页，落盘到 webdemo/
	webDir := filepath.Join(".", "webdemo")
	if err := os.MkdirAll(webDir, 0o755); err != nil {
		fmt.Println("建 webdemo 目录失败:", err)
		return
	}
	// 本地真实版优先：webdemo/design.html 由脱敏流程落盘（git 忽略），
	// 不存在时回退到内置模板 demoWebTemplate（assets/demo_template.html 经
	// //go:embed 打进二进制，可安全提交 GitHub、任何环境可跑通）。
	pageHTML := demoWebTemplate
	if raw, err := os.ReadFile(filepath.Join(webDir, "design.html")); err == nil {
		pageHTML = string(raw)
		fmt.Println("使用本地真实版 design.html（未提交，仅本地）")
	} else {
		fmt.Println("未找到 design.html，使用内置模板 demoWebTemplate（可提交）")
	}
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte(pageHTML), 0o644); err != nil {
		fmt.Println("写 index.html 失败:", err)
		return
	}
	fmt.Println("已生成页面: webdemo/index.html")

	reg := agent.NewRegistry()
	if fileKey := os.Getenv("FIGMA_FILE_KEY"); token != "" && fileKey != "" {
		for _, t := range figmaagent.NewTools(fileKey, token).All() {
			reg.Register(t)
		}
	}
	for _, t := range webagent.NewTools(".").All() {
		reg.Register(t)
	}

	// 脚本：读设计稿结构（可选）→ 起预览 → 截屏回看。
	// serve 固定端口 8321（占用会报错提示）；screenshot 375 宽模拟移动端。
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "figma_get_node", ToolInput: `{"ids":"104:17791","max_depth":1}`},
		{ToolName: "serve_dir", ToolInput: `{"dir":"webdemo","port":8321}`},
		{ToolName: "screenshot", ToolInput: `{"url":"http://127.0.0.1:8321/","out":"webdemo/shot.png","width":375}`},
		{Content: "已完成：读取设计稿结构、起本地预览服务，并将还原页面截屏保存为 webdemo/shot.png。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(5),
		agent.WithVerbose(verbose),
	)
	showResult(a.Run("读取活动主页面设计稿结构，起本地预览服务，对还原的页面截图"))
}

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

// runDemoStack 演示 detect_tech_stack：对工程上下文感知技术栈。
// 边界提示写在输出里：mine-harness 自身是 Go 项目（无前端信号），
// 演示的是"无前端信号 → 需评审"路径；对任何前端工程 root 运行时，
// react/vue/纯静态会被自动识别，冲突时才升级评审。
func runDemoStack(verbose bool) {
	fs := fsagent.NewTools(".")
	reg := agent.NewRegistry()
	for _, t := range fs.All() {
		reg.Register(t)
	}
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "detect_tech_stack", ToolInput: `{}`},
		{Content: "已完成：已感知当前工程上下文的技术栈信号（或列出冲突交由你评审）。"},
	})
	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(2), agent.WithVerbose(verbose))
	showResult(a.Run("感知当前工程的技术栈，判断前端代码生成目标（html / react / 其他）"))
}

// buildFullRegistry 组装 MCP 对外暴露的完整工具集：
// 文件（fsagent）+ 命令（cmdagent）+ 前端场景（webagent）+ Figma（figmaagent）。
// 覆盖 PLAN 第 4 步目标"第 3 步前端能力优先以 MCP 形式注册——一套实现，
// harness 自用 + 外部 Agent 共用"。
//
// Figma 工具需要 token：从 .env 读取 FIGMA_TOKEN / FIGMA_FILE_KEY，
// 缺失时跳过注册并提示（MCP client 的 tools/list 里就没有 figma_* 工具）。
func buildFullRegistry(verbose bool) *agent.Registry {
	reg := agent.NewRegistry()
	for _, t := range fsagent.NewTools(".").All() {
		reg.Register(t)
	}
	for _, t := range cmdagent.NewTools(".").All() {
		reg.Register(t)
	}
	for _, t := range webagent.NewTools(".").All() {
		reg.Register(t)
	}
	loadEnv(".env")
	fileKey := os.Getenv("FIGMA_FILE_KEY")
	token := os.Getenv("FIGMA_TOKEN")
	if fileKey == "" || token == "" {
		if verbose {
			fmt.Println("[mcp] 未配置 FIGMA_FILE_KEY / FIGMA_TOKEN（写入项目根 .env），跳过 Figma 工具注册")
		}
		return reg
	}
	for _, t := range figmaagent.NewTools(fileKey, token).All() {
		reg.Register(t)
	}
	return reg
}

// runDemoMCP 演示 PLAN 第 4 步 MCP 适配层——MCP Server（对外暴露）：
// 把 harness 注册的真实工具（文件/命令/前端预览/Figma）以 MCP 工具形式暴露，
// 由"外部 MCP client"（脚本化的 JSON-RPC 请求）调用。
//
// 演示链路：
//
//	initialize（握手）→ tools/list（全量工具清单，含 risk 风险透出）
//	→ tools/call read_file 读真实文件（RiskNone 直接放行）
//	→ tools/call serve_dir 起本地预览（前端工具 MCP 可用：起服务→返回端口）
//	→ tools/call write_file（RiskMedium → 注入的审批函数拦截，isError）
//
// JS/TS ↔ Go 对比：TS 的 Node 生态有官方 @modelcontextprotocol/sdk；
// Go 手写 JSON-RPC 2.0 + stdio 传输（本 demo 的 mcpserver 包）——
// 协议只有 tools/list + tools/call 两个核心方法，理解传输层比引 SDK 更重要。
func runDemoMCP(verbose bool) {
	reg := buildFullRegistry(verbose)

	// 脚本化"外部 MCP client"：模拟 Codex/Claude Code 对 harness 的调用序列
	script := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"external-agent","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"go.mod"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"serve_dir","arguments":{"dir":"webdemo"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"tmp-demo.txt","content":"mcp demo"}}}`,
	}, "\n")

	out := &bytes.Buffer{}
	srv := mcpserver.New(reg, strings.NewReader(script), out)
	// 注入审批：写类工具（write_file/edit_file/figma_export_image）走人工评审——
	// 演示"外部 Agent 也要过 harness 的审批闸门"（MCP 暴露不等于放开风险工具）
	srv.SetApprover(func(toolName, args string) bool {
		if toolName == "write_file" || toolName == "edit_file" || toolName == "figma_export_image" {
			fmt.Printf("[审批] %s(%s) → 拒绝（演示：写类工具需人工评审，外部 Agent 触发审批闸门）\n", toolName, args)
			return false
		}
		return true
	})
	if verbose {
		fmt.Println("[mcp] 外部 MCP client 连接 harness（JSON-RPC 2.0 over stdio，工具集 = 文件+命令+前端+Figma）")
	}
	if err := srv.Run(); err != nil {
		fmt.Println("MCP server 运行失败:", err)
		return
	}

	// 逐行打印协议响应（缩进 JSON，便于阅读）
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(line), "  ", "  "); err != nil {
			fmt.Println(line)
			continue
		}
		fmt.Println(pretty.String())
	}
}

// runDemoMCPClient 演示 PLAN 第 4 步 MCP 适配层——MCP Client（对内接入）：
// harness 作为 client 连接外部 MCP server，把外部工具发现并注册进主循环，
// 由模型按普通工具调用——与 Server 方向互为镜像：
// Server = 外部 Agent 用 harness 的能力；Client = harness 用外部服务的能力。
//
// 演示链路（net.Pipe 全内存互通，对端就是 harness 自己的 MCP Server，
// 即"双向角色"的直观验证：同一套协议，Server/Client 各持一端）：
//
//	client initialize（握手）→ tools/list（发现外部工具，含 risk 透出）
//	→ 注册进主循环 Registry → MockLLM 驱动调外部 read_file 读真实文件
//	→ 最终回答
//
// 生产形态用 NewStdio 起外部 server 子进程（浏览器/Figma/数据库等第三方
// MCP server）；demo 用内存互通，零外部依赖、可提交可复现。
//
// JS/TS ↔ Go 对比：TS 的 MCP 生态有官方 SDK（@modelcontextprotocol/sdk）；
// 这里与 Server 方向共用同一套手写 JSON-RPC 2.0 协议结构（同包复用），
// 理解"双向角色"比引 SDK 更重要。
func runDemoMCPClient(verbose bool) {
	// 对端：harness 自己的 MCP Server（文件+命令+前端，buildFullRegistry）
	srvReg := buildFullRegistry(verbose)
	srvSide, cliSide := net.Pipe()
	srv := mcpserver.New(srvReg, srvSide, srvSide)
	go func() {
		if err := srv.Run(); err != nil && verbose {
			fmt.Println("[mcp-client] MCP server 退出:", err)
		}
	}()
	defer srvSide.Close()

	cli := mcpserver.NewInMemory(cliSide, cliSide)
	defer cli.Close()
	ctx := context.Background()

	if verbose {
		fmt.Println("[mcp-client] harness 作为 MCP client 连接外部 MCP server（net.Pipe 内存互通）")
	}
	proto, err := cli.Initialize(ctx)
	if err != nil {
		fmt.Println("握手失败:", err)
		return
	}
	tools, err := cli.ListTools(ctx)
	if err != nil {
		fmt.Println("工具发现失败:", err)
		return
	}
	fmt.Printf("tools/list → 发现 %d 个外部工具（protocolVersion=%s）\n", len(tools), proto)
	for _, t := range tools {
		fmt.Printf("  - %s（风险 %s）\n", t.Name, riskLabel(t.BaseRisk))
	}

	// 外部工具注册进主循环：对模型来说就是普通工具，同样过循环检测、
	// 审批闸门、重试预算——机制归内核，工具归应用层（外部服务也不例外）。
	reg := agent.NewRegistry()
	for _, t := range tools {
		reg.Register(t)
	}

	// MockLLM 驱动主循环：调外部 read_file 读真实文件 → 最终回答。
	// 链路：模型 → Registry → Client.Call → tools/call → 外部 Server → fsagent
	mock := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "read_file", ToolInput: `{"path":"go.mod"}`},
		{Content: "已通过 MCP 客户端连接外部 server，成功读取 go.mod。"},
	})
	a := agent.NewAgent(mock, reg, agent.WithVerbose(verbose))
	showResult(a.Run("通过 MCP 客户端读取 go.mod 文件内容"))
}

// runDemoMCPHTTP 演示 PLAN 第 4 步 Streamable HTTP（SSE）传输层：
// 与 mcp-client 的 net.Pipe 内存管道不同，这里走真实 TCP + HTTP 栈——
// 即"云上 Agent 通过 URL 接入 harness"的网络形态（火山方舟 Remote MCP
// 只支持 Streamable HTTP，不认 stdio，这是对齐生产接入的关键一层）。
//
// 演示链路：
//
//	HTTP Server 起在真实端口 127.0.0.1:PORT/mcp（POST = JSON-RPC，GET = SSE）
//	→ HTTPClient 连接（initialize 握手 + initialized 通知）
//	→ tools/list 发现外部工具 → 注册进主循环
//	→ MockLLM 驱动调外部 read_file 读真实文件
//	→ server 主动 Push 一条 tools/list_changed 通知，SSE 订阅打印收到
func runDemoMCPHTTP(verbose bool) {
	srvReg := buildFullRegistry(verbose)
	srv := mcpserver.New(srvReg, nil, nil)
	hs := mcpserver.NewHTTPServer(srv)

	ln, err := net.Listen("tcp", "127.0.0.1:0") // 随机空闲端口
	if err != nil {
		fmt.Println("监听端口失败:", err)
		return
	}
	defer ln.Close()
	go func() {
		if err := http.Serve(ln, hs.Handler()); err != nil && verbose {
			fmt.Println("[mcp-http] HTTP server 退出:", err)
		}
	}()
	url := "http://" + ln.Addr().String() + "/mcp"
	if verbose {
		fmt.Println("[mcp-http] harness HTTP Server 已起:", url)
		fmt.Println("[mcp-http] harness 作为 HTTPClient 连接（Streamable HTTP：POST=JSON-RPC，GET=SSE）")
	}

	cli := mcpserver.NewHTTP(url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// SSE 订阅（goroutine）：接收 server 主动推送（tools/list_changed 等）
	go func() {
		if err := cli.Subscribe(ctx, func(msg map[string]any) {
			if m, ok := msg["method"].(string); ok {
				fmt.Println("[sse] 收到 server 推送:", m)
			}
		}); err != nil && verbose {
			fmt.Println("[mcp-http] SSE 订阅结束:", err)
		}
	}()

	proto, err := cli.Initialize(ctx)
	if err != nil {
		fmt.Println("握手失败:", err)
		return
	}
	tools, err := cli.ListTools(ctx)
	if err != nil {
		fmt.Println("工具发现失败:", err)
		return
	}
	fmt.Printf("tools/list → 发现 %d 个外部工具（protocolVersion=%s）\n", len(tools), proto)

	// 外部工具注册进主循环（与 mcp-client 相同：走循环检测/审批/重试纪律）
	reg := agent.NewRegistry()
	for _, t := range tools {
		reg.Register(t)
	}
	mock := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "read_file", ToolInput: `{"path":"go.mod"}`},
		{Content: "已通过 Streamable HTTP 连接外部 server，成功读取 go.mod。"},
	})
	a := agent.NewAgent(mock, reg, agent.WithVerbose(verbose))
	showResult(a.Run("通过 HTTP 客户端读取 go.mod 文件内容"))

	// 演示 server → client 的 SSE 主动推送（MCP 标准通知：工具清单变化）
	hs.Push(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
	time.Sleep(200 * time.Millisecond) // 等订阅 goroutine 打印
}

// riskLabel 把内核风险级别显示为中文标签（demo 打印用）。
func riskLabel(l agent.RiskLevel) string {
	switch l {
	case agent.RiskNone:
		return "none（自动放行）"
	case agent.RiskLow:
		return "low"
	case agent.RiskMedium:
		return "medium（达审批阈值）"
	case agent.RiskHigh:
		return "high（强制审批）"
	default:
		return "unknown"
	}
}

// runDemoTrace 演示增量 A · 结构化轨迹导出：
// 每次 Run 的事件流（llm / tool / approval / skill / compact / cancel）随
// RunResult.Trace 暴露，落盘为 traces/<run>.json，可回答三个监测问题：
//   - 模型为何选/不选某 skill？（agent.TraceSkill：loaded / rejected / not-found）
//   - 审批在哪一步通过/拒绝？（agent.TraceApproval：approved / denied + 风险级别）
//   - Token 与延迟多少？（agent.TraceLLM 请求级元数据：Model / Token / 延迟预留字段）
//
// 脚本（MockLLM 确定性驱动，真实 fs 工具）：
//
//	加载存在的技能（loaded）→ 加载不存在的技能（rejected）→ 读文件（tool ok）
//	→ 编辑文件被审批拒绝（approval denied）→ 写入 traces/ 放行（approval approved
//	+ tool ok）→ 读不存在文件（tool failed）→ 最终回答。
//
// 审批：edit_file/write_file 走闸门，演示拒绝与放行两条路径；读类自动放行。
func runDemoTrace(verbose bool) {
	root, err := filepath.Abs(".")
	if err != nil {
		fmt.Println("解析仓库根失败:", err)
		return
	}
	reg := agent.NewRegistry()
	for _, t := range fsagent.NewTools(root).All() {
		reg.Register(t)
	}

	// 技能发现器（B 方案入口）：常驻清单拼进 load_skill 描述，模型按清单选
	provider := newDirSkillProvider("skills")
	loadSkillTool := agent.NewLoadSkillTool()
	loadSkillTool.Description = agent.BuildLoadSkillDescription(provider.List())
	reg.Register(loadSkillTool)
	reg.Register(agent.NewReadSkillRefTool())

	// 审批器：写类拒绝（演示 denied）、写 traces/ 下放行（演示 approved）
	approver := agent.ApproverFunc(func(toolName, args string) bool {
		if toolName == "edit_file" || toolName == "delete_file" {
			fmt.Printf("[审批] %s(%s) → 拒绝（演示拒绝路径）\n", toolName, args)
			return false
		}
		if toolName == "write_file" {
			fmt.Printf("[审批] %s(%s) → 放行（演示放行路径）\n", toolName, args)
			return true
		}
		return true
	})

	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: agent.LoadSkillToolName, ToolInput: `{"name":"file-ops-policy"}`},                   // 存在 → loaded
		{ToolName: agent.LoadSkillToolName, ToolInput: `{"name":"no-such-skill"}`},                     // 不存在 → rejected
		{ToolName: "read_file", ToolInput: `{"path":"AGENTS.md"}`},                                     // 读类放行 → tool ok
		{ToolName: "edit_file", ToolInput: `{"path":"AGENTS.md","old":"x","new":"y"}`},                 // 被审批拒绝 → approval denied
		{ToolName: "write_file", ToolInput: `{"path":"traces/demo-write.txt","content":"trace demo"}`}, // 放行 → approved + ok
		{ToolName: "read_file", ToolInput: `{"path":"NO_SUCH_FILE.md"}`},                               // 失败 → tool failed
		{Content: "轨迹演示结束：可回答「为何选/不选某 skill（loaded/rejected）、审批在哪一步拒绝与放行（denied/approved）、Token 与延迟（llm 事件请求级元数据）」。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(8),
		agent.WithSkillProvider(provider),
		agent.WithModel("mock/trace-demo"), // 请求级元数据：真实 demo 传模型 ID，这里标 mock
		agent.WithApprover(approver),
		agent.WithVerbose(verbose),
	)
	// 先建 traces/：write_file 步骤要写入该目录（fsagent 原子写在目标目录内
	// 创建 temp 文件），同时它也是轨迹 JSON 的落盘目录。
	if err := os.MkdirAll("traces", 0o755); err != nil {
		fmt.Println("创建 traces/ 失败:", err)
		return
	}
	res := a.Run("演示结构化轨迹：先加载 file-ops-policy 技能，再加载一个不存在的技能，读 AGENTS.md，尝试编辑（应被拒绝），写 traces/demo-write.txt（应放行），最后读一个不存在的文件")

	// 落盘：traces/trace-<时间戳>.json（运行时产物目录，.gitignore 已忽略）
	if err := os.MkdirAll("traces", 0o755); err == nil {
		name := fmt.Sprintf("trace-%s.json", time.Now().Format("20060102-150405"))
		if b, err := json.MarshalIndent(res.Trace, "", "  "); err == nil {
			if err := os.WriteFile(filepath.Join("traces", name), b, 0o644); err == nil {
				fmt.Printf("\n轨迹已落盘: traces/%s\n", name)
			}
		}
	}

	// 可读摘要：按类型统计 + 关键决策行
	fmt.Printf("\n--- 轨迹摘要（共 %d 条事件）---\n", len(res.Trace))
	byType := map[string]int{}
	for _, ev := range res.Trace {
		byType[ev.Type]++
	}
	for _, t := range []string{agent.TraceLLM, agent.TraceTool, agent.TraceApproval, agent.TraceSkill, agent.TraceCompact, agent.TraceCancel} {
		if n := byType[t]; n > 0 {
			fmt.Printf("%-9s %2d 条\n", t, n)
		}
	}
	fmt.Println("\n关键决策点（按事件序）：")
	for _, ev := range res.Trace {
		switch ev.Type {
		case agent.TraceLLM:
			if ev.Outcome == agent.OutcomeFailed {
				fmt.Printf("  [llm]    step %d 模型调用失败：%s\n", ev.Step, ev.Detail)
			}
		case agent.TraceSkill:
			fmt.Printf("  [skill]   step %d load_skill(%s) → %s（%s）\n", ev.Step, ev.Name, ev.Outcome, ev.Detail)
		case agent.TraceApproval:
			fmt.Printf("  [approval] step %d %s(%s) → %s（%s）\n", ev.Step, ev.Name, ev.Input, ev.Outcome, ev.Detail)
		case agent.TraceTool:
			fmt.Printf("  [tool]    step %d %s → %s\n", ev.Step, ev.Name, ev.Outcome)
		case agent.TraceCompact:
			fmt.Printf("  [compact] step %d %s\n", ev.Step, ev.Detail)
		case agent.TraceCancel:
			fmt.Printf("  [cancel]  step %d %s\n", ev.Step, ev.Detail)
		}
	}
	fmt.Println("\n请求级元数据（llm 事件）：", res.Trace[0].Model, "— 真实 API 接入后 Token/延迟在此字段填充")
	showResult(res)
}

// runDemoWorkflow 演示第 6 步 · 开发工作流 Skill 端到端（fix-bug 分支）：
// 在临时迷你 Go 项目里造一个真实 bug + 失败测试，Agent 按 fix-bug Skill 流程
// 完成「加载流程规范 → 复现（go test 红）→ 定位 → 最小修改 → 验证（go test 绿）」。
// 展示点：
//   - Skill 只写"何时加载、加载什么"：load_skill fix-bug 注入流程规范，正文按需进 system
//   - 流程闭环：复现命令输出从红到绿（前后对比），不凭感觉"改好了"
//   - 复用第 1/2 步真实工具：fsagent（read/edit）+ cmdagent（run_cmd go test 白名单自动放行）
//   - 写类动作照常走审批闸门（edit_file RiskMedium → 放行）
func runDemoWorkflow(verbose bool) {
	// 临时迷你 Go 项目：Double 乘 3（应为乘 2）+ 断言失败的测试
	root, err := os.MkdirTemp("", "mine-harness-fix-*")
	if err != nil {
		fmt.Println("创建临时项目失败:", err)
		return
	}
	defer os.RemoveAll(root)
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixdemo\n\ngo 1.27.1\n"), 0o644)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\n// Double 把输入翻倍。\nfunc Double(n int) int {\n\treturn n * 3 // bug：乘 3 应为乘 2\n}\n\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif got := Double(2); got != 4 {\n\t\tt.Fatalf(\"Double(2) = %d, want 4\", got)\n\t}\n}\n"), 0o644)

	reg := agent.NewRegistry()
	for _, t := range fsagent.NewTools(root).All() {
		reg.Register(t)
	}
	for _, t := range cmdagent.NewTools(root).All() {
		reg.Register(t)
	}
	// Skill 入口：常驻清单（fix-bug / implement-feature / run-tests-fix / visual-verify）
	provider := newDirSkillProvider("skills")
	loadSkillTool := agent.NewLoadSkillTool()
	loadSkillTool.Description = agent.BuildLoadSkillDescription(provider.List())
	reg.Register(loadSkillTool)
	reg.Register(agent.NewReadSkillRefTool())

	// 写类审批放行（演示闸门照常触发）
	approver := agent.ApproverFunc(func(toolName, args string) bool {
		fmt.Printf("[审批] %s(%s) → 放行（演示 Auto 放行）\n", toolName, args)
		return true
	})

	// 脚本（fix-bug 流程，MockLLM 确定性驱动）：
	// load_skill fix-bug → go test 红（复现）→ read main.go（定位）→
	// edit_file 最小修改 → go test 绿（验证）→ 汇报
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: agent.LoadSkillToolName, ToolInput: `{"name":"fix-bug"}`},
		{ToolName: "run_cmd", ToolInput: `{"cmd":"go test ./..."}`},
		{ToolName: "read_file", ToolInput: `{"path":"main.go"}`},
		{ToolName: "edit_file", ToolInput: `{"path":"main.go","old":"return n * 3 // bug：乘 3 应为乘 2","new":"return n * 2"}`},
		{ToolName: "run_cmd", ToolInput: `{"cmd":"go test ./..."}`},
		{Content: "按 fix-bug 流程完成：复现（go test 红：Double(2)=6 期望 4）→ 定位（main.go Double 乘 3）→ 最小修改（乘 2）→ 验证（go test 绿）。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(8),
		agent.WithSkillProvider(provider),
		agent.WithModel("mock/workflow-demo"),
		// 参数感知风险：go test 白名单 → 自动放行（不打扰）；edit_file 保持
		// RiskMedium → 走审批闸门（演示"写类动作照常审批"）
		agent.WithRiskEvaluator(agent.FuncRiskEvaluator(func(toolName, args string, base agent.RiskLevel) agent.RiskLevel {
			if toolName == "run_cmd" {
				return cmdagent.RiskOfArgs(args)
			}
			return base
		})),
		agent.WithApprover(approver),
		agent.WithVerbose(verbose),
	)
	showResult(a.Run("临时项目里 Double 函数算错了，请按 fix-bug 流程修复"))
}

// runDemoVisualVerify 演示第 6 步 · 前端视觉验证 Skill 端到端：
// 临时页面（标题 nowrap 溢出移动端宽度）→ serve_dir 起服务 → screenshot 移动端截屏 →
// 视觉核对（mock 视觉模型结论：标题溢出）→ edit_file 修复 → 再截屏核对 → 停服务。
// 展示点：
//   - 截图证据驱动：每个验收项（此处=窄屏无溢出）都有截图，修复后新旧对比
//   - 复用第 3 步真实工具：serve_dir（net/http）+ screenshot（无头 Chrome）
//   - 闭环到问题清单清空，服务用完即停（不留常驻进程）
func runDemoVisualVerify(verbose bool) {
	root, err := os.MkdirTemp("", "mine-harness-vv-*")
	if err != nil {
		fmt.Println("创建临时页面失败:", err)
		return
	}
	defer os.RemoveAll(root)
	page := "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>视觉验证演示</title>" +
		"<style>body{margin:0;font-family:sans-serif}.title{font-size:48px;white-space:nowrap;padding:16px;background:#eee}</style>" +
		"</head><body><div class=\"title\">这个标题非常长而且不允许换行会溢出屏幕宽度</div></body></html>"
	os.WriteFile(filepath.Join(root, "index.html"), []byte(page), 0o644)

	reg := agent.NewRegistry()
	for _, t := range webagent.NewTools(root).All() {
		reg.Register(t)
	}
	for _, t := range fsagent.NewTools(root).All() {
		reg.Register(t)
	}

	// 写类审批放行
	approver := agent.ApproverFunc(func(toolName, args string) bool {
		fmt.Printf("[审批] %s(%s) → 放行（演示 Auto 放行）\n", toolName, args)
		return true
	})

	// 固定端口 8322（演示脚本需预知 URL；真实 Agent 用 serve_dir 返回端口拼 URL）
	const port = 8322
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolName: "serve_dir", ToolInput: fmt.Sprintf(`{"dir":".","port":%d}`, port)},
		{ToolName: "screenshot", ToolInput: fmt.Sprintf(`{"url":"http://127.0.0.1:%d/","out":"shots/page.png","width":375}`, port)},
		// 视觉核对（mock 视觉模型结论）：375 宽度下标题溢出
		{ToolName: "edit_file", ToolInput: `{"path":"index.html","old":".title{font-size:48px;white-space:nowrap;padding:16px;background:#eee}","new":".title{font-size:24px;padding:16px;background:#eee}"}`},
		{ToolName: "screenshot", ToolInput: fmt.Sprintf(`{"url":"http://127.0.0.1:%d/","out":"shots/page2.png","width":375}`, port)},
		{ToolName: "serve_dir", ToolInput: `{"action":"stop","port":8322}`},
		{Content: "视觉验证完成：起服务 → 移动端截屏发现标题溢出 → 缩小字号修复（nowrap 保留）→ 再截屏确认无溢出 → 停止服务。每个验收项都有截图证据。"},
	})

	a := agent.NewAgent(llm, reg,
		agent.WithMaxSteps(8),
		agent.WithModel("mock/visual-verify-demo"),
		agent.WithApprover(approver),
		agent.WithVerbose(verbose),
	)
	showResult(a.Run("按 visual-verify 流程验证临时页面的移动端渲染：起服务、截屏、核对、修复、再验证"))
}

// runDemoCompactFidelity 演示增量 F（压缩保真度抽查）+ 增量 G（硬约束保护层）：
//
//	A. 抽取式摘要（Heuristic）保留关键数字 → 保真度抽查通过，摘要正常插入；
//	B. 自定义"丢实体"摘要器 → 订单号/金额等关键实体全丢 → 抽查判定摘要
//	   不可信、保留原文（宁超预算不压错），TraceCompact Detail 记录介入；
//	C. WithHardConstraints 注入受保护消息 → 压缩跳过（Detail 记"跳过受保护
//	   N 条"），压缩后硬约束仍完整保留（长会话后期仍记得第一轮的禁忌）。
//
// JS/TS ↔ Go 差异：自定义摘要器实现 Summarizer 接口（方法集隐式实现，
// 对应 TS 的 implements）；RunResult.Trace 是增量 A 的产物，demo 里过滤
// TraceCompact 事件读取压缩详情，相当于 TS 里 filter(type === 'compact')。
func runDemoCompactFidelity(verbose bool) {
	fmt.Println("== 增量 F/G 演示：压缩保真度抽查 + 硬约束保护层 ==")

	newLLM := func() *agent.MockLLM {
		return agent.NewMockLLM([]agent.MockDecision{
			{ToolName: "get_order", ToolInput: `{"id":"A"}`},
			{ToolName: "get_order", ToolInput: `{"id":"B"}`},
			{ToolName: "get_order", ToolInput: `{"id":"C"}`},
			{ToolName: "get_order", ToolInput: `{"id":"D"}`},
			{Content: "四个订单已全部处理完成。"},
		})
	}
	showCompactDetail := func(r agent.RunResult, label string) {
		for _, e := range r.Trace {
			if e.Type == agent.TraceCompact {
				fmt.Printf("  [%s] TraceCompact: %s\n", label, e.Detail)
			}
		}
	}

	fmt.Println("\n--- A. 抽取式摘要（Heuristic）：关键数字保留 → 抽查通过 ---")
	{
		reg := agent.NewRegistry()
		reg.Register(agent.Tool{
			Name:        "get_order",
			Description: "查询订单详情（返回含订单号/金额/URL 等关键数字）",
			Execute: func(input string) (string, error) {
				// 关键数字（订单号/金额）靠前，尾部是长中文描述：
				// Heuristic 截断只丢尾部描述、关键实体全保留 → 抽查通过
				return `{"order":50231,"amount":4500,"note":"已处理完成等待确认，收货地址无误，发票已开具并归档，后续流程专人跟进"}`, nil
			},
		})
		a := agent.NewAgent(newLLM(), reg, agent.WithMaxSteps(8), agent.WithTokenBudget(60), agent.WithVerbose(verbose))
		r := a.Run("依次查询 4 个订单并汇总")
		showResult(r)
		showCompactDetail(r, "A")
	}

	fmt.Println("\n--- B. 丢实体摘要器：关键实体全丢 → 保留原文（宁超预算不压错）---")
	{
		reg := agent.NewRegistry()
		reg.Register(agent.Tool{
			Name:        "get_order",
			Description: "查询订单详情",
			Execute: func(input string) (string, error) {
				return `{"order":50231,"amount":4500,"note":"已处理完成等待确认，收货地址无误，金额已核对并归档"}`, nil
			},
		})
		// 模拟生成式摘要归纳过头：把订单号/金额全部丢掉
		a := agent.NewAgent(newLLM(), reg,
			agent.WithMaxSteps(8), agent.WithTokenBudget(60),
			agent.WithSummarizer(demoKeyDropSummarizer{}),
			agent.WithVerbose(verbose),
		)
		r := a.Run("依次查询 4 个订单并汇总")
		showResult(r)
		showCompactDetail(r, "B")
	}

	fmt.Println("\n--- C. WithHardConstraints：硬约束不参与压缩，压缩后仍完整保留 ---")
	{
		reg := agent.NewRegistry()
		reg.Register(agent.Tool{
			Name:        "get_order",
			Description: "查询订单详情",
			Execute: func(input string) (string, error) {
				return `{"order":50231,"amount":4500,"note":"已处理完成等待确认，收货地址无误，金额已核对并归档"}`, nil
			},
		})
		a := agent.NewAgent(newLLM(), reg,
			agent.WithMaxSteps(8), agent.WithTokenBudget(60),
			agent.WithHardConstraints([]string{"金额超过 5000 需人工确认后再执行", "禁止删除任何文件"}),
			agent.WithVerbose(verbose),
		)
		r := a.Run("依次查询 4 个订单并汇总")
		showResult(r)
		showCompactDetail(r, "C")
		// 硬约束压缩后仍在历史里（受保护消息不被压缩/FIFO 触碰）
		kept := false
		for _, m := range a.History() {
			if m.Protected && strings.Contains(m.Content, "金额超过 5000") {
				kept = true
			}
		}
		if kept {
			fmt.Println("  [C] 硬约束消息压缩后仍完整保留 ✓（长会话后期仍记得第一轮的禁忌）")
		} else {
			fmt.Println("  [C] 硬约束消息丢失 ✗")
		}
	}
}

// demoKeyDropSummarizer 演示用"丢实体"摘要器：模拟生成式摘要改写/归纳过头、
// 把订单号/金额等关键数字全部丢掉（对比 HeuristicSummarizer 抽取式必保留原文）。
type demoKeyDropSummarizer struct{}

func (demoKeyDropSummarizer) Summarize(msgs []agent.Message) (string, error) {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(": 内容已处理\n")
	}
	return b.String(), nil
}

// runDemoTaskGraph 演示增量 H（任务系统 + 依赖图）：
//
//	A 段 · 纯 TaskGraph 编排：父任务 release 拆 3 个子任务——
//	   t1 改代码 / t2 写文档（无依赖，可并行），t3 跑测试（依赖 t1+t2）。
//	   演示依赖门控（t3 初始 blocked）、可调度查询、完成态传播（子任务
//	   全 done → release 自动 done）。
//	B 段 · 与 Agent.Run 打通：带 WithTaskGraph 的 mock agent 跑一次 Run，
//	   开始标 running、completed 自动 done，TraceTask 事件进入轨迹。
func runDemoTaskGraph(verbose bool) {
	fmt.Println("== 增量 H 演示：任务系统 + 依赖图（Task Graph）==")

	// ---- A 段：纯 TaskGraph 编排 ----
	g := agent.NewTaskGraph()
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(g.Register("release", "发布 v2.0", ""))
	must(g.Register("t1", "改代码", "release"))
	must(g.Register("t2", "写文档", "release"))
	must(g.Register("t3", "跑测试", "release", "t1", "t2"))

	fmt.Println("\n[A] 注册 4 个任务：release 父任务 + t1/t2 可并行 + t3 依赖 t1+t2")
	fmt.Print(g.Dump())

	fmt.Println("\n[A] 初始可调度（依赖全就绪）:", g.Schedulable())
	fmt.Println("[A] t3 依赖未就绪 → IsBlocked =", g.IsBlocked("t3"))

	// 模拟并行执行：先并行领取 t1、t2（Schedulable 只有它俩）
	fmt.Println("\n[A] 领取并执行 t1、t2（并行）→ 均 done")
	g.MarkRunning("t1")
	g.MarkRunning("t2")
	g.MarkDone("t1")
	fmt.Println("[A] t1 done 后：t3 仍 blocked =", g.IsBlocked("t3"), "；可调度 =", g.Schedulable())
	g.MarkDone("t2")
	fmt.Println("[A] t2 done 后：t3 解除门控 =", !g.IsBlocked("t3"), "；可调度 =", g.Schedulable())

	fmt.Println("\n[A] 领取并执行 t3 → done")
	g.MarkRunning("t3")
	g.MarkDone("t3")
	st, _ := g.Status("release")
	fmt.Printf("[A] 所有子任务完成 → 完成态传播：release 状态 = %s\n", st)

	// ---- B 段：与 Agent.Run 打通 ----
	fmt.Println("\n[B] Agent 接入：mock agent 跑 Run(\"t1\")，Run 开始标 running、completed 自动 done")
	g2 := agent.NewTaskGraph()
	must(g2.Register("t1", "查询天气", ""))
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:     "get_weather",
		BaseRisk: agent.RiskNone,
		Execute:  func(input string) (string, error) { return "晴 24 度", nil },
	})
	llm := agent.NewMockLLM([]agent.MockDecision{
		{ToolCalls: []agent.ToolCall{{ID: "c1", Name: "get_weather", Input: `{"city":"北京"}`}}},
		{Content: "北京晴 24 度"},
	})
	a := agent.NewAgent(llm, reg, agent.WithMaxSteps(3), agent.WithTaskGraph(g2))
	res := a.Run("t1")
	fmt.Printf("[B] Run 结果：status=%s，任务 t1 状态=%s\n", res.Status, mustStatus(g2, "t1"))
	fmt.Println("[B] 轨迹中的 TraceTask 事件：")
	for _, ev := range res.Trace {
		if ev.Type == agent.TraceTask {
			fmt.Printf("    · %s: %s（%s）\n", ev.Type, ev.Outcome, ev.Name)
		}
	}
}

// mustStatus 查询任务状态（demo 用，注册必成功）。
func mustStatus(g *agent.TaskGraph, id string) string {
	st, _ := g.Status(id)
	return st
}

// runDemoIntentRoute 演示增量 D（意图路由 · 第 7 步前置）：
//
//	A 段 · 多意图并行查询：一句话拆 3 条意图 → 都路由到检索链 → 三路并行
//	   （README 读取 + git 提交计数 + git 作者列表）→ 合并汇报；
//	B 段 · 混合意图分流：改数据走审批链（未过审批不执行）、查知识走检索链；
//	C 段 · 闲聊直答：chit-chat 不调工具直接回答。
//
// 并行用标准库 goroutine + WaitGroup（AGENTS.md：Go 标准库优先，
// errgroup 属第三方 x/sync，不引入）。
func runDemoIntentRoute(verbose bool) {
	fmt.Println("== 增量 D 演示：意图路由（第 7 步前置）==")

	cls := &agent.HeuristicIntentClassifier{}

	// ---- A 段：多意图并行查询 ----
	task := "这个项目有什么内容、要多少次更新、都是谁提交的"
	fmt.Printf("\n[A] 输入：%q\n", task)
	results := agent.SplitAndClassify(task, cls)
	for i, r := range results {
		fmt.Printf("    子句 %d：%q → %s（conf=%.2f）→ 路由 %s\n",
			i+1, agent.HeuristicIntentSplitter(task)[i], r.Intent, r.Confidence, r.Route)
	}

	// 三路并行执行（每路一个 goroutine），WaitGroup 汇总
	type laneResult struct {
		label  string
		result string
		err    error
	}
	lanes := []laneResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(label string, fn func() (string, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := fn()
			mu.Lock()
			lanes = append(lanes, laneResult{label: label, result: res, err: err})
			mu.Unlock()
		}()
	}
	run("README 读取", func() (string, error) {
		b, err := os.ReadFile("README.md")
		if err != nil {
			return "", err
		}
		// rune 安全截断：UTF-8 多字节字符不能按字节切（会出乱码）
		s := string(b)
		r := []rune(s)
		if len(r) > 40 {
			s = string(r[:40]) + "…"
		}
		return strings.ReplaceAll(s, "\n", " "), nil
	})
	run("提交计数", func() (string, error) {
		out, err := exec.Command("git", "log", "--oneline").Output()
		if err != nil {
			return "", err
		}
		n := len(strings.Split(strings.TrimSpace(string(out)), "\n"))
		return fmt.Sprintf("%d 次提交", n), nil
	})
	run("作者列表", func() (string, error) {
		out, err := exec.Command("git", "log", "--format=%an").Output()
		if err != nil {
			return "", err
		}
		authors := map[string]bool{}
		for _, a := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if a != "" {
				authors[a] = true
			}
		}
		var names []string
		for a := range authors {
			names = append(names, a)
		}
		return "作者: " + strings.Join(names, ", "), nil
	})
	wg.Wait()
	fmt.Println("[A] 三路并行结果合并：")
	for _, l := range lanes {
		if l.err != nil {
			fmt.Printf("    · %s → 失败: %v\n", l.label, l.err)
		} else {
			fmt.Printf("    · %s → %s\n", l.label, l.result)
		}
	}

	// ---- B 段：混合意图分流 ----
	taskB := "帮我修改一下 README，另外这个项目是做什么的"
	fmt.Printf("\n[B] 混合意图输入：%q\n", taskB)
	resB := agent.SplitAndClassify(taskB, cls)
	for i, r := range resB {
		switch r.Route {
		case "approval":
			fmt.Printf("    子句 %d：%s → 审批链：写类动作，未过审批闸门不执行（演示拒绝）\n", i+1, r.Intent)
		case "retrieve":
			fmt.Printf("    子句 %d：%s → 检索链：读取 README 开头…\n", i+1, r.Intent)
			b, _ := os.ReadFile("README.md")
			s := string(b)
			rs := []rune(s)
			if len(rs) > 30 {
				s = string(rs[:30]) + "…"
			}
			fmt.Printf("       → %s\n", strings.ReplaceAll(s, "\n", " "))
		default:
			fmt.Printf("    子句 %d：%s → 链路 %s\n", i+1, r.Intent, r.Route)
		}
	}

	// ---- C 段：闲聊直答 ----
	fmt.Printf("\n[C] 闲聊输入：%q\n", "你好")
	c := cls.Classify("你好")
	fmt.Printf("    → %s（conf=%.2f）→ 路由 %s：不调工具，直接回答\n", c.Intent, c.Confidence, c.Route)
	fmt.Println("    回答：你好！有什么可以帮你？（如需查项目内容/改代码，请直接说）")
}

// runDemoSelfEvolve 演示"自进化 Skill"闭环：
// 从评测失败（Bad Case）→ 四维归因 → 诊断 Skill 缺陷 → 自动改写 Skill 文本。
//
// 对应 SkillForge 论文（arXiv:2604.08618）的多轮进化实验：
//   - v0：缺工具调用指引 → Agent 不跑测试（tool 类失败）
//   - v1：补上了工具调用，但回答缺关键信息（knowledge 类失败）
//   - v2：全好了 → 收敛
//
// 用 EvolveLoop 自动跑多轮，每轮 onEvolved 回调里切换 mock 行为版本。
// 展示论文里"分数单调递增、每轮都有收益"的效果。
func runDemoSelfEvolve(verbose bool) {
	fmt.Println("== 自进化 Skill 多轮演示：每轮解决一类问题 ==")

	// v0：有缺陷的 Skill——缺工具调用指引、缺回答要点
	v0Skill := `# 修复 bug

## 何时加载
任务描述指向既有代码行为错误时加载。

## 流程
1. 复现：读相关测试与源码，跑复现命令
2. 定位根因：用 grep / read_file 顺失败栈找源头
3. 修改：edit_file 精确替换
`

	// skillVersion 控制 mock 脚本行为：
	//   0 = v0（不调 run_tests，回答缺"验证"关键词）→ tool 类失败
	//   1 = v1（调了 run_tests，但回答还缺"验证"）→ knowledge 类失败
	//   2 = v2（调了 run_tests，回答有"验证"）→ 全过
	skillVersion := 0

	mkFixBugAgent := func() *agent.Agent {
		reg := agent.NewRegistry()
		reg.Register(agent.Tool{
			Name:     "read_file",
			BaseRisk: agent.RiskNone,
			Execute:  func(input string) (string, error) { return "package main\n\nfunc add(a, b int) int { return a - b }", nil },
		})
		reg.Register(agent.Tool{
			Name:     "edit_file",
			BaseRisk: agent.RiskLow,
			Execute:  func(input string) (string, error) { return "file updated", nil },
		})
		reg.Register(agent.Tool{
			Name:     "run_tests",
			BaseRisk: agent.RiskNone,
			Execute:  func(input string) (string, error) { return "ok  \tcoverage: 85%", nil },
		})

		var decisions []agent.MockDecision
		switch skillVersion {
		case 0:
			// v0：不跑测试（Skill 缺工具调用指引），回答缺"验证"
			decisions = []agent.MockDecision{
				{ToolName: "read_file", ToolInput: `{"path":"main.go"}`},
				{ToolName: "edit_file", ToolInput: `{"path":"main.go","action":"replace"}`},
				{Content: "已修复 add 函数的减法 bug，现在返回 a+b。"},
			}
		case 1:
			// v1：补了跑测试（tool 类解决了），但回答还缺"验证"关键词
			decisions = []agent.MockDecision{
				{ToolName: "read_file", ToolInput: `{"path":"main.go"}`},
				{ToolName: "edit_file", ToolInput: `{"path":"main.go","action":"replace"}`},
				{ToolName: "run_tests", ToolInput: `{"cmd":"go test ./..."}`},
				{Content: "已修复 add 函数的减法 bug。"},
			}
		default:
			// v2：全好了——跑了测试，回答也有"验证"
			decisions = []agent.MockDecision{
				{ToolName: "read_file", ToolInput: `{"path":"main.go"}`},
				{ToolName: "edit_file", ToolInput: `{"path":"main.go","action":"replace"}`},
				{ToolName: "run_tests", ToolInput: `{"cmd":"go test ./..."}`},
				{Content: "已修复 add 函数的减法 bug，验证通过，测试覆盖率 85%。"},
			}
		}

		llm := agent.NewMockLLM(decisions)
		return agent.NewAgent(llm, reg, agent.WithMaxSteps(6))
	}

	// 评测套件：3 个用例，要求跑测试 + 回答含"修复"和"验证"
	suite := []eval.BenchCase{
		{ID: "fix-add", Task: "修复 add 函数的 bug", BuildAgent: mkFixBugAgent,
			Judge: eval.RuleJudge{
				RequireTools:  []string{"read_file", "edit_file", "run_tests"},
				RequireAnswer: []string{"修复", "验证"},
			}},
		{ID: "fix-multiply", Task: "修复 multiply 函数的 bug", BuildAgent: mkFixBugAgent,
			Judge: eval.RuleJudge{
				RequireTools:  []string{"read_file", "edit_file", "run_tests"},
				RequireAnswer: []string{"修复", "验证"},
			}},
		{ID: "fix-divide", Task: "修复 divide 函数除零的 bug", BuildAgent: mkFixBugAgent,
			Judge: eval.RuleJudge{
				RequireTools:  []string{"read_file", "edit_file", "run_tests"},
				RequireAnswer: []string{"修复", "验证"},
			}},
	}

	// 跑多轮进化
	runSuite := func() eval.Report {
		return eval.RunSuite(suite)
	}

	results := evolve.EvolveLoop(runSuite, v0Skill, 5, func(round int, newSkill string) {
		// 每轮进化后切换 mock 行为版本（模拟 Skill 内容变化导致 Agent 行为变化）
		skillVersion = round
	}, evolve.RuleDiagnostician{})

	// ---- 打印每轮结果 ----
	fmt.Println("\n--- 进化过程 ---")
	for _, r := range results {
		fmt.Printf("\n[第 %d 轮] 通过率：%.0f%%\n", r.Round, r.Score)
		if len(r.Failures) == 0 {
			fmt.Println("  ✓ 无失败，已收敛")
			continue
		}
		fmt.Printf("  失败 %d 条：\n", len(r.Failures))
		for _, f := range r.Failures {
			fmt.Printf("    · [%s/%s] %s\n", f.Category, f.Severity, f.CaseID)
		}
		if len(r.Suggestions) > 0 {
			fmt.Printf("  诊断建议 %d 条：\n", len(r.Suggestions))
			for _, s := range r.Suggestions {
				fmt.Printf("    · [%s]\n", s.Section)
			}
		}
	}

	// ---- 最终总结 ----
	fmt.Println("\n--- 进化总结 ---")
	final := results[len(results)-1]
	fmt.Printf("初始通过率：%.0f%% → 最终通过率：%.0f%%\n", results[0].Score, final.Score)
	fmt.Printf("共跑 %d 轮，失败从 %d 条降到 %d 条\n",
		len(results), len(results[0].Failures), len(final.Failures))

	fmt.Println("\n最终 Skill 全文（v0 + 各轮追加）：")
	fmt.Println(strings.Repeat("─", 50))
	fmt.Println(final.NewSkillBody)
	fmt.Println(strings.Repeat("─", 50))
}

// runDemoDAG 演示 Plan-and-Execute DAG 执行器：
// 模拟"分析支付接口变慢原因"的 6 步任务，展示三层 DAG 并行执行。
//
// 场景设计（对应文章里的例子）：
//   第一层（4 个并行）：查延迟监控 / 查依赖健康度 / 查发布记录 / 查错误日志
//   第二层（等齐）：分析延迟与错误的时间相关性
//   第三层（汇总）：生成报告
//
// 故意让"查发布记录"这步失败，展示失败隔离——
// 其他分支照跑，后继节点标记 skipped。
func runDemoDAG(verbose bool) {
	fmt.Println("=== Plan-and-Execute DAG 执行器演示 ===")
	fmt.Println("场景：分析昨天支付接口变慢的原因，生成报告")
	fmt.Println()

	// 1. 构造 DAG：6 个节点，三层
	graph := dag.Graph{
		Nodes: []dag.Node{
			{ID: "1", Action: "查支付接口延迟监控", Tool: "query_metrics", Params: `{"service":"payment","metric":"latency"}`},
			{ID: "2", Action: "查依赖服务健康度", Tool: "query_health", Params: `{"deps":["db","redis","mq"]}`},
			{ID: "3", Action: "查昨天的发布记录", Tool: "query_deploys", Params: `{"date":"yesterday"}`},
			{ID: "4", Action: "查错误日志", Tool: "query_logs", Params: `{"level":"error","date":"yesterday"}`},
			{ID: "5", Action: "分析延迟与错误的时间相关性", Tool: "analyze_correlation", Params: `{"window":"24h"}`},
			{ID: "6", Action: "汇总生成报告", Tool: "gen_report", Params: `{"title":"支付接口变慢分析"}`},
		},
		Edges: []dag.Edge{
			// 前 4 步都要等齐，第 5 步才能跑
			{From: "1", To: "5"},
			{From: "2", To: "5"},
			{From: "3", To: "5"},
			{From: "4", To: "5"},
			// 第 5 步完成后才能出报告
			{From: "5", To: "6"},
		},
	}

	// 2. 校验 + 拓扑排序
	fmt.Println("--- 1. 校验 DAG + 拓扑排序 ---")
	layers, err := graph.TopoSort()
	if err != nil {
		fmt.Println("DAG 校验失败:", err)
		return
	}
	fmt.Printf("共 %d 层：\n", len(layers))
	for i, layer := range layers {
		fmt.Printf("  第 %d 层：%v（%d 个并行节点）\n", i+1, layer, len(layer))
	}
	fmt.Println()

	// 3. 注册工具（模拟执行函数）
	executor := dag.NewExecutor(3) // 最多 3 个并发
	executor.Register("query_metrics", func(params string) (string, error) {
		fmt.Println("    [模拟] 查询延迟监控数据...")
		return `{"p99":"850ms","p95":"420ms","baseline":"200ms"}`, nil
	})
	executor.Register("query_health", func(params string) (string, error) {
		fmt.Println("    [模拟] 查询依赖服务健康度...")
		return `{"db":"healthy","redis":"healthy","mq":"degraded"}`, nil
	})
	executor.Register("query_deploys", func(params string) (string, error) {
		// 故意让这步失败——模拟"发布记录 API 超时"
		return "", fmt.Errorf("api timeout: deploy service unavailable")
	})
	executor.Register("query_logs", func(params string) (string, error) {
		fmt.Println("    [模拟] 查询错误日志...")
		return `{"error_count":127,"top_error":"timeout"}`, nil
	})
	executor.Register("analyze_correlation", func(params string) (string, error) {
		fmt.Println("    [模拟] 分析时间相关性...")
		return `{"finding":"mq 降级时段与延迟飙升时段高度重合"}`, nil
	})
	executor.Register("gen_report", func(params string) (string, error) {
		fmt.Println("    [模拟] 生成报告...")
		return `# 支付接口变慢分析报告\n\n结论：MQ 服务降级导致支付接口延迟从 200ms 飙升到 850ms。`, nil
	})

	// 4. 执行 DAG
	fmt.Println("--- 2. 执行 DAG ---")
	nodeMap, err := executor.Run(&graph)
	if err != nil {
		fmt.Println("执行失败:", err)
		return
	}
	fmt.Println()

	// 5. 打印最终结果
	fmt.Println("--- 3. 执行结果汇总 ---")
	for id := range nodeMap {
		node := nodeMap[id]
		switch node.Status {
		case dag.StatusSuccess:
			fmt.Printf("  ✅ %s: success\n", id)
		case dag.StatusFailed:
			fmt.Printf("  ❌ %s: failed（%v）\n", id, node.Err)
		case dag.StatusSkipped:
			fmt.Printf("  ⏭️  %s: skipped\n", id)
		}
	}

	// 6. 展示最终节点的输出
	fmt.Println("\n--- 4. 最终报告内容 ---")
	if nodeMap["6"].Status == dag.StatusSuccess {
		fmt.Println(nodeMap["6"].Output)
	} else {
		fmt.Printf("报告节点状态：%s\n", nodeMap["6"].Status)
	}

	// 7. 总结
	fmt.Println("\n--- 关键观察 ---")
	fmt.Println("• 前 4 步并行执行（第 1 层），不是串行 6 步")
	fmt.Println("• 第 3 步（查发布记录）失败，后继节点标记 skipped")
	fmt.Println("• 其他分支不受影响，第 5、6 步照样跑完")
	fmt.Println("• 这就是 DAG 相比 for 循环的优势：并行加速 + 失败隔离")
}

// mockPlannerLLM 是一个假的 Planner LLM，模拟模型输出 DAG JSON。
type mockPlannerLLM struct {
	response string
}

func (m *mockPlannerLLM) Generate(prompt string) (string, error) {
	return m.response, nil
}

// runDemoDAGPlanner 演示 Planner：从自然语言任务自动生成 DAG。
//
// 跟 runDemoDAG 的区别：
//   - runDemoDAG：手写 Graph（节点和边都是硬编码的）
//   - runDemoDAGPlanner：输入任务描述，Planner 自动出 DAG
//
// 这里用 mock LLM 模拟模型输出，展示"从任务到图"的完整链路。
func runDemoDAGPlanner(verbose bool) {
	fmt.Println("=== DAG Planner：从任务描述自动生成执行图 ===")
	fmt.Println()

	// 1. 可用工具列表
	tools := []dag.ToolDesc{
		{Name: "query_metrics", Description: "查询服务监控指标（延迟、QPS、错误率）"},
		{Name: "query_health", Description: "查询依赖服务健康状态"},
		{Name: "query_logs", Description: "查询错误日志"},
		{Name: "analyze", Description: "分析多组数据的时间相关性"},
		{Name: "gen_report", Description: "生成分析报告"},
	}

	// 2. 构造 mock LLM——模拟模型返回的 DAG JSON
	//    实际场景里这一步是真实模型跑出来的
	mockLLM := &mockPlannerLLM{
		response: `
{
  "nodes": [
    {"id": "1", "action": "查支付接口延迟监控", "tool": "query_metrics"},
    {"id": "2", "action": "查依赖服务健康度", "tool": "query_health"},
    {"id": "3", "action": "查昨天的错误日志", "tool": "query_logs"},
    {"id": "4", "action": "分析延迟与错误的相关性", "tool": "analyze"},
    {"id": "5", "action": "生成分析报告", "tool": "gen_report"}
  ],
  "edges": [
    {"from": "1", "to": "4"},
    {"from": "2", "to": "4"},
    {"from": "3", "to": "4"},
    {"from": "4", "to": "5"}
  ]
}
`,
	}

	// 3. 创建 Planner，输入任务
	planner := dag.NewPlanner(mockLLM, tools)
	task := "分析昨天支付接口变慢的原因，生成一份分析报告"

	fmt.Println("--- 1. 输入任务 ---")
	fmt.Printf("任务：%s\n\n", task)

	// 4. Planner 自动出 DAG
	fmt.Println("--- 2. Planner 输出 DAG ---")
	graph, err := planner.Plan(task)
	if err != nil {
		fmt.Println("Planner 失败:", err)
		return
	}

	fmt.Printf("节点数：%d\n", len(graph.Nodes))
	for _, n := range graph.Nodes {
		fmt.Printf("  · %s: %s（工具：%s）\n", n.ID, n.Action, n.Tool)
	}
	fmt.Printf("依赖边：%d 条\n", len(graph.Edges))
	for _, e := range graph.Edges {
		fmt.Printf("  · %s → %s\n", e.From, e.To)
	}

	// 5. 拓扑排序，展示分层
	fmt.Println("\n--- 3. 拓扑排序（分层并行） ---")
	layers, err := graph.TopoSort()
	if err != nil {
		fmt.Println("拓扑排序失败:", err)
		return
	}
	for i, layer := range layers {
		fmt.Printf("  第 %d 层：%v（%d 个并行）\n", i+1, layer, len(layer))
	}

	// 6. 执行 DAG
	fmt.Println("\n--- 4. 执行 DAG ---")
	executor := dag.NewExecutor(3)
	executor.Register("query_metrics", func(params string) (string, error) {
		return `{"p99":"850ms"}`, nil
	})
	executor.Register("query_health", func(params string) (string, error) {
		return `{"mq":"degraded"}`, nil
	})
	executor.Register("query_logs", func(params string) (string, error) {
		return `{"errors":127}`, nil
	})
	executor.Register("analyze", func(params string) (string, error) {
		return `{"finding":"MQ 降级导致延迟飙升"}`, nil
	})
	executor.Register("gen_report", func(params string) (string, error) {
		return `# 支付接口变慢分析报告\n结论：MQ 服务降级导致延迟从 200ms 涨到 850ms。`, nil
	})

	nodeMap, err := executor.Run(graph)
	if err != nil {
		fmt.Println("执行失败:", err)
		return
	}

	// 7. 结果汇总
	fmt.Println("\n--- 5. 执行结果 ---")
	for id, node := range nodeMap {
		switch node.Status {
		case dag.StatusSuccess:
			fmt.Printf("  ✅ %s: success\n", id)
		case dag.StatusFailed:
			fmt.Printf("  ❌ %s: failed\n", id)
		case dag.StatusSkipped:
			fmt.Printf("  ⏭️  %s: skipped\n", id)
		}
	}

	// 8. 总结
	fmt.Println("\n--- 关键观察 ---")
	fmt.Println("• 从自然语言任务 → 结构化 DAG，全程不需要手写图")
	fmt.Println("• Planner 输出 JSON，执行器负责解析 + 校验 + 执行")
	fmt.Println("• 跟手写 Graph 的区别：模型负责规划，执行器负责调度")
	fmt.Println("• 这就是 Plan-and-Execute 的完整闭环：Planner → Executor → 结果")
}

// runDemoDAGReplan 演示局部重规划（Replan）。
//
// 场景：
//   1. 初始 DAG：查监控 → 查日志 → 分析 → 生成报告
//   2. "查监控"这步失败了（工具不存在 = 永久失败）
//   3. 触发 Replan：摘出失败节点 + 它的后继
//   4. （mock）Planner 重新规划：用"查 trace"代替"查监控"
//   5. 合并回原 DAG，继续执行
func runDemoDAGReplan(verbose bool) {
	fmt.Println("=== DAG Replan：局部重规划演示 ===")
	fmt.Println()

	// 1. 初始 DAG
	graph := &dag.Graph{
		Nodes: []dag.Node{
			{ID: "1", Action: "查延迟监控", Tool: "query_metrics", Params: `{}`},
			{ID: "2", Action: "查错误日志", Tool: "query_logs", Params: `{}`},
			{ID: "3", Action: "分析相关性", Tool: "analyze", Params: `{}`},
			{ID: "4", Action: "生成报告", Tool: "gen_report", Params: `{}`},
		},
		Edges: []dag.Edge{
			{From: "1", To: "3"},
			{From: "2", To: "3"},
			{From: "3", To: "4"},
		},
	}

	fmt.Println("--- 1. 初始 DAG ---")
	for _, n := range graph.Nodes {
		fmt.Printf("  · %s: %s\n", n.ID, n.Action)
	}
	fmt.Println()

	// 2. 模拟执行："查监控"失败了（工具不存在）
	fmt.Println("--- 2. 执行中... ---")
	executor := dag.NewExecutor(3)
	executor.Register("query_logs", func(params string) (string, error) {
		return `{"errors": 127}`, nil
	})
	// 故意不注册 query_metrics——模拟"工具不存在"的永久失败
	executor.Register("analyze", func(params string) (string, error) {
		return `{"finding": "错误率飙升"}`, nil
	})
	executor.Register("gen_report", func(params string) (string, error) {
		return `# 报告\n\n结论：错误率飙升导致问题。`, nil
	})

	nodeMap, _ := executor.Run(graph)
	fmt.Println()

	// 3. 检查是否需要 Replan
	fmt.Println("--- 3. 检查是否需要 Replan ---")
	replanner := dag.NewReplanner(dag.DefaultReplanTrigger{}, 3)

	failedNode := nodeMap["1"]
	needReplan, reason := replanner.CheckReplan(failedNode, failedNode.Err)
	fmt.Printf("  %s\n", reason)
	fmt.Printf("  需要 Replan：%v\n\n", needReplan)

	if !needReplan {
		fmt.Println("不需要重规划，流程结束。")
		return
	}

	// 4. 摘出失败节点（只替换它本身，后继节点保留）
	fmt.Println("--- 4. 摘出需要替换的节点 ---")
	fmt.Printf("  失败节点：1（查延迟监控）\n")
	fmt.Printf("  已成功、不许动的节点：%v\n", graph.LockedNodes())
	fmt.Println("  （后继节点 3、4 保留，只是要重新连接）")
	fmt.Println()

	// 5. （mock）Planner 重新规划——用"查 trace"代替"查监控"
	fmt.Println("--- 5. Planner 重新规划 ---")
	fmt.Println("  （模拟）Planner 说：监控工具挂了，改用 trace 分析")
	newNode := dag.Node{ID: "1b", Action: "查调用链 trace", Tool: "query_trace", Params: `{}`}
	fmt.Println("  新节点：1b（查 trace）")
	fmt.Println("  新依赖边：1b → 3（连到原来的分析节点）")
	fmt.Println()

	// 6. 合并回原 DAG：删掉失败节点 1，加上新节点 1b，重连边
	fmt.Println("--- 6. 合并回原 DAG ---")
	graph.MergeSubgraph([]string{"1"}, []dag.Node{newNode}, []dag.Edge{{From: "1b", To: "3"}})
	fmt.Println("  合并后的节点：")
	for _, n := range graph.Nodes {
		fmt.Printf("    · %s: %s\n", n.ID, n.Action)
	}
	fmt.Println()

	// 7. 重新执行
	fmt.Println("--- 7. 重新执行（第 2 轮）---")
	executor2 := dag.NewExecutor(3)
	executor2.Register("query_trace", func(params string) (string, error) {
		return `{"slow_calls": ["db.query", "mq.publish"]}`, nil
	})
	executor2.Register("query_logs", func(params string) (string, error) {
		return `{"errors": 127}`, nil
	})
	executor2.Register("analyze", func(params string) (string, error) {
		return `{"finding": "DB 查询慢 + 错误率飙升"}`, nil
	})
	executor2.Register("gen_report", func(params string) (string, error) {
		return `# 报告\n\n结论：DB 查询慢导致延迟飙升，伴随错误率上升。`, nil
	})

	nodeMap2, _ := executor2.Run(graph)
	fmt.Println()

	// 8. 结果
	fmt.Println("--- 8. 最终结果 ---")
	for id, node := range nodeMap2 {
		switch node.Status {
		case dag.StatusSuccess:
			fmt.Printf("  ✅ %s: success\n", id)
		case dag.StatusFailed:
			fmt.Printf("  ❌ %s: failed\n", id)
		case dag.StatusSkipped:
			fmt.Printf("  ⏭️  %s: skipped\n", id)
		}
	}

	fmt.Println("\n--- 关键观察 ---")
	fmt.Println("• 失败是永久的（工具不存在）→ 触发 Replan")
	fmt.Println("• 已成功的节点（2、3、4...不对，这里 2 还没跑）→ 锁定不动")
	fmt.Println("• 只替换失败子图，不全盘重来")
	fmt.Println("• Replan 上限 3 次——防止模型每次都规划错陷入死循环")
	fmt.Println("• 这就是论文里说的'局部重规划'：改后半段，不推翻前半段")
}

// runDemoReflection 演示 Reflection：Agent 交付前的高标准自检。
//
// 不是"及格线检查"（空不空/太短），而是"任务级验收"：
// 根据任务类型匹配对应的检查标准——SQL 查 SQL 的标准，Bug 修复查修复的标准。
func runDemoReflection(verbose bool) {
	fmt.Println("=== Reflection：Agent 交付前高标准自检演示 ===")
	fmt.Println()

	reflector := reflection.NewRuleReflector()

	// ---- 场景 1：Bug 修复类任务 ----
	fmt.Println("========================================")
	fmt.Println("场景 1：Bug 修复类任务")
	fmt.Println("========================================")

	task1 := "修复登录接口偶尔超时的 bug"

	// 有问题的回答——只说了现象，没说根因和修复
	answer1Bad := "登录接口有时候会超时，可能是网络问题吧。"

	fmt.Printf("\n任务：%s\n", task1)
	fmt.Printf("回答（有问题版）：%s\n\n", answer1Bad)

	result1Bad := reflector.Check(answer1Bad, task1, nil)
	fmt.Printf("自检得分：%d/100（通过：%v）\n", result1Bad.Score, result1Bad.Passed)
	fmt.Printf("检查项：%d 条，未达标：%d 条\n", result1Bad.TotalChecks, len(result1Bad.Issues))
	for _, issue := range result1Bad.Issues {
		fmt.Printf("  ❌ %s\n", issue.Standard)
	}

	// 好的回答——完整的修复流程
	answer1Good := `## 复现
在高并发场景下，连续调用登录接口 100 次，约 5% 的请求超时（>3s）。

## 根因定位
登录接口同步调用了 Redis 查 session + DB 查用户信息，两步串行加起来超过 2s。
在高并发下，DB 连接池被占满，后续请求排队等待连接，导致超时。

## 修复方案
1. Redis 查 session 和 DB 查用户信息改成并行调用（Promise.all）
2. DB 连接池从 10 调到 50
3. 加一个 3s 超时降级，超时直接返回"系统繁忙"

## 验证
压测 1000 QPS，P99 从 4.2s 降到 800ms，超时率从 5% 降到 0。`

	fmt.Printf("\n回答（高质量版）：\n%s\n\n", answer1Good)

	result1Good := reflector.Check(answer1Good, task1, nil)
	fmt.Printf("自检得分：%d/100（通过：%v）\n", result1Good.Score, result1Good.Passed)
	fmt.Printf("检查项：%d 条，未达标：%d 条\n", result1Good.TotalChecks, len(result1Good.Issues))
	for _, issue := range result1Good.Issues {
		fmt.Printf("  ❌ [%s] %s\n", issue.Standard, issue.Standard)
	}

	// ---- 场景 2：SQL 类任务 ----
	fmt.Println("\n\n========================================")
	fmt.Println("场景 2：SQL 查询类任务")
	fmt.Println("========================================")

	task2 := "写一个 SQL，查最近 7 天每天的订单失败率"

	answer2Bad := "SELECT * FROM orders"

	fmt.Printf("\n任务：%s\n", task2)
	fmt.Printf("回答（有问题版）：%s\n\n", answer2Bad)

	result2Bad := reflector.Check(answer2Bad, task2, nil)
	fmt.Printf("自检得分：%d/100（通过：%v）\n", result2Bad.Score, result2Bad.Passed)
	for _, issue := range result2Bad.Issues {
		fmt.Printf("  ❌ [%s] %s\n", issue.Standard, issue.Standard)
	}

	answer2Good := `-- 查最近 7 天每天的订单失败率
SELECT
  DATE(created_at) AS dt,
  COUNT(*) AS total,
  SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) AS failed,
  ROUND(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) * 100.0 / COUNT(*), 2) AS fail_rate
FROM orders
WHERE created_at >= DATE_SUB(CURRENT_DATE(), INTERVAL 7 DAY)
GROUP BY DATE(created_at)
ORDER BY dt DESC;

-- 注意：一定要加 WHERE 条件，否则全表扫描会很慢`

	fmt.Printf("\n回答（高质量版）：\n%s\n\n", answer2Good)

	result2Good := reflector.Check(answer2Good, task2, nil)
	fmt.Printf("自检得分：%d/100（通过：%v）\n", result2Good.Score, result2Good.Passed)
	for _, issue := range result2Good.Issues {
		fmt.Printf("  ❌ [%s] %s\n", issue.Standard, issue.Standard)
	}

	// ---- 总结 ----
	fmt.Println("\n\n========================================")
	fmt.Println("关键观察")
	fmt.Println("========================================")
	fmt.Println("• 不是及格线检查——是任务级高标准验收")
	fmt.Println("• 检查标准分两类：必过项（缺了就不合格）+ 质量项（有更好）")
	fmt.Println("• Bug 修复类：复现/根因/修复 是必过项，验证是质量项")
	fmt.Println("• SQL 类：SELECT/WHERE 是必过项，时间范围/防全表扫描是质量项")
	fmt.Println("• 必过项全过 + 质量项 >= 50% 才算合格——不是凑总分")
	fmt.Println("• 这就是文章里说的\"Reflection 要有明确标准，不是让模型自嗨\"")
}

// doubaoStandardGenLLM 是把豆包 API 适配成 reflection.StandardGenLLM 的适配器。
type doubaoStandardGenLLM struct {
	llm agent.LLM
}

func (m *doubaoStandardGenLLM) Generate(prompt string) (string, error) {
	msgs := []agent.Message{
		{Role: "user", Content: prompt},
	}
	resp, err := m.llm.Chat(msgs, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// runDemoReflectionGenerator 演示标准生成器：让 LLM 根据任务自动生成检查标准。
//
// 完整流程：
//   1. 用户给任务 + 特殊要求
//   2. LLM 生成检查标准（必过项 + 质量项）
//   3. 展示生成的标准
//   4. 用这个标准检查回答
func runDemoReflectionGenerator(verbose bool) {
	loadEnv(".env")
	key := os.Getenv("ARK_API_KEY")
	if key == "" {
		fmt.Println("缺少 ARK_API_KEY：export ARK_API_KEY=xxx 或写入项目根 .env（已被 git 忽略）")
		return
	}
	model := os.Getenv("DOUBAO_MODEL")
	if model == "" {
		model = "doubao-seed-2-1-lite-260915"
	}

	fmt.Println("=== Reflection 标准生成器：LLM 自动生成验收标准（真实豆包 API）===")
	fmt.Println()

	// 1. 用户任务
	task := "写一个登录页面组件（React + TypeScript）"
	userNotes := "要兼容 Chrome 最新版，性能要好，有表单校验"

	fmt.Println("--- 1. 用户任务 ---")
	fmt.Printf("任务：%s\n", task)
	fmt.Printf("特殊要求：%s\n\n", userNotes)

	// 2. 调用真实豆包 API 生成标准
	fmt.Println("--- 2. 调用豆包生成检查标准（请稍等几秒）---")

	provider := agent.NewOpenAICompatibleProvider(
		"https://ark.cn-beijing.volces.com/api/v3",
		model,
		key,
	)
	// 标准生成需要更多 token + 更长超时，关闭 thinking 加速
	provider.MaxTokens = 4096
	provider.Client = &http.Client{Timeout: 120 * time.Second}
	provider.DisableThinking = true

	llm := &doubaoStandardGenLLM{llm: provider}
	generator := reflection.NewStandardGenerator(llm)

	standards, err := generator.Generate(task, userNotes)
	if err != nil {
		fmt.Println("生成失败:", err)
		return
	}

	fmt.Printf("任务类型：%s\n", standards.TaskType)
	fmt.Printf("核心验收点：%s\n\n", standards.Summary)

	fmt.Println("必过项（缺了就不合格）：")
	for i, s := range standards.Required {
		fmt.Printf("  %d. [%s] %s\n", i+1, s.Dimension, s.Description)
		fmt.Printf("     怎么算过：%s\n", s.PassIf)
	}

	fmt.Println("\n质量项（有更好）：")
	for i, s := range standards.Quality {
		fmt.Printf("  %d. [%s] %s\n", i+1, s.Dimension, s.Description)
		fmt.Printf("     怎么算过：%s\n", s.PassIf)
	}

	// 3. 用生成的标准检查回答
	fmt.Println("\n\n--- 3. 用这个标准检查回答 ---")

	// 有问题的回答——只写了个表单，没校验、没 loading、没加密
	badAnswer := `
export default function LoginPage() {
  return (
    <div>
      <h1>登录</h1>
      <input placeholder="邮箱" />
      <input placeholder="密码" type="password" />
      <button>登录</button>
    </div>
  )
}
`

	fmt.Printf("回答（有问题版）：\n%s\n", badAnswer)

	reflector := reflection.ReflectorFromStandards(standards)
	result := reflector.Check(badAnswer, task, nil)

	fmt.Printf("\n自检得分：%d/100（通过：%v）\n", result.Score, result.Passed)
	fmt.Printf("检查项：%d 条，未达标：%d 条\n", result.TotalChecks, len(result.Issues))
	for _, issue := range result.Issues {
		fmt.Printf("  ❌ %s\n", issue.Standard)
	}

	// 4. 总结
	fmt.Println("\n\n--- 关键观察 ---")
	fmt.Println("• 标准不是硬编码的——是真实豆包 API 根据任务自动生成的")
	fmt.Println("• 用户特殊要求会影响标准（比如\"要兼容 Chrome\"）")
	fmt.Println("• 必过项是底线，质量项是加分")
	fmt.Println("• 这就是\"标准驱动\"：先想清楚什么叫好，再动手做")
}
