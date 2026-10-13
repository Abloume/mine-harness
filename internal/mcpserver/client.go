// 方向二：MCP Client（内部接入外部 MCP server）。
//
// 与方向一（Server，对外暴露）互为镜像：Server 把 harness 工具翻译成 MCP 工具
// 供外部 Agent 调用；Client 把外部 MCP server 的工具翻译回 harness 工具注册进
// 主循环——"一套协议，两个方向"。本文件复用 server.go 的 rpcRequest/rpcResponse
// 协议结构（同包共享），体现 PLAN 第 4 步的 Go 练习点「双向角色」。
//
// 协议面：JSON-RPC 2.0 over stdio，按行分隔 JSON。Client 侧流程：
//
//	initialize（握手）→ notifications/initialized（单向通知）
//	→ tools/list（发现外部工具，映射为 agent.Tool）
//	→ tools/call（执行外部工具，结果回填 harness 主循环）
//
// 设计要点（对齐生产 harness 的"外部依赖不信任"原则）：
//   - 超时护栏：单次请求默认 30s，防外部 server 挂死卡住主循环——
//     对应"下游服务超时"的生产问题（指数退避在 Provider，超时在 Client）。
//   - 风险映射：外部工具透出 risk 字段（harness 扩展，none/low/medium/high）
//     则映射为对应 RiskLevel；未透出的外部工具保守默认 RiskMedium，
//     照样走 harness 审批闸门——"外部工具不等于放开风险"。
//   - 并发安全：外部工具默认不声明 ConcurrentSafe（不知道对方是否线程安全），
//     内核自动按顺序执行，正确性优先。
//   - 结果解析：MCP 工具结果 content 数组只取 text 项；isError=true 转为 error
//     回填（模型看到失败而非"假成功"）。
//
// 传输层两种构造：
//   - NewInMemory：测试/demo 用 net.Pipe 全内存互通（本包 server 双端对连）；
//   - NewStdio：生产形态，起外部 MCP server 子进程，stdin/stdout 走协议、
//     stderr 收集（MCP 约定：日志走 stderr，stdout 只走协议）。
//
// Go 练习点（JS/TS ↔ Go 差异）：
//   - 双向通道：TS 里 WebSocket/双工流是事件驱动；Go 用 io.Reader/io.Writer
//     两个单向接口组合（server 端和 client 端各持一端）。
//   - 超时取消：TS 常用 AbortController + Promise.race；Go 用 time.Timer +
//     select（多路等待：响应 / 超时 / ctx 取消）。
//   - 子进程管道：TS child_process 的 spawn 回调式；Go exec.Cmd 的
//     StdinPipe/StdoutPipe 是同步接口，配合 bufio.Scanner 读行。
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mine-harness/internal/agent"
)

// defaultTimeout 是单次 MCP 请求的默认超时：外部 server 是"不信任的下游"，
// 必须限时，否则一个挂死的 server 会拖垮整个 agent 主循环。
const defaultTimeout = 30 * time.Second

// remoteTool 是 MCP tools/list 返回的外部工具描述。
// Risk 字段是 harness 对 MCP 的扩展透出（server.go 的 toolSchema 会带）；
// 第三方 MCP server 一般没有该字段 → 映射时走保守默认（RiskMedium）。
type remoteTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Risk        string `json:"risk"`
	InputSchema any    `json:"inputSchema"`
}

// Client 是 MCP client：连接一个外部 MCP server，把它的工具发现并包装成
// harness 的 agent.Tool，注册进 Registry 后由模型按普通工具调用。
//
// 并发模型：一个常驻 reader goroutine 读响应并按 id 路由到 pending map，
// request 只负责写请求 + 等自己的响应 channel——标准 JSON-RPC client 模式
// （Go 练习点：goroutine + channel 做"异步响应路由"，对应 TS 的 Promise map）。
type Client struct {
	in     io.Writer          // 写给 server（stdio 模式 = cmd.Stdin）
	sc     *bufio.Scanner     // 从 server 读（stdio 模式 = cmd.Stdout）
	errB   *bytes.Buffer      // stdio 模式的 stderr 收集（协议外日志，调试用）
	proc   *exec.Cmd          // stdio 模式持有的子进程（Close 时 Wait 回收）
	timeout time.Duration     // 单次请求超时（可经 WithTimeout 覆盖）

	mu     sync.Mutex            // 保护 nextID 与 pending
	nextID int                   // 自增请求 id（JSON-RPC id 关联响应）
	pending map[int]chan rpcResponse // id → 响应通道（reader goroutine 投递）
	started bool
}

