// Package mcpserver 实现 MCP（Model Context Protocol）适配层——PLAN 第 4 步。
//
// 方向一（本文件）：MCP Server（对外暴露）。把 harness Registry 里注册的工具
// 映射为 MCP 工具（JSON-RPC 2.0 over stdio：tools/list + tools/call），
// 任何支持 MCP 的宿主（Claude Code / Codex CLI / OpenClaw 等）都能直接调用
// harness 的工具集——"一套实现，harness 自用 + 外部 Agent 共用"。
//
// 方向二（MCP Client，内部接入外部 MCP server）：另见 client.go（下一步）。
//
// 协议面：MCP 用 JSON-RPC 2.0，stdio 传输层按行分隔 JSON（每条消息独占一行）。
// 支持的方法：initialize / notifications/initialized / ping / tools/list /
// tools/call；未知方法回 -32601（Method not found）。
//
// Go 练习点（JS/TS ↔ Go 差异）：
//   - JSON：JS 用 JSON.parse/JSON.stringify 抛异常；Go 用 json.Unmarshal /
//     json.Marshal 返回 error，必须显式处理。
//   - io.Reader/io.Writer 是接口：这里注入 bytes.Buffer 可测试，
//     注入 os.Stdin/os.Stdout 即真实 stdio——依赖注入对应 JS 传参回调。
package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"mine-harness/internal/agent"
)

// ProtocolVersion 是 MCP 协议版本（2024-11-05 稳定版）。
const ProtocolVersion = "2024-11-05"

// ServerName / ServerVersion 用于 initialize 的 serverInfo（MCP client 可展示）。
const (
	ServerName    = "mine-harness"
	ServerVersion = "0.1.0"
)

// Server 是 stdio 传输的 MCP server：读 in（默认 os.Stdin）按行解析
// JSON-RPC 请求，处理后在 out（默认 os.Stdout）写响应。
type Server struct {
	reg      *agent.Registry                  // 工具白名单：只暴露已注册工具（复用 harness 护栏）
	approver func(toolName, args string) bool // 可选审批函数；nil = 外部 Agent 全权调用
	in       io.Reader
	out      io.Writer
}

// New 构造 MCP server。reg 为 nil 时创建空注册表（tools/list 返回空数组，
// tools/call 一律 Unknown tool）。in/out 为 nil 时用 os.Stdin/os.Stdout——
// 测试注入 bytes.Buffer，真实运行注入标准输入输出。
func New(reg *agent.Registry, in io.Reader, out io.Writer) *Server {
	if reg == nil {
		reg = agent.NewRegistry()
	}
	if in == nil {
		in = os.Stdin // 默认真实 stdin；测试注入 bytes.Buffer
	}
	if out == nil {
		out = os.Stdout // 默认真实 stdout；测试注入 bytes.Buffer
	}
	return &Server{reg: reg, in: in, out: out}
}

// SetApprover 注入审批函数：tools/call 执行前先问审批（工具作者声明的
// BaseRisk 透出在 tools/list 的 risk 字段，外部 Agent 可见）。
// 审批返回 false → 工具不执行，isError=true 提示需人工审批。
// 默认 nil：MCP 语义是外部 Agent 显式调用工具，全权放行；
// 注入后即可复用 harness 的"写类/导图走人工评审"纪律。
func (s *Server) SetApprover(approver func(toolName, args string) bool) {
	s.approver = approver
}

// riskName 把内核 RiskLevel（iota 枚举）映射为 MCP 可见的字符串标签，
// 随 tools/list 的 inputSchema 一起透出——外部 Agent 能感知每个工具的风险级别。
func riskName(l agent.RiskLevel) string {
	switch l {
	case agent.RiskLow:
		return "low"
	case agent.RiskMedium:
		return "medium"
	case agent.RiskHigh:
		return "high"
	default:
		return "none"
	}
}

