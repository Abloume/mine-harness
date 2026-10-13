package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mine-harness/internal/agent"
)

// newHTTPTestServer 起一个 Streamable HTTP 传输的 MCP server（真实 HTTP 栈：
// httptest 走 TCP，不是内存管道——验证"云上 Agent 通过 URL 接入"的形态）。
func newHTTPTestServer(t *testing.T, reg *agent.Registry) (*httptest.Server, *HTTPServer) {
	t.Helper()
	srv := New(reg, nil, nil)
	hs := NewHTTPServer(srv)
	ts := httptest.NewServer(hs.Handler())
	t.Cleanup(ts.Close)
	return ts, hs
}

// TestHTTPRoundTrip 验证 HTTP 传输全链路：initialize（含 initialized 通知）→
// tools/list（risk 映射）→ tools/call（结果回传）。
func TestHTTPRoundTrip(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:        "echo",
		Description: "回显 value 字段",
		BaseRisk:    agent.RiskLow,
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
	ts, _ := newHTTPTestServer(t, reg)

	cli := NewHTTP(ts.URL + "/mcp")
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
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("工具发现错误: %+v", tools)
	}
	if tools[0].BaseRisk != agent.RiskLow {
		t.Errorf("risk 映射错误: %v，期望 RiskLow", tools[0].BaseRisk)
	}

	out, err := cli.Call(ctx, "echo", `{"value":"http"}`)
	if err != nil {
		t.Fatalf("tools/call 失败: %v", err)
	}
	if out != "echo: http" {
		t.Errorf("结果 = %q，期望 %q", out, "echo: http")
	}
}

// TestHTTPSSE 验证 SSE 流：建立 GET 订阅 → server Push 一条主动消息 →
// 客户端从流中收到 data 内容（tools/list_changed 通知场景）。
func TestHTTPSSE(t *testing.T) {
	ts, hs := newHTTPTestServer(t, agent.NewRegistry())

	// 手动构造 GET 请求（Accept: text/event-stream）
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("建立 SSE 连接失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 状态码 = %d", resp.StatusCode)
	}

	// goroutine 读流，把 event: message 的 data 行内容发到 channel
	// （跳过 event: endpoint 初始事件——它是连接就绪标记，不是 JSON-RPC 消息）。
	dataCh := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:") && event == "message":
				dataCh <- strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()

	// server 主动推送一条 tools/list_changed 通知
	hs.Push(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/tools/list_changed",
	})
	select {
	case data := <-dataCh:
		var msg map[string]any
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			t.Fatalf("SSE data 不是合法 JSON: %q: %v", data, err)
		}
		if m, _ := msg["method"].(string); m != "notifications/tools/list_changed" {
			t.Errorf("SSE 消息 method = %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到 SSE 推送")
	}
}

// TestHTTPTimeout 验证 HTTP client 超时护栏：server 挂死不响应 →
// 单次请求在时限内返回 error（防外部 server 拖垮主循环）。
func TestHTTPTimeout(t *testing.T) {
	// 手写挂起 handler：tools/call 睡 1s（超过 client 的 50ms 超时），
	// 其他请求重置 body 后代理给正常 handler（避免 body 被重复读成 EOF）。
	hs := NewHTTPServer(New(agent.NewRegistry(), nil, nil))
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body)) // 重置 body：ServeHTTP 会再读一次
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if req.Method == "tools/call" {
			time.Sleep(time.Second) // 挂死：不响应
			return
		}
		hs.Handler().ServeHTTP(w, r)
	}))
	defer slow.Close()

	cli := NewHTTP(slow.URL + "/mcp").WithTimeout(50 * time.Millisecond)
	ctx := context.Background()
	if _, err := cli.Initialize(ctx); err != nil {
		t.Fatalf("握手应成功: %v", err)
	}
	_, err := cli.Call(ctx, "hang", "{}")
	if err == nil {
		t.Fatal("挂死的 server 应触发超时 error")
	}
	if !strings.Contains(err.Error(), "超时") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("错误应说明超时: %v", err)
	}
}

// TestHTTPCallError 验证 isError 转 error（HTTP 传输同样遵守 MCP 约定）。
func TestHTTPCallError(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(agent.Tool{
		Name:     "boom",
		BaseRisk: agent.RiskNone,
		Execute:  func(string) (string, error) { return "", fmt.Errorf("boom failed") },
	})
	ts, _ := newHTTPTestServer(t, reg)

	cli := NewHTTP(ts.URL + "/mcp")
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
