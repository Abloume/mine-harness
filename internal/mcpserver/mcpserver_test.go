package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mine-harness/internal/agent"
)

// newTestServer 构造带两个 mock 工具的 MCP server：
//   - echo：接收 JSON 入参，把 value 字段回显（验证参数传递）
//   - boom：固定失败（验证 isError 语义）
func newTestServer() (*Server, *bytes.Buffer) {
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
	reg.Register(agent.Tool{
		Name:        "boom",
		Description: "总是失败",
		BaseRisk:    agent.RiskNone,
		Execute: func(string) (string, error) {
			return "", errors.New("boom failed") // 任意 error 即可，验证 isError 语义
		},
	})
	out := &bytes.Buffer{}
	return New(reg, nil, out), out
}

// runMessages 把若干请求行一次性喂给 server，返回全部响应行。
func runMessages(t *testing.T, s *Server, out *bytes.Buffer, lines ...string) []map[string]any {
	t.Helper()
	msg := strings.Join(lines, "\n")
	s.in = strings.NewReader(msg) // 注入 stdin
	if err := s.Run(); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("响应不是合法 JSON: %q: %v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

func respResult(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 result 字段: %v", m)
	}
	return res
}

// TestInitialize 验证 MCP 握手：protocolVersion + capabilities + serverInfo。
func TestInitialize(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test"}}}`)
	if len(resps) != 1 {
		t.Fatalf("应返回 1 个响应，实际 %d", len(resps))
	}
	res := respResult(t, resps[0])
	if res["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v，期望 %s", res["protocolVersion"], ProtocolVersion)
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != ServerName {
		t.Errorf("serverInfo.name = %v", info["name"])
	}
}

// TestToolsList 验证 tools/list：工具名/描述/inputSchema 正确映射。
func TestToolsList(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	res := respResult(t, resps[0])
	tools := res["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("应列出 2 个工具，实际 %d", len(tools))
	}
	first := tools[0].(map[string]any)
	if first["name"] != "boom" && first["name"] != "echo" {
		t.Errorf("工具名映射异常: %v", first["name"])
	}
	if _, ok := first["description"].(string); !ok {
		t.Errorf("缺少 description")
	}
	schema := first["inputSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("inputSchema.type = %v，期望 object", schema["type"])
	}
}

// TestCallToolInputField 验证 tools/call：arguments.input（JSON 字符串）直接进 Execute。
func TestCallToolInputField(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"input":"{\"value\":\"hi\"}"}}}`)
	res := respResult(t, resps[0])
	if res["isError"] != false {
		t.Errorf("isError = %v，期望 false", res["isError"])
	}
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"]
	if text != "echo: hi" {
		t.Errorf("content.text = %v，期望 echo: hi", text)
	}
}

// TestCallToolArgumentsObject 验证兼容形态：arguments 直接是对象（无 input 字段）
// → 整体序列化为入参 JSON。
func TestCallToolArgumentsObject(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"value":"direct"}}}`)
	res := respResult(t, resps[0])
	text := res["content"].([]any)[0].(map[string]any)["text"]
	if text != "echo: direct" {
		t.Errorf("content.text = %v，期望 echo: direct", text)
	}
}

// TestCallToolError 验证工具执行失败：isError=true、错误进 content，协议层不回 error。
func TestCallToolError(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"boom","arguments":{}}}`)
	res := respResult(t, resps[0])
	if res["isError"] != true {
		t.Errorf("isError = %v，期望 true", res["isError"])
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if text == "" {
		t.Errorf("错误信息应非空")
	}
}

// TestCallUnknownTool 验证护栏：未注册工具 isError=true（复用 harness 白名单）。
func TestCallUnknownTool(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	res := respResult(t, resps[0])
	if res["isError"] != true {
		t.Errorf("未知工具 isError 应为 true")
	}
}

// TestMethodNotFound 验证未知方法回 JSON-RPC -32601。
func TestMethodNotFound(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out,
		`{"jsonrpc":"2.0","id":7,"method":"tools/unknown","params":{}}`)
	if errObj, ok := resps[0]["error"].(map[string]any); ok {
		if errObj["code"] != float64(-32601) {
			t.Errorf("error.code = %v，期望 -32601", errObj["code"])
		}
	} else {
		t.Errorf("未知方法应返回 error 对象，实际 %v", resps[0])
	}
}

// TestNotificationNoResponse 验证 notification（无 id）：处理但不响应。
func TestNotificationNoResponse(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":8,"method":"ping","params":{}}`)
	if len(resps) != 1 {
		t.Fatalf("notification 不应有响应；应只返回 ping 的 1 个响应，实际 %d", len(resps))
	}
	if _, ok := resps[0]["error"]; ok {
		t.Errorf("ping 不应报错: %v", resps[0])
	}
}

// TestParseError 验证畸形 JSON 回 -32700。
func TestParseError(t *testing.T) {
	s, out := newTestServer()
	resps := runMessages(t, s, out, `{"jsonrpc":"2.0","id":9,"method":broken`)
	errObj := resps[0]["error"].(map[string]any)
	if errObj["code"] != float64(-32700) {
		t.Errorf("error.code = %v，期望 -32700", errObj["code"])
	}
}
