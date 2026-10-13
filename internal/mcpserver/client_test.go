package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"mine-harness/internal/agent"
)

// clientServer 起一个 harness MCP Server 跑在 net.Pipe 的一端，返回 client 端连接。
// demo 与测试共用同一模式：harness Server ↔ harness Client 全内存互通。
func clientServer(t *testing.T, reg *agent.Registry) (net.Conn, *Client) {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	srv := New(reg, srvConn, srvConn)
	go func() { _ = srv.Run() }() // server 跑在 goroutine；测试结束由 defer 关闭连接终止
	t.Cleanup(func() { srvConn.Close() })
	cli := NewInMemory(cliConn, cliConn)
	t.Cleanup(func() { cli.Close() })
	return cliConn, cli
}

// TestClientRoundTrip 验证完整链路：initialize 握手 → tools/list 发现 →
// tools/call 执行（含 BaseRisk 从 server 透出的 risk 字段正确映射）。
func TestClientRoundTrip(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "echo",
		Description: "回显 value 字段",
		BaseRisk:    agent.RiskNone,
		Execute: func(input string) (string, error) {
			var args struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal([]byte(input), &args); err != nil {
				return "", err
			}
			return "echo: " + args.Value, nil
		},
	})
	_, cli := clientServer(t, reg)

	ctx := context.Background()
	proto, err := cli.Initialize(ctx)
	if err != nil {
		t.Fatalf("initialize 失败: %v", err)
	}
	if proto != ProtocolVersion {
		t.Errorf("protocolVersion = %q，期望 %q", proto, ProtocolVersion)
	}

	tools, err := cli.ListTools(ctx)
	if err != nil {
		t.Fatalf("tools/list 失败: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("期望 1 个工具，实际 %d", len(tools))
	}
	if tools[0].Name != "echo" || tools[0].Description != "回显 value 字段" {
		t.Errorf("工具映射错误: %+v", tools[0])
	}
	if tools[0].BaseRisk != agent.RiskNone {
		t.Errorf("risk 映射错误: %v，期望 RiskNone", tools[0].BaseRisk)
	}

	out, err := cli.Call(ctx, "echo", `{"value":"hello"}`)
	if err != nil {
		t.Fatalf("tools/call 失败: %v", err)
	}
	if out != "echo: hello" {
		t.Errorf("结果 = %q，期望 %q", out, "echo: hello")
	}
}

// TestClientCallError 验证 MCP 约定：工具执行失败不是协议错误，
// isError=true 进 result——Client 必须把它转成 error 回填（模型看到失败
// 而非"假成功"，与 harness 工具失败语义对齐）。
func TestClientCallError(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:     "boom",
		BaseRisk: agent.RiskNone,
		Execute:  func(string) (string, error) { return "", fmt.Errorf("boom failed") },
	})
	_, cli := clientServer(t, reg)

	ctx := context.Background()
	if _, err := cli.Initialize(ctx); err != nil {
		t.Fatalf("initialize 失败: %v", err)
	}
	_, err := cli.Call(ctx, "boom", "{}")
	if err == nil {
		t.Fatal("isError=true 时应返回 error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error 应包含工具名: %v", err)
	}
}

// TestClientUnknownRiskDefaultsMedium 验证对"标准第三方 MCP server"的兼容：
// 无 risk 扩展字段（本包 Server 才有）→ 保守默认 RiskMedium，照走审批闸门。
func TestClientUnknownRiskDefaultsMedium(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	defer srvConn.Close()
	// 手写第三方 server：只回 initialize + tools/list，清单里没有 risk 字段
	go func() {
		sc := bufio.NewScanner(srvConn)
		for sc.Scan() {
			var req rpcRequest
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				continue
			}
			var result any
			switch req.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2024-11-05"}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{
					"name":        "ext_tool",
					"description": "第三方工具（无 risk 字段）",
					"inputSchema": map[string]any{"type": "object"},
				}}}
			default:
				continue
			}
			resp := rpcResponse{JSONRPC: "2.0", ID: idOr(req.ID), Result: result}
			if b, err := json.Marshal(resp); err == nil {
				srvConn.Write(append(b, '\n'))
			}
		}
	}()

	cli := NewInMemory(cliConn, cliConn)
	defer cli.Close()
	ctx := context.Background()
	if _, err := cli.Initialize(ctx); err != nil {
		t.Fatalf("initialize 失败: %v", err)
	}
	tools, err := cli.ListTools(ctx)
	if err != nil {
		t.Fatalf("tools/list 失败: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "ext_tool" {
		t.Fatalf("工具发现错误: %+v", tools)
	}
	if tools[0].BaseRisk != agent.RiskMedium {
		t.Errorf("缺省 risk 应保守为 RiskMedium，实际 %v", tools[0].BaseRisk)
	}
}

// TestClientTimeout 验证超时护栏：外部 server 挂死不响应 → Client 在时限内
// 返回 error，不阻塞主循环（生产教训："下游服务超时"必须限时）。
func TestClientTimeout(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	defer srvConn.Close()
	// 只回 initialize；tools/call 故意不响应（模拟挂死的 server）
	go func() {
		sc := bufio.NewScanner(srvConn)
		for sc.Scan() {
			var req rpcRequest
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				continue
			}
			if req.Method == "initialize" {
				resp := rpcResponse{JSONRPC: "2.0", ID: idOr(req.ID),
					Result: map[string]any{"protocolVersion": "2024-11-05"}}
				if b, err := json.Marshal(resp); err == nil {
					srvConn.Write(append(b, '\n'))
				}
			}
			// tools/call：不响应 → client 应超时
		}
	}()

	cli := NewInMemory(cliConn, cliConn).WithTimeout(50 * time.Millisecond)
	defer cli.Close()
	ctx := context.Background()
	if _, err := cli.Initialize(ctx); err != nil {
		t.Fatalf("握手应成功: %v", err)
	}
	_, err := cli.Call(ctx, "hang", "{}")
	if err == nil {
		t.Fatal("挂死的 server 应触发超时 error")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误应说明超时: %v", err)
	}
}

// TestNewStdioStartFailure 验证 stdio 模式对"命令不存在"的显式报错
// （Go 练习点：exec.Command 启动失败返回 error，不会静默）。
func TestNewStdioStartFailure(t *testing.T) {
	ctx := context.Background()
	if _, err := NewStdio(ctx, "definitely-not-a-real-cmd-xyz"); err == nil {
		t.Fatal("不存在的命令应启动失败并返回 error")
	}
}
