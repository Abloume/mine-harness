package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenAICompatibleProvider 协议测试：用 httptest 模拟 API 服务端，
// 验证 Provider 的"内核消息 → OpenAI 协议 → 响应解析"转换是否正确。
// 这是 mock → 真实差异的关键验证：不用真实 Key 也能证明协议层是对的。

// newTestProvider 起一个假 API 服务并返回 Provider + 捕获请求的句柄。
func newTestProvider(t *testing.T, handler http.HandlerFunc) (*OpenAICompatibleProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	return p, srv
}

func TestProviderSendsStructuredToolCalls(t *testing.T) {
	// 关键协议语义：assistant 的历史工具调用必须转成结构化 tool_calls
	// （content:null + id/type/function），而不是自然语言文本。
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization 头应为 Bearer test-key，实际 %s", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("解析请求失败: %v", err)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	msgs := []Message{
		{Role: roleUser, Content: "删掉 /tmp/a"},
		{Role: roleAssistant, Content: "调用工具 delete_file", ToolCalls: []ToolCall{{ID: "call_1", Name: "delete_file", Input: `{"file":"/tmp/a"}`}}},
		{Role: roleTool, Content: "deleted", ToolCallID: "call_1"},
	}

	_, err := p.Chat(msgs, nil)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	// 校验：assistant 消息转成了 tool_calls 结构
	var assistantMsg *chatMessage
	for i := range gotReq.Messages {
		if gotReq.Messages[i].Role == roleAssistant {
			assistantMsg = &gotReq.Messages[i]
			break
		}
	}
	if assistantMsg == nil {
		t.Fatal("请求中没有 assistant 消息")
	}
	if assistantMsg.Content != nil {
		t.Error("结构化 tool_calls 消息的 content 应为 null")
	}
	if len(assistantMsg.ToolCalls) != 1 {
		t.Fatalf("应有 1 个 tool_calls，实际 %d", len(assistantMsg.ToolCalls))
	}
	tc := assistantMsg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" {
		t.Errorf("tool_call 的 id/type 不符: %+v", tc)
	}
	if tc.Function.Name != "delete_file" || !strings.Contains(tc.Function.Arguments, "/tmp/a") {
		t.Errorf("tool_call 的 function 不符: %+v", tc.Function)
	}

	// 校验：tool 结果消息带 tool_call_id 关联
	var toolMsg *chatMessage
	for i := range gotReq.Messages {
		if gotReq.Messages[i].Role == roleTool {
			toolMsg = &gotReq.Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatal("请求中没有 tool 消息")
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Errorf("tool 消息的 tool_call_id 应为 call_1，实际 %s", toolMsg.ToolCallID)
	}
}

func TestProviderParsesToolCallResponse(t *testing.T) {
	// 响应解析：API 返回 tool_calls → Provider 转成 ToolCall{ID,Name,Input}。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"choices":[{"message":{
				"content":null,
				"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]
			}}]
		}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	resp, err := p.Chat([]Message{{Role: roleUser, Content: "查天气"}}, nil)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("应返回 1 个 ToolCall，实际 %d 个", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_9" || tc.Name != "get_weather" {
		t.Errorf("ToolCall 字段不符: %+v", tc)
	}
	if tc.Input != `{"city":"北京"}` {
		t.Errorf("ToolCall.Input 应透传原始 JSON，实际 %s", tc.Input)
	}
}

func TestProviderParsesMultipleToolCalls(t *testing.T) {
	// 多 tool_call：真实模型支持"一轮并行调用多个工具"，Provider 必须全部透传，
	// 而不是只取第一个——这是 mock 时代"一次一个动作"假设的协议级升级。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"choices":[{"message":{
				"content":null,
				"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}},
					{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{\"zone\":\"Asia/Shanghai\"}"}}
				]
			}}]
		}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	resp, err := p.Chat([]Message{{Role: roleUser, Content: "查天气和时间"}}, nil)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("应透传 2 个 ToolCall，实际 %d 个", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_1" || resp.ToolCalls[0].Name != "get_weather" {
		t.Errorf("第一个 tool_call 不符: %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].ID != "call_2" || resp.ToolCalls[1].Name != "get_time" {
		t.Errorf("第二个 tool_call 不符: %+v", resp.ToolCalls[1])
	}
}

