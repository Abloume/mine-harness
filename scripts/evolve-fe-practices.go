// 临时脚本：在 fe-practices Skill 上跑一轮自进化闭环
// 用法：go run scripts/evolve-fe-practices.go
package main

import (
	"fmt"
	"os"
	"strings"

	"mine-harness/internal/agent"
	"mine-harness/internal/eval"
	"mine-harness/internal/evolve"
)

// loadEnv 极简 .env 加载（跟 main.go 里的同款）
func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func main() {
	// 1. 读取 fe-practices 的 SKILL.md
	data, err := os.ReadFile("skills/fe-practices/SKILL.md")
	if err != nil {
		fmt.Println("读取 SKILL.md 失败:", err)
		return
	}
	currentSkill := string(data)

	// 2. 构造模拟的 Bad Case：Agent 加载 fe-practices 后还是犯的错
	//    这些是"评测失败"的 CaseResult
	report := eval.Report{
		Results: []eval.CaseResult{
			{
				ID:    "search-input",
				Pass:  false,
				Reason: "最终回答缺少关键内容 \"防抖\"", // Agent 写搜索框但没加防抖
			},
			{
				ID:    "submit-button",
				Pass:  false,
				Reason: "要求实际执行 loading_state_check，但轨迹中未发现", // Agent 写提交按钮没检查 loading
			},
			{
				ID:    "user-list",
				Pass:  false,
				Reason: "最终回答缺少关键内容 \"key\"", // Agent 写列表用了 index 当 key
			},
			{
				ID:    "fetch-data",
				Pass:  false,
				Reason: "要求实际执行 error_handling_check，但轨迹中未发现", // Agent 写请求没处理 error 态
			},
			{
				ID:    "modal-close",
				Pass:  false,
				Reason: "未完成（aborted）: 步数用尽", // Agent 写弹窗时逻辑绕晕了
			},
		},
	}

	// 3. 跑自进化闭环——规则版诊断
	result := evolve.Evolve(report, currentSkill, evolve.RuleDiagnostician{})

	// 4. 输出规则版结果
	fmt.Println("=== 规则版诊断结果 ===")
	fmt.Printf("\n收集到 %d 条失败记录：\n", len(result.Failures))
	for _, f := range result.Failures {
		fmt.Printf("  · [%s/%s] %s: %s\n", f.Category, f.Severity, f.CaseID, f.Detail)
	}

	fmt.Printf("\n诊断出 %d 条改写建议：\n", len(result.Suggestions))
	for i, s := range result.Suggestions {
		fmt.Printf("  %d. [%s]\n", i+1, s.Section)
		fmt.Printf("     %s\n", strings.ReplaceAll(s.Content, "\n", "\n     "))
	}

	// 5. 用真实豆包 API 跑 LLM 版诊断
	loadEnv(".env")
	apiKey := os.Getenv("ARK_API_KEY")
	if apiKey == "" {
		fmt.Println("\n缺少 ARK_API_KEY，无法跑真实豆包诊断")
		return
	}
	model := os.Getenv("DOUBAO_MODEL")
	if model == "" {
		model = "doubao-seed-2-1-lite-260915"
	}

	fmt.Println("\n=== 真实豆包 LLM 诊断 ===")
	fmt.Printf("模型：%s\n", model)

	llm := agent.NewOpenAICompatibleProvider(
		"https://ark.cn-beijing.volces.com/api/v3",
		model,
		apiKey,
	)

	llmDiag := evolve.NewLLMDiagnostician(llm)
	llmSuggestions, err := llmDiag.Diagnose(result.Failures, currentSkill)
	if err != nil {
		fmt.Println("LLM 诊断失败:", err)
		return
	}

	fmt.Printf("\n诊断出 %d 条改写建议：\n", len(llmSuggestions))
	for i, s := range llmSuggestions {
		fmt.Printf("  %d. [%s]\n", i+1, s.Section)
		fmt.Printf("     %s\n", strings.ReplaceAll(s.Content, "\n", "\n     "))
	}

	// 6. 对比总结
	fmt.Println("\n\n=== 对比总结 ===")
	fmt.Println("规则版：通用模板，不知道 fe-practices 是讲前端的")
	fmt.Println("  → '检查常见报错处理方法' / '明确什么场景必须调用工具'")
	fmt.Println("豆包 LLM 版：读了 Skill 内容后给出针对性建议")
	fmt.Println("  → 见上方输出")
}
