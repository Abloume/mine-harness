// 传输层三：MCP Streamable HTTP（2025-03-26 规范）——Client 端。
//
// 与 stdio Client（client.go）的关系：stdio 起外部 server 子进程、管道通信，
// 适合本地/测试；Streamable HTTP 用 net/http POST 远程 server，适合云上
// 形态——**火山方舟 Remote MCP 只支持 Streamable HTTP，不认 stdio**。
// 协议处理共用：initialize / tools/list / tools/call 的解析逻辑（parseToolList、
// callResult）与 stdio Client 共享，本文件只实现"HTTP 怎么收发 JSON-RPC"。
//
// 传输形态：
//   - 请求：POST 到 server URL（Content-Type: application/json；
//     Accept: application/json, text/event-stream——声明也接受 SSE）；
//     响应同步回 200 + JSON（本包 Server 工具全部同步执行）。
//   - 会话：server 若在响应头回 Mcp-Session-Id，后续请求自动带上。
//   - 主动消息：可选 SSE 订阅（Subscribe），接收 server 推送
//     （tools/list_changed 等通知）。
//
// 与 stdio Client 的关键差异：HTTP 是请求-响应模型，每个 POST 对应一个
// 响应，不需要 pending map / reader goroutine（stdio 要处理 server
// 主动消息与多请求交错，HTTP 由 HTTP 连接天然隔离）。
//
// Go 练习点（JS/TS ↔ Go 差异）：
//   - http.NewRequestWithContext + http.Client.Do：对应 TS fetch(url, {signal})，
//     超时用 context.WithTimeout（TS 用 AbortSignal.timeout）。
//   - 响应头/状态码显式处理：TS fetch 只有 ok/status；Go 的 resp.Body
//     必须 defer Close（资源手动管理，对应 TS 的 res.body 流式读取）。
//   - SSE 行式解析：bufio.Scanner 逐行，data: 前缀剥离——对应 TS 的
//     EventSource 事件回调（Go 没有内置 EventSource，手写更透明）。
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"mine-harness/internal/agent"
)

// HTTPClient 通过 Streamable HTTP 连接远程 MCP server。
// 用途：harness 作为 client 接入"只支持 HTTP 的 MCP 服务"——火山方舟
// 知识库 MCP、AgentKit 转换出的 MCP 服务等。
type HTTPClient struct {
	url       string
	hc        *http.Client
	timeout   time.Duration
	sessionID string // server 回传的会话 id（响应头 Mcp-Session-Id）
	mu        sync.Mutex
	nextID    int
}

// NewHTTP 构造 HTTP client。url 是 MCP 端点（如 https://host/mcp）。
func NewHTTP(url string) *HTTPClient {
	return &HTTPClient{
		url:     url,
		hc:      &http.Client{}, // 默认无整体超时：单次请求由 WithTimeout/ctx 控制
		timeout: defaultTimeout,
	}
}

// WithTimeout 设置单次请求超时（默认 30s；外部 server 不可信，防挂死）。
func (c *HTTPClient) WithTimeout(d time.Duration) *HTTPClient {
	c.timeout = d
	return c
}

// Initialize 完成握手：POST initialize（记录 session id）→ 发 initialized 通知。
// 返回对端声明的 protocolVersion。
func (c *HTTPClient) Initialize(ctx context.Context) (string, error) {
	result, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mine-harness", "version": ServerVersion},
	})
	if err != nil {
		return "", err
	}
	// initialized 是 notification（无 id）：POST 后 server 回 202（无响应体），
	// 不需要解析响应。sendNotification 专门处理这种"发出即忘"的消息。
	if err := c.sendNotification(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return "", err
	}
	proto, _ := result["protocolVersion"].(string)
	return proto, nil
}

// sendNotification 发送一条 JSON-RPC notification（无 id：server 不响应）。
// 只检查 HTTP 状态码（202/200 均可），不解析响应体——
// 与 stdio Client 的"fire-and-forget"通知语义对齐（client.go Initialize）。
// Go 练习点：notification 与 request 是两种消息，Go 用 struct 字段
// （ID 指针 nil）区分，对应 TS 联合类型的分支判断。
func (c *HTTPClient) sendNotification(ctx context.Context, method string, params map[string]any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params} // 无 ID 字段 = notification
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("MCP %s 通知发送失败: %v", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("MCP %s 通知 HTTP %d", method, resp.StatusCode)
	}
	return nil
}

// ListTools 发现外部工具（与 stdio Client 共用解析逻辑）。
func (c *HTTPClient) ListTools(ctx context.Context) ([]agent.Tool, error) {
	result, err := c.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return parseToolList(result, c.Call)
}

// Call 执行外部 MCP 工具。参数包装与 stdio Client 一致
// （arguments.input 直传，见 client.go 的 Call 注释）。
func (c *HTTPClient) Call(ctx context.Context, name, input string) (string, error) {
	result, err := c.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": map[string]any{"input": input},
	})
	if err != nil {
		return "", err
	}
	return callResult(result, name)
}

// request 发送一条 JSON-RPC 请求并解析响应。
// 处理三种状态码：200 = 同步 JSON 响应；202 = notification/异步（无响应体，
// 正常路径）；其他 = 协议错误。超时由 ctx + c.timeout 双保险控制。
func (c *HTTPClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("请求序列化失败: %v", err)
	}

	// 单次请求超时：context.WithTimeout 覆盖（对应 TS 的 AbortSignal.timeout）。
	// Go 练习点：ctx 从入参继承，超时是派生 context——"超时是调用链属性"。
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream") // 声明也接受 SSE
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("MCP %s 请求失败: %v", method, err)
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid // 记住会话：后续请求带上
	}
	switch resp.StatusCode {
	case http.StatusAccepted:
		return map[string]any{}, nil // notification 正常路径：202 + 空 body
	case http.StatusOK:
		var rpcResp rpcResponse
		if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
			return nil, fmt.Errorf("MCP %s 响应解析失败: %v", method, err)
		}
		if rpcResp.Error != nil {
			return nil, fmt.Errorf("MCP %s 返回错误(%d): %s", method, rpcResp.Error.Code, rpcResp.Error.Message)
		}
		result, _ := rpcResp.Result.(map[string]any)
		return result, nil
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("MCP %s HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

// Subscribe 建立 GET SSE 流，接收 server 主动消息（event: message）。
// 阻塞运行直到 ctx 取消或连接断开；每条消息调用 handler。
// 可选能力：工具调用不依赖它（响应全走 POST 同步），订阅只为接收通知。
func (c *HTTPClient) Subscribe(ctx context.Context, handler func(msg map[string]any)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE 连接 HTTP %d", resp.StatusCode)
	}

	// SSE 行式协议：event: <name> / data: <payload>，空行结束一条事件。
	// Go 练习点：bufio.Scanner 逐行（对应 TS EventSource 的回调封装）。
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if data.Len() > 0 {
				var msg map[string]any
				if err := json.Unmarshal(data.Bytes(), &msg); err == nil {
					handler(msg)
				}
				data.Reset()
			}
		}
		// event: 行与 endpoint 初始事件忽略（demo 只关心 message 的 data）
	}
	return sc.Err()
}