func TestProviderSendsMultipleToolCallsInHistory(t *testing.T) {
	// 历史回填：assistant 消息带多个 tool_calls 时，请求里必须原样输出全部
	//（content:null + 数组），每条 tool 结果消息用各自 tool_call_id 关联。
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotReq)
		w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	msgs := []Message{
		{Role: roleUser, Content: "并行查两个城市天气"},
		{Role: roleAssistant, Content: "调用工具 2 个: get_weather, get_weather",
			ToolCalls: []ToolCall{
				{ID: "c1", Name: "get_weather", Input: `{"city":"北京"}`},
				{ID: "c2", Name: "get_weather", Input: `{"city":"上海"}`},
			}},
		{Role: roleTool, Content: "晴 24 度", ToolCallID: "c1"},
		{Role: roleTool, Content: "雨 20 度", ToolCallID: "c2"},
	}
	if _, err := p.Chat(msgs, nil); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	var assistantMsg *chatMessage
	for i := range gotReq.Messages {
		if gotReq.Messages[i].Role == roleAssistant && gotReq.Messages[i].ToolCalls != nil {
			assistantMsg = &gotReq.Messages[i]
			break
		}
	}
	if assistantMsg == nil {
		t.Fatal("请求中没有带 tool_calls 的 assistant 消息")
	}
	if len(assistantMsg.ToolCalls) != 2 {
		t.Fatalf("应有 2 个 tool_calls，实际 %d", len(assistantMsg.ToolCalls))
	}
	if assistantMsg.ToolCalls[1].ID != "c2" || assistantMsg.ToolCalls[1].Function.Name != "get_weather" {
		t.Errorf("第二个 tool_call 不符: %+v", assistantMsg.ToolCalls[1])
	}

	toolIDs := map[string]bool{}
	for _, m := range gotReq.Messages {
		if m.Role == roleTool {
			toolIDs[m.ToolCallID] = true
		}
	}
	if !toolIDs["c1"] || !toolIDs["c2"] {
		t.Errorf("tool 结果消息应分别关联 c1/c2，实际 %v", toolIDs)
	}
}

func TestProviderSurfacesAPIError(t *testing.T) {
	// 错误透传：401 等非 2xx 要把服务端 message 带出来，而不是只丢状态码。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"401","message":"令牌已过期或验证不正确"}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	_, err := p.Chat([]Message{{Role: roleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("应返回错误，实际为 nil")
	}
	if !strings.Contains(err.Error(), "令牌已过期") {
		t.Errorf("错误应透传服务端 message，实际: %v", err)
	}
}

func TestProviderOmitsToolsWhenEmpty(t *testing.T) {
	// 无工具时不传 tools 字段（部分 API 不接受空数组）。
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotReq)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	if _, err := p.Chat([]Message{{Role: roleUser, Content: "hi"}}, nil); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if gotReq.Tools != nil {
		t.Errorf("无工具时 tools 应为 nil（omitempty 不传），实际 %+v", gotReq.Tools)
	}
}

func TestProviderRetriesOn429(t *testing.T) {
	// 限流重试：先返回 429，再返回 200——重试后应成功（免费模型高峰期常态）。
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"429","message":"该模型当前访问量过大，请您稍后再试"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	resp, err := p.Chat([]Message{{Role: roleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("429 后重试应成功，实际失败: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("重试后的响应内容应为 ok，实际 %s", resp.Content)
	}
	if attempts != 2 {
		t.Errorf("应尝试 2 次（1 次 429 + 1 次重试），实际 %d", attempts)
	}
}

func TestProviderDoesNotRetryOn401(t *testing.T) {
	// 确定性错误不重试：401（鉴权失败）重试无意义，应立即透传。
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"401","message":"令牌已过期或验证不正确"}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProvider(srv.URL, "test-model", "test-key")
	_, err := p.Chat([]Message{{Role: roleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if attempts != 1 {
		t.Errorf("401 不应重试，实际尝试 %d 次", attempts)
	}
}
