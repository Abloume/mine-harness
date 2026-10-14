package dag

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PlannerLLM 是 Planner 依赖的最小 LLM 接口。
//
// 为什么不直接用 agent.LLM？因为 dag 包要保持独立——
// 它是一个纯 DAG 执行器，不依赖 agent 包的其他东西。
// 调用方想接真实 LLM，就把 agent.LLM 包一层实现这个接口。
//
// JS/TS ↔ Go 差异：TS 里常用结构类型（只要有 Generate 方法就算实现）；
// Go 里是名义类型，必须显式实现这个接口的方法集。
type PlannerLLM interface {
	// Generate 输入一段 prompt，输出模型返回的文本。
	Generate(prompt string) (string, error)
}

// ToolDesc 是 Planner 给模型看的工具描述。
//
// 只传名字和描述，不传执行函数——模型只需要知道"有哪些工具可用"，
// 不需要知道工具怎么实现。
type ToolDesc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Planner 负责把自然语言任务转成结构化 DAG。
//
// 流程：
//  1. 拼 Prompt：任务描述 + 可用工具列表 + 输出格式要求
//  2. 调 LLM 生成 JSON
//  3. 解析 JSON 成 Graph
//  4. Validate（环检测），有环就报错
//
// JS/TS ↔ Go 差异：TS 里 JSON 解析常用 try/catch + zod 校验；
// Go 用 json.Unmarshal + 手动校验，错误处理更显式。
type Planner struct {
	llm   PlannerLLM
	tools []ToolDesc
}

// NewPlanner 创建一个 Planner。
// llm：LLM 实现（真实模型或 mock）
// tools：可用工具列表（名字 + 描述）
func NewPlanner(llm PlannerLLM, tools []ToolDesc) *Planner {
	return &Planner{llm: llm, tools: tools}
}

// planJSON 是模型输出的 JSON 格式（跟 Graph 对应，但字段更简单）。
type planJSON struct {
	Nodes []struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Tool   string `json:"tool"`
		Params string `json:"params,omitempty"`
	} `json:"nodes"`
	Edges []struct {
		From     string `json:"from"`
		To       string `json:"to"`
		Optional bool   `json:"optional,omitempty"`
	} `json:"edges"`
}

// Plan 输入任务描述，输出一个可执行的 DAG。
//
// 内部做了三件事：
//  1. 构造 Prompt 让模型输出 JSON
//  2. 解析 JSON 成 Graph
//  3. Validate 确保没有环
func (p *Planner) Plan(task string) (*Graph, error) {
	prompt := p.buildPrompt(task)

	// 调 LLM
	raw, err := p.llm.Generate(prompt)
	if err != nil {
		return nil, fmt.Errorf("Planner 调用 LLM 失败: %w", err)
	}

	// 解析 JSON——模型可能返回带 markdown 代码块的 JSON，先清理一下
	cleaned := cleanJSON(raw)

	var plan planJSON
	if err := json.Unmarshal([]byte(cleaned), &plan); err != nil {
		return nil, fmt.Errorf("解析 Planner 输出失败: %w\n原始输出: %s", err, raw)
	}

	// 转成 Graph
	g := &Graph{}
	for _, n := range plan.Nodes {
		g.Nodes = append(g.Nodes, Node{
			ID:     n.ID,
			Action: n.Action,
			Tool:   n.Tool,
			Params: n.Params,
		})
	}
	for _, e := range plan.Edges {
		g.Edges = append(g.Edges, Edge{
			From:     e.From,
			To:       e.To,
			Optional: e.Optional,
		})
	}

	// 校验合法性
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("Planner 输出的 DAG 不合法: %w", err)
	}

	return g, nil
}

// buildPrompt 构造 Planner 的 prompt。
//
// 关键设计：
//   - 明确告诉模型输出格式（严格 JSON）
//   - 给出一个例子（few-shot）
//   - 强调"DAG = 节点 + 依赖边"
func (p *Planner) buildPrompt(task string) string {
	var sb strings.Builder

	sb.WriteString("你是一个任务规划器。把用户的任务拆解成 DAG（有向无环图）。\n\n")

	sb.WriteString("## 输出格式\n")
	sb.WriteString("严格输出 JSON，不要加其他文字。格式如下：\n")
	sb.WriteString("```json\n")
	sb.WriteString(`{
  "nodes": [
    {"id": "1", "action": "人类可读的动作描述", "tool": "工具名", "params": "JSON 参数（可选）"}
  ],
  "edges": [
    {"from": "1", "to": "2"}
  ]
}`)
	sb.WriteString("\n```\n\n")

	sb.WriteString("## 规则\n")
	sb.WriteString("1. 每个节点对应一次工具调用\n")
	sb.WriteString("2. edges 表示依赖关系：from 完成后 to 才能执行\n")
	sb.WriteString("3. 没有依赖关系的节点尽量并行（不要硬加边）\n")
	sb.WriteString("4. 节点粒度：一个节点做一件事，不要太粗也不要太细\n\n")

	sb.WriteString("## 可用工具\n")
	for _, t := range p.tools {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", t.Name, t.Description))
	}
	sb.WriteString("\n")

	sb.WriteString("## 用户任务\n")
	sb.WriteString(task)
	sb.WriteString("\n\n")

	sb.WriteString("## 示例\n")
	sb.WriteString("任务：分析昨天支付接口变慢的原因，生成报告\n")
	sb.WriteString("输出：\n")
	sb.WriteString("```json\n")
	sb.WriteString(`{
  "nodes": [
    {"id": "1", "action": "查延迟监控", "tool": "query_metrics"},
    {"id": "2", "action": "查依赖健康度", "tool": "query_health"},
    {"id": "3", "action": "查错误日志", "tool": "query_logs"},
    {"id": "4", "action": "分析相关性", "tool": "analyze"},
    {"id": "5", "action": "生成报告", "tool": "gen_report"}
  ],
  "edges": [
    {"from": "1", "to": "4"},
    {"from": "2", "to": "4"},
    {"from": "3", "to": "4"},
    {"from": "4", "to": "5"}
  ]
}`)
	sb.WriteString("\n```\n")

	return sb.String()
}

// cleanJSON 清理模型返回的文本：去掉 markdown 代码块标记，提取纯 JSON。
//
// 模型很喜欢输出 ```json ... ``` 这种格式，需要剥掉外壳。
func cleanJSON(raw string) string {
	s := strings.TrimSpace(raw)

	// 去掉开头的 ```json 或 ```
	if strings.HasPrefix(s, "```") {
		// 找到第一行换行
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
	}

	// 去掉结尾的 ```
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}

	return strings.TrimSpace(s)
}
