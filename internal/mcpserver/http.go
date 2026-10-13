// 传输层三：MCP Streamable HTTP（2025-03-26 规范）——Server 端。
//
// 与 stdio（本包 Server/Client）的关系：stdio 是进程内/本地形态（起子进程、
// 管道通信）；Streamable HTTP 是网络形态——**火山方舟 Remote MCP 只支持
// Streamable HTTP，不认 stdio**，云上 Agent 通过 URL 接入，不需要与本地
// 进程共享 stdin/stdout。同一个内核（srv.handleMessage）两种传输复用——
// "机制与传输解耦"：协议处理（initialize/tools/list/tools/call）只依赖
// JSON-RPC 消息，不关心它从管道来还是从 HTTP 来。
//
// 传输形态：
//   - POST /mcp：JSON-RPC 请求。同步处理直接返回 JSON 响应（工具调用
//     基本同步，立即给结果）；notification 无响应 → 202 Accepted（空 body，
//     规范约定）。
//   - GET /mcp：SSE 流（text/event-stream）。server 主动消息走这里：
//     tools/list_changed、进度通知等。每条事件格式：
//     event: message\n data: <json>\n\n
//
// 简化点（学习项目，文档化而非实现）：多 session 隔离未做（单客户端场景
// 用不到 Mcp-Session-Id 关联；未来多租户部署时按 session 分流即可）；
// "202 + 延迟通过 SSE 回响应"的异步模式未做（本包工具全部同步执行，
// 响应就绪直接回 200，SSE 只承载 server 主动消息）。
//
// Go 练习点（JS/TS ↔ Go 差异）：
//   - net/http Handler 是函数签名（w http.ResponseWriter, r *http.Request），
//     对应 TS 的中间件 (req, res) => ...；Go 没有"框架层"，标准库就够。
//   - SSE 需要 Flusher：http.ResponseWriter 动态断言（w.(http.Flusher)），
//     对应 TS 直接 res.flush()——Go 要求运行时确认能力存在。
//   - 连接生命周期：用 r.Context().Done() 感知客户端断开（TS 是
//     req.on('close')）。
package mcpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// sseSub 是一个活跃的 SSE 连接：ch 接收待推送的 JSON 字节（缓冲队列，
// 客户端落后时丢消息而非阻塞 server——SSE 语义允许）。
type sseSub struct {
	ch chan []byte
}

// HTTPServer 把 Server 包装成 Streamable HTTP 传输。
// Handler() 返回处理器，挂到任意 mux 路径（约定 /mcp）。
type HTTPServer struct {
	srv  *Server
	mu   sync.Mutex
	subs map[*sseSub]bool // 活跃 SSE 连接（Push 的广播目标）
}

// NewHTTPServer 构造 HTTP 传输包装器。
func NewHTTPServer(s *Server) *HTTPServer {
	return &HTTPServer{srv: s, subs: make(map[*sseSub]bool)}
}

// Handler 返回 net/http 处理器：POST = JSON-RPC 请求，GET = SSE 流。
func (h *HTTPServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.handlePost(w, r)
		case http.MethodGet:
			h.handleSSE(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// handlePost 处理一条 JSON-RPC 请求：读 body → 复用 srv.handleMessage
// （同一个协议处理函数，stdio 与 HTTP 两用）→ 写响应。
func (h *HTTPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024)) // 单条消息上限 4MB（与 stdio 对齐）
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	resp := h.srv.handleMessage(body)
	if resp == nil {
		// notification：无响应。规范约定回 202 Accepted（空 body）。
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

// handleSSE 建立 SSE 流：注册订阅 → 推送事件 → 客户端断开自动注销。
// 连接保持打开，事件逐条 Flush（HTTP 流式输出）。
func (h *HTTPServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := &sseSub{ch: make(chan []byte, 16)}
	h.mu.Lock()
	h.subs[sub] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, sub)
		h.mu.Unlock()
	}()

	// 连接就绪事件（MCP 规范：SSE 端点先发 event: endpoint）。
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", r.URL.String())
	fl.Flush()

	for {
		select {
		case msg := <-sub.ch:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			fl.Flush()
		case <-r.Context().Done():
			return // 客户端断开：退出，defer 注销订阅
		}
	}
}

// Push 向全部活跃 SSE 连接广播一条 server 主动消息
// （如 notifications/tools/list_changed——工具清单变化通知）。
// 队列满的订阅丢消息不阻塞（SSE 客户端落后时允许）。
func (h *HTTPServer) Push(msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- b:
		default:
		}
	}
}