// NewInMemory 构造一个内存互通的 client：r 是从 server 读的方向，
// w 是写给 server 的方向。测试/demo 用 net.Pipe 连接本包 Server 的同一端。
func NewInMemory(r io.Reader, w io.Writer) *Client {
	c := &Client{
		in:      w,
		sc:      bufio.NewScanner(r),
		timeout: defaultTimeout,
		pending: make(map[int]chan rpcResponse),
	}
	c.sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // 单行上限 4MB，防畸形超长行（与 server 对齐）
	c.startReader()
	return c
}

// NewStdio 起一个外部 MCP server 子进程并连接（生产形态）。
// 协议走 stdin/stdout；stderr 收集进 Client（MCP 约定：日志必须走 stderr，
// 混入 stdout 会破坏按行解析）。cmd 不存在/启动失败返回 error。
func NewStdio(ctx context.Context, cmd string, args ...string) (*Client, error) {
	c := exec.CommandContext(ctx, cmd, args...)
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin 管道失败: %v", err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout 管道失败: %v", err)
	}
	var errB bytes.Buffer
	c.Stderr = &errB // 协议外日志：收集不丢弃，Close 后可查（errB 导出见 DebugErr）
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("启动外部 MCP server %q 失败: %v", cmd, err)
	}
	cli := &Client{
		in:      stdin,
		sc:      bufio.NewScanner(stdout),
		errB:    &errB,
		proc:    c,
		timeout: defaultTimeout,
		pending: make(map[int]chan rpcResponse),
	}
	cli.sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	cli.startReader()
	return cli, nil
}

// WithTimeout 设置单次请求超时（默认 30s）。外部 server 不可信，
// 超时是防挂死的护栏；0 表示不设超时（不推荐，仅调试用）。
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.timeout = d
	return c
}

// startReader 启动常驻 reader goroutine：逐行读响应，按 id 投递到 pending map。
// Scanner 只被这一个 goroutine 读，天然无竞争；底层流关闭（对端关/进程退出）
// 后 Scan 返回 false，goroutine 退出。
func (c *Client) startReader() {
	c.started = true
	go func() {
		for c.sc.Scan() {
			var resp rpcResponse
			if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
				continue // 畸形行跳过（协议宽容，但不会 panic）
			}
			c.mu.Lock()
			ch, ok := c.pending[resp.ID]
			if ok {
				delete(c.pending, resp.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- resp // 投递；无 pending 的消息（server 主动通知）直接丢弃
			}
		}
	}()
}

// request 发送一条 JSON-RPC 请求并等待对应响应（id 关联）。
// 超时/ctx 取消 → 返回 error；响应带 error 对象 → 返回 error（协议层错误）。
// 这是 Client 的唯一收发原语，Initialize/ListTools/Call 都建立在它之上。
func (c *Client) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1) // 先放 pending 再写请求：防止响应先到
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { // 兜底清理：超时/取消后残留的 pending 项
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("请求序列化失败: %v", err)
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("写入 MCP 请求失败: %v", err)
	}

	// 超时护栏：select 多路等待（响应 / 超时 / ctx 取消）。
	// Go 练习点：time.Timer + select 对应 TS 的 Promise.race + AbortController。
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP %s 返回错误(%d): %s", method, resp.Error.Code, resp.Error.Message)
		}
		result, _ := resp.Result.(map[string]any)
		return result, nil
	case <-timer.C:
		return nil, fmt.Errorf("MCP %s 超时（%v）：外部 server 未在时限内响应", method, c.timeout)
	case <-ctx.Done():
		return nil, fmt.Errorf("MCP %s 被取消: %v", method, ctx.Err())
	}
}

// Initialize 完成 MCP 握手：initialize 请求 + initialized 单向通知。
// 返回对端声明的 protocolVersion（可打印用于确认连接成功）。
func (c *Client) Initialize(ctx context.Context) (string, error) {
	result, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mine-harness", "version": ServerVersion},
	})
	if err != nil {
		return "", err
	}
	// initialized 是 notification（无 id）：发出后对端才认为握手完成。
	// server.go 对 notification 不响应，这里只写不发等待。
	notif := rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized", Params: map[string]any{}}
	b, _ := json.Marshal(notif)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return "", fmt.Errorf("发送 initialized 通知失败: %v", err)
	}
	proto, _ := result["protocolVersion"].(string)
	return proto, nil
}

// ListTools 调用 tools/list 发现外部工具，映射为 harness 的 agent.Tool
// （包装 Execute 走 tools/call），供注册进主循环。风险映射规则见 riskFromName。
func (c *Client) ListTools(ctx context.Context) ([]agent.Tool, error) {
	result, err := c.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return parseToolList(result, c.Call)
}

