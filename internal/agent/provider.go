package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAICompatibleProvider 是 LLM 接口的真实实现：调用 OpenAI 兼容格式的
// chat completions API（智谱 BigModel / DeepSeek / 火山 Ark 等均兼容）。
//
// 这是 harness"模型接入层可插拔"的落地：内核只依赖 LLM 接口，换模型 =
// 换 baseURL + model + apiKey，loop / 护栏 / 审批 / 压缩一行不改。
//
// 教学点（mock → 真实的差异）：
//   - mock 是"脚本化假模型"：无网络、无延迟、无错误、无协议细节；
//   - 真实 Provider 必须处理：HTTP 请求/响应、tool_call 的 id 关联、
//     非 2xx 错误解析、JSON 编解码、超时；
//   - 协议适配：mock 用自然语言文本记录工具调用（"调用工具 X"），
//     真实 API 要求 assistant 消息携带结构化的 tool_calls——
//     内核的 Message.ToolCall 字段就是为此而设，适配在本 Provider 完成。
//
// OpenAICompatibleProvider 通过 OpenAI 兼容的 /chat/completions 协议调用真实模型
// （智谱 BigModel / DeepSeek / 火山 Ark 等）。
//
// MaxTokens 控制单次输出预算：**混合思考模型（如 glm-4.7-flash）的 reasoning
// 会先吃掉预算**，max_tokens 太小会导致 reasoning 没写完、content 为空——
// 对"只输出一个级别名"这类短输出任务尤其致命（风险判断要用大预算）。
type OpenAICompatibleProvider struct {
	BaseURL   string // 例如 https://open.bigmodel.cn/api/paas/v4
	Model     string // 例如 glm-4.7-flash（智谱免费模型）
	APIKey    string
	MaxTokens int // 0 = 不传 max_tokens，用服务端默认值
	Client    *http.Client
}

// NewOpenAICompatibleProvider 构造 Provider；baseURL 示例：
//
//	智谱 BigModel: https://open.bigmodel.cn/api/paas/v4
//	DeepSeek:      https://api.deepseek.com/v1
//	火山 Ark:      https://ark.cn-beijing.volces.com/api/v3
func NewOpenAICompatibleProvider(baseURL, model, apiKey string) *OpenAICompatibleProvider {
	return &OpenAICompatibleProvider{
		BaseURL: baseURL,
		Model:   model,
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 60 * time.Second}, // 真实系统要考虑超时：mock 永远不会超时
	}
}

// ---- OpenAI 兼容协议的结构 ----

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Tools     []chatTool    `json:"tools,omitempty"` // 无工具时不传，部分 API 不接受空数组
	MaxTokens *int          `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"` // nil 对应协议里 content:null（tool_calls 消息）
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatToolFunc `json:"function"`
}

// chatToolFunc 的 parameters 是 JSON Schema；mini-harness 的工具没有参数
// 声明（输入是任意 JSON 字符串），用最宽松的 schema：任意对象。
type chatToolFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---- LLM 接口实现 ----

// maxAttempts 是单次 Chat 的最大尝试次数（1 次原始请求 + 重试）。
// 免费模型高峰期常返回 429/5xx，生产 Provider 必须有重试机制——mock 永远不会有这个。
const maxAttempts = 3

// Chat 调用真实 API 并返回 LLMResponse；对瞬时错误（429/5xx/网络）做指数退避重试。
//
// JS/TS ↔ Go 差异：这里把"协议转换"集中在 Provider 内部——TS 生态常用
// fetch 直接拼对象；Go 用 struct tag（json:"..."）声明编解码映射，
// 编译器不能校验 API 侧字段名，错误要到运行期才发现。
func (p *OpenAICompatibleProvider) Chat(messages []Message, tools []Tool) (LLMResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model:     p.Model,
		Messages:  p.toChatMessages(messages),
		Tools:     toChatTools(tools),
		MaxTokens: intPtrOrNil(p.MaxTokens),
	})
	if err != nil {
		return LLMResponse{}, fmt.Errorf("构造请求体失败: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 指数退避：1s、2s（真实系统还会加抖动 jitter 防止惊群）
			time.Sleep(time.Duration(1<<(attempt-1)) * time.Second)
		}
		resp, err := p.doOnce(body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(err) {
			return LLMResponse{}, err // 4xx 等确定性错误：不重试，直接透传
		}
	}
	return LLMResponse{}, fmt.Errorf("模型调用重试 %d 次仍失败: %w", maxAttempts, lastErr)
}