// rpcRequest 是 JSON-RPC 2.0 请求。id 为 nil 表示 notification（无需响应）。
type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      *int           `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

// rpcError 遵循 JSON-RPC 2.0 错误对象；MCP 保留码段 -32000..-32099。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

// Run 进入 stdio 主循环：逐行读请求、处理、写响应，直到输入关闭或出错。
// 每行一条独立 JSON（MCP stdio 传输约定）。
func (s *Server) Run() error {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // 单行上限 4MB，防畸形超长行
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if resp := s.handleMessage(line); resp != nil {
			if _, err := s.out.Write(append(resp, '\n')); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// handleMessage 处理一条请求消息，返回响应 JSON（notification 返回 nil）。
// 拆成纯函数便于单测：输入输出都是 []byte，无 io 依赖。
func (s *Server) handleMessage(msg []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		return s.errResp(0, -32700, "Parse error: "+err.Error()) // 无法取 id，用 0
	}
	if req.JSONRPC != "2.0" {
		return s.errResp(idOr(req.ID), -32600, "Invalid Request: jsonrpc 必须为 2.0")
	}
	if req.ID == nil {
		// notification：处理但不响应（initialized / 其他单向通知）
		return nil
	}
	id := *req.ID
	switch req.Method {
	case "initialize":
		return s.okResp(id, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": ServerName, "version": ServerVersion},
		})
	case "ping":
		return s.okResp(id, map[string]any{})
	case "tools/list":
		return s.okResp(id, map[string]any{"tools": s.toolSchemas()})
	case "tools/call":
		return s.handleToolCall(id, req.Params)
	default:
		return s.errResp(id, -32601, "Method not found: "+req.Method)
	}
}

// toolSchema 是 MCP 工具描述：name/description/inputSchema（JSON Schema 子集）。
// risk 字段透出工具作者声明的 BaseRisk（none/low/medium/high）——外部 Agent
// 在 tools/list 阶段就能感知每个工具的副作用等级，与 harness 审批纪律对齐。
type toolSchema struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Risk        string `json:"risk"`
	InputSchema any    `json:"inputSchema"`
}

func (s *Server) toolSchemas() []toolSchema {
	tools := s.reg.List()
	out := make([]toolSchema, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolSchema{
			Name:        t.Name,
			Description: t.Description,
			Risk:        riskName(t.BaseRisk),
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "工具入参 JSON 字符串（可省略：缺省时把整个 arguments 作为入参）",
					},
				},
			},
		})
	}
	return out
}

// handleToolCall 执行工具并返回 MCP 结果对象：
// {content:[{type:"text",text:...}], isError:bool}。
// 执行失败（工具返回 error）→ isError=true，text 带错误信息——MCP 约定
// 工具执行失败不算协议错误（协议层仍回 result，错误进 content）。
func (s *Server) handleToolCall(id int, params map[string]any) []byte {
	name, _ := params["name"].(string)
	if name == "" {
		return s.errResp(id, -32602, "Invalid params: tools/call 需要 name")
	}
	input, err := inputFromParams(params)
	if err != nil {
		return s.errResp(id, -32602, "Invalid params: "+err.Error())
	}
	// 审批闸门：注入 approver 后，执行前先问人工评审（写类/导图工具 RiskMedium+）。
	// 默认 nil 放行（MCP 语义 = 外部 Agent 显式调用工具）。
	if s.approver != nil && !s.approver(name, input) {
		return s.okResp(id, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "工具 " + name + " 已被审批拒绝（需人工评审）：停止该动作，不要尝试效果等价的替代操作",
			}},
			"isError": true,
		})
	}
	out, callErr := s.reg.Call(name, input)
	content := []map[string]any{{"type": "text", "text": out}}
	if callErr != nil {
		content[0]["text"] = callErr.Error()
	}
	return s.okResp(id, map[string]any{
		"content": content,
		"isError": callErr != nil,
	})
}

// inputFromParams 把 tools/call 的 arguments 转成 harness 工具入参 JSON 字符串：
//   - arguments 含 input 字段（字符串）→ 直接用；
//   - arguments 为其他对象 → 整体序列化（兼容直接传参数对象的 client）。
func inputFromParams(params map[string]any) (string, error) {
	args, ok := params["arguments"].(map[string]any)
	if !ok || len(args) == 0 {
		return "{}", nil // 无参数工具
	}
	if s, ok := args["input"].(string); ok {
		return s, nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("arguments 无法序列化: %v", err)
	}
	return string(b), nil
}

func (s *Server) okResp(id int, result any) []byte {
	b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
	return b
}

func (s *Server) errResp(id, code int, message string) []byte {
	b, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
	return b
}

func idOr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