// parseToolList 把 tools/list 的 result 解析映射为 []agent.Tool。
// 独立成包级函数：stdio Client 与 HTTP Client（http_client.go）共用，
// 避免两份实现漂移（Go 练习点：提取公共逻辑，对应 TS 抽公共函数）。
// call 参数注入各自的 tools/call 执行函数（stdio 传 c.Call、HTTP 传 hc.Call）。
func parseToolList(result map[string]any, call func(ctx context.Context, name, input string) (string, error)) ([]agent.Tool, error) {
	raw, ok := result["tools"].([]any)
	if !ok {
		return nil, fmt.Errorf("tools/list 返回结构异常：缺少 tools 数组")
	}
	tools := make([]agent.Tool, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		rt := remoteTool{}
		// 类型断言逐字段复制（JSON 解码到 map 再拷贝，避免二次反序列化歧义）
		rt.Name, _ = m["name"].(string)
		rt.Description, _ = m["description"].(string)
		rt.Risk, _ = m["risk"].(string)
		name := rt.Name
		if name == "" {
			continue // 无名字的工具没法注册，跳过
		}
		desc := rt.Description
		if desc == "" {
			desc = "外部 MCP 工具（经 mine-harness MCP Client 接入）"
		}
		tools = append(tools, agent.Tool{
			Name:        name,
			Description: desc,
			BaseRisk:    riskFromName(rt.Risk), // 未声明风险 → RiskMedium 保守进审批
			Execute: func(input string) (string, error) {
				// Tool.Execute 无 ctx 签名：内部超时由各自 Client 的超时兜底
				return call(context.Background(), name, input)
			},
		})
	}
	return tools, nil
}

// riskFromName 把外部工具透出的 risk 字符串映射为内核 RiskLevel。
// 规则：none/low/medium/high 一一对应；未声明（空/未知值）→ RiskMedium——
// 外部工具默认不可信，宁可多送审（不对称风险论：宁多送审、勿漏审）。
func riskFromName(s string) agent.RiskLevel {
	switch s {
	case "none":
		return agent.RiskNone
	case "low":
		return agent.RiskLow
	case "medium":
		return agent.RiskMedium
	case "high":
		return agent.RiskHigh
	default:
		return agent.RiskMedium // 未声明 → 保守
	}
}

// Call 执行外部 MCP 工具（tools/call）。
//
// 参数兼容：input 是 harness 工具入参 JSON 字符串；这里包成
// {"arguments":{"input": "<json>"}} 直传——与本包 Server 的 inputFromParams
// 兼容路径一一对应（arguments.input 字符串优先），任何按本协议实现的
// 外部 server 都能接。
//
// 结果解析：content 数组只取 text 项拼接；isError=true → 返回 error
// （错误进 content 是 MCP 约定，协议层不算错误——与 server.go 的 handleToolCall
// 语义对齐）。
func (c *Client) Call(ctx context.Context, name, input string) (string, error) {
	result, err := c.request(ctx, "tools/call", map[string]any{
		"name": name,
		"arguments": map[string]any{"input": input},
	})
	if err != nil {
		return "", err
	}
	return callResult(result, name)
}

// callResult 解析 tools/call 的 result：content 数组取 text 拼接，
// isError=true 转 error（工具失败是业务结果不是协议错误）。
// 独立成包级函数：stdio Client 与 HTTP Client 共用。
func callResult(result map[string]any, name string) (string, error) {
	text := contentText(result)
	if isErr, _ := result["isError"].(bool); isErr {
		return "", fmt.Errorf("外部 MCP 工具 %s 执行失败: %s", name, text)
	}
	return text, nil
}

// contentText 从 MCP 工具结果中提取 text 内容（跳过 image 等其他类型）。
// Go 练习点：JSON 解码到 any 后要逐层类型断言（map[string]any / []any），
// 对应 TS 的 any 一路点下去——Go 没有隐式类型转换，断言失败返回零值不 panic。
func contentText(result map[string]any) string {
	items, ok := result["content"].([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "text" {
			if s, ok := m["text"].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

// Close 关闭连接。stdio 模式：关 stdin 让 server 读到 EOF 退出，再 Wait 回收
// 子进程（防僵尸）；内存模式：关底层写端（net.Pipe 对端读到 EOF）。
func (c *Client) Close() error {
	if wc, ok := c.in.(io.Closer); ok {
		if err := wc.Close(); err != nil {
			return err
		}
	}
	if c.proc != nil {
		return c.proc.Wait()
	}
	return nil
}

// DebugErr 返回 stdio 模式收集的 stderr 内容（外部 server 的协议外日志；
// 内存模式为空）。调试外部 server 行为时使用。
func (c *Client) DebugErr() string {
	if c.errB == nil {
		return ""
	}
	return c.errB.String()
}