// intPtrOrNil 把 0 值转成 nil（omitempty 不传），避免"0 token"这种非法请求。
func intPtrOrNil(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// apiHTTPError 标记 HTTP 状态错误，并携带服务端 message。
type apiHTTPError struct {
	Status  int
	Message string
}

func (e *apiHTTPError) Error() string {
	return fmt.Sprintf("模型 API 返回 HTTP %d: %s", e.Status, e.Message)
}

// retryable 判断错误是否值得重试：429（限流）、5xx（服务端瞬时错误）、
// 网络错误（连接失败/超时）可重试；4xx（参数/鉴权错误）重试无意义。
func retryable(err error) bool {
	var he *apiHTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusTooManyRequests || he.Status >= http.StatusInternalServerError
	}
	return true
}

// doOnce 发一次请求并解析响应；不包含重试逻辑。
func (p *OpenAICompatibleProvider) doOnce(body []byte) (LLMResponse, error) {
	httpReq, err := http.NewRequest(http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return LLMResponse{}, fmt.Errorf("构造 HTTP 请求失败: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return LLMResponse{}, fmt.Errorf("调用模型 API 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return LLMResponse{}, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// 错误体通常是 {"error":{"code":"401","message":"令牌已过期..."}}，
		// 尽量把服务端 message 透传给调用方，而不是只丢一个状态码。
		var errResp chatResponse
		msg := fmt.Sprintf("模型 API 返回 HTTP %d", resp.StatusCode)
		if json.Unmarshal(raw, &errResp) == nil && errResp.Error != nil {
			msg = fmt.Sprintf("模型 API 返回 HTTP %d: %s", resp.StatusCode, errResp.Error.Message)
		}
		return LLMResponse{}, &apiHTTPError{Status: resp.StatusCode, Message: msg}
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return LLMResponse{}, fmt.Errorf("解析响应 JSON 失败: %w", err)
	}
	if len(cr.Choices) == 0 {
		return LLMResponse{}, fmt.Errorf("模型 API 响应中没有 choices")
	}

	m := cr.Choices[0].Message
	if len(m.ToolCalls) > 0 {
		// 真实模型支持"一轮并行调用多个工具"（tool_calls 数组），全部透传给内核；
		// arguments 是 JSON 字符串，直接透传给工具（mini-harness 工具输入约定）
		calls := make([]ToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			calls = append(calls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: tc.Function.Arguments})
		}
		return LLMResponse{ToolCalls: calls}, nil
	}
	return LLMResponse{Content: m.Content}, nil
}

// toChatMessages 做"内核消息 → OpenAI 协议消息"的适配。
//
// 关键转换：内核回填的 assistant 工具调用是自然语言文本（"调用工具 X"），
// 真实协议要求 assistant 消息带 tool_calls 结构（含 id/name/arguments）——
// 内核已把结构化记录放在 Message.ToolCalls，这里优先读它；读不到才退回文本。
func (p *OpenAICompatibleProvider) toChatMessages(msgs []Message) []chatMessage {
	out := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case roleAssistant:
			if len(m.ToolCalls) > 0 {
				// 结构化工具调用（可多个）：content 置 null（协议要求），带全部 tool_calls
				tcs := make([]toolCall, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					tcs = append(tcs, toolCall{
						ID:   tc.ID,
						Type: "function",
						Function: functionCall{
							Name:      tc.Name,
							Arguments: tc.Input,
						},
					})
				}
				out = append(out, chatMessage{Role: roleAssistant, Content: nil, ToolCalls: tcs})
				continue
			}
			// 普通 assistant 文本（最终回答）
			out = append(out, chatMessage{Role: roleAssistant, Content: strPtr(m.Content)})
		case roleTool:
			// tool 结果消息必须带 tool_call_id（关联上方的 assistant tool_calls）
			out = append(out, chatMessage{Role: roleTool, Content: strPtr(m.Content), ToolCallID: m.ToolCallID})
		default: // user / system
			out = append(out, chatMessage{Role: m.Role, Content: strPtr(m.Content)})
		}
	}
	return out
}

// toChatTools 把内核工具列表转成 OpenAI tools 参数。
func toChatTools(tools []Tool) []chatTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]chatTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, chatTool{
			Type: "function",
			Function: chatToolFunc{
				Name:        t.Name,
				Description: t.Description,
				// 最宽松 schema：允许任意对象参数（mini-harness 工具无参数声明）
				Parameters: map[string]any{
					"type":                 "object",
					"properties":           map[string]any{},
					"additionalProperties": true,
				},
			},
		})
	}
	return out
}

// strPtr 是 string → *string 的小工具。
// JS/TS ↔ Go 差异：Go 没有内置的"取地址"，*string 表示"可为 null"，
// 对应 TS 的 string | null；协议里 content:null 是 tool_calls 消息的规范。
func strPtr(s string) *string { return &s }
