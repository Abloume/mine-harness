package figmaagent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mine-harness/internal/agent"
)

// newMockTools 起一个 httptest server 模拟 Figma API，返回绑好 baseURL 的 Tools。
// 测试不依赖真实网络/token——mock 返回精心构造的响应，覆盖解析与错误分支。
func newMockTools(t *testing.T, handler http.HandlerFunc) *Tools {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	tools := NewTools("test-file-key", "test-token")
	tools.baseURL = srv.URL
	return tools
}

// sampleNodes 是 nodes 端点的迷你响应：画布 → 一个 Frame（带颜色、尺寸）→ 一个 TEXT。
func sampleNodes(t *testing.T) string {
	t.Helper()
	doc := map[string]any{
		"name": "示例品牌项目",
		"nodes": map[string]any{
			"0:1": map[string]any{
				"document": map[string]any{
					"id":   "0:1",
					"name": "Page 1",
					"type": "CANVAS",
					"children": []any{
						map[string]any{
							"id":   "52:3253",
							"name": "产品目录页",
							"type": "FRAME",
							"absoluteBoundingBox": map[string]any{
								"x": 0.0, "y": 0.0, "width": 1440.0, "height": 1024.0,
							},
							"fills": []any{
								map[string]any{
									"type": "SOLID", "visible": true,
									"color": map[string]any{"r": 0.13, "g": 0.45, "b": 0.82, "a": 1.0},
								},
							},
							"children": []any{
								map[string]any{
									"id": "52:3254", "name": "标题", "type": "TEXT",
									// 42 个 rune（18 前缀 + 24）：超过 40 字截断阈值，可验证截断逻辑
									"characters": "示例品牌项目 2026 主视觉标题文案" + strings.Repeat("很长", 12),
									"fills":      []any{},
								},
							},
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGetNodeSummarizes(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sampleNodes(t))
	})
	out, err := tools.getNode(`{"ids":"0:1"}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, want := range []string{
		"文件: 示例品牌项目",
		"=== 节点 0:1 ===",
		`[0:1] CANVAS "Page 1"`,
		`[52:3253] FRAME "产品目录页" 1440x1024`,
		"填充=#2173D1", // 0.13*255≈33=0x21, 0.45*255≈115=0x73, 0.82*255≈209=0xD1
		// 42 字文本截断为前 40 字 + 省略号（Go 按 rune 截，中文不会劈成乱码）
		`文本="` + "示例品牌项目 2026 主视觉标题文案" + strings.Repeat("很长", 10) + "很…\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出应包含 %q，实际:\n%s", want, out)
		}
	}
}

// TestGetNodeLimitsDepth 验证 max_depth 生效：深层节点被省略并标注。
func TestGetNodeLimitsDepth(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sampleNodes(t))
	})
	out, err := tools.getNode(`{"ids":"0:1","max_depth":1}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// depth=1 时 TEXT（第 3 层）不应出现，且标注省略
	if strings.Contains(out, `[52:3254]`) {
		t.Errorf("max_depth=1 不应展示深层 TEXT 节点，实际:\n%s", out)
	}
	if !strings.Contains(out, "超出深度") {
		t.Errorf("超出深度应显式标注，实际:\n%s", out)
	}
}

// TestGetNodeTextTruncated 验证超长文本被截断（防刷屏）。
func TestGetNodeTextTruncated(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sampleNodes(t))
	})
	out, err := tools.getNode(`{"ids":"0:1"}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 完整 42 字文案不应出现；截断应带省略号
	full := "主视觉标题文案" + strings.Repeat("很长", 12)
	if strings.Contains(out, full) {
		t.Errorf("超长文本应截断（不应含完整 %d 字），实际:\n%s", len([]rune(full)), out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("截断应带省略号，实际:\n%s", out)
	}
}

// TestGetNodeBadInput 验证参数校验：空 ids 直接报错，不浪费一次 API 调用。
func TestGetNodeBadInput(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数非法时不应发 HTTP 请求")
	})
	if _, err := tools.getNode(`{"ids":""}`); err == nil {
		t.Fatal("空 ids 应报错")
	}
	if _, err := tools.getNode(`not-json`); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// TestGetNodeHTTPError 验证 403/404/429 返回对模型有用的中文提示。
func TestGetNodeHTTPError(t *testing.T) {
	for code, want := range map[int]string{
		401: "token 无效",
		403: "权限不足",
		404: "文件或节点不存在",
		429: "rate limit",
	} {
		tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			fmt.Fprint(w, `{"message":"boom"}`)
		})
		_, err := tools.getNode(`{"ids":"0:1"}`)
		if err == nil {
			t.Fatalf("HTTP %d 应报错", code)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("HTTP %d 错误应含 %q，实际: %v", code, want, err)
		}
	}
}

func TestExportImage(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		// 节点导出必须走 /images/:key（不是 /files/:key/images）
		if r.URL.Path != "/images/test-file-key" {
			t.Errorf("应请求 /images/:key，实际 path: %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "format=png") || !strings.Contains(r.URL.RawQuery, "scale=2") {
			t.Errorf("应带 format=png&scale=2，实际 query: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"err":null,"images":{"52:3253":"https://example.com/render/52_3253.png"}}`)
	})
	out, err := tools.exportImage(`{"ids":"52:3253","format":"png","scale":2}`)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !strings.Contains(out, "https://example.com/render/52_3253.png") {
		t.Errorf("应返回图片 URL，实际:\n%s", out)
	}
}

// TestExportImagePending 验证未渲染节点（null）被明确标注，不静默消失。
func TestExportImagePending(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"err":null,"images":{"52:3253":null,"52:3254":"https://example.com/ok.png"}}`)
	})
	out, err := tools.exportImage(`{"ids":"52:3253,52:3254"}`)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !strings.Contains(out, "尚未渲染") {
		t.Errorf("null 节点应标注尚未渲染，实际:\n%s", out)
	}
	if !strings.Contains(out, "https://example.com/ok.png") {
		t.Errorf("已渲染节点应返回 URL，实际:\n%s", out)
	}
}

// TestExportImageAllPending 验证全部未渲染时明确报错（模型知道要重试）。
func TestExportImageAllPending(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"err":null,"images":{"52:3253":null}}`)
	})
	if _, err := tools.exportImage(`{"ids":"52:3253"}`); err == nil || !strings.Contains(err.Error(), "未渲染") {
		t.Fatalf("全部未渲染应报错提示重试，实际: %v", err)
	}
}

// TestExportImageAPIError 验证 err 非空时明确报错。
func TestExportImageAPIError(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"err":{"message":"node not found"},"images":{}}`)
	})
	_, err := tools.exportImage(`{"ids":"999:999"}`)
	if err == nil || !strings.Contains(err.Error(), "导出失败") {
		t.Fatalf("err 非空应报错，实际: %v", err)
	}
}

func TestExportImageBadFormat(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("format 非法时不应发 HTTP 请求")
	})
	if _, err := tools.exportImage(`{"ids":"0:1","format":"webp"}`); err == nil {
		t.Fatal("webp 应被拒绝（仅 png/svg/pdf）")
	}
}

// TestGetVariables 验证变量读取：COLOR 转 hex，其余类型原样展示。
func TestGetVariables(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/variables/local") {
			t.Errorf("读变量应走 /variables/local，实际 path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{
			"meta": {
				"variables": {
					"v1": {"name": "品牌主色", "type": "COLOR", "resolvedType": "COLOR",
						"valuesByMode": {"m1": {"r": 1.0, "g": 0.2, "b": 0.2, "a": 1.0}}},
					"v2": {"name": "标题字号", "type": "FLOAT", "resolvedType": "FLOAT",
						"valuesByMode": {"m1": 32}}
				}
			}
		}`)
	})
	out, err := tools.getVariables(`{}`)
	if err != nil {
		t.Fatalf("变量解析失败: %v", err)
	}
	if !strings.Contains(out, "品牌主色 (COLOR) = #FF3333") { // 1.0→FF, 0.2→33
		t.Errorf("COLOR 变量应转 hex，实际:\n%s", out)
	}
	if !strings.Contains(out, "标题字号 (FLOAT) = 32") {
		t.Errorf("FLOAT 变量应原样展示，实际:\n%s", out)
	}
}

func TestGetVariablesEmpty(t *testing.T) {
	tools := newMockTools(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"meta":{"variables":{}}}`)
	})
	out, err := tools.getVariables(`{}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(out, "未定义设计变量") {
		t.Errorf("空变量应友好提示，实际:\n%s", out)
	}
}

func TestTruncateKeepEdges(t *testing.T) {
	// 大输入（2000 字节）截断到 100：显著压缩且保留头尾
	in := strings.Repeat("a", 1996) + "TAIL"
	out := truncateKeepEdges(in, 100)
	if !strings.Contains(out, "已截断") || !strings.HasSuffix(out, "TAIL") {
		t.Errorf("应保留头尾并标注截断，实际:\n%s", out)
	}
	if len(out) >= len(in)/2 {
		t.Errorf("截断后应显著小于原文（2000→%d），实际 %d", len(in), len(out))
	}
	// 短文本不截断
	if got := truncateKeepEdges("short", 100); got != "short" {
		t.Errorf("短文本不应截断，实际 %q", got)
	}
}

// TestNormalizeIDs 验证 id 列表空格 → 逗号规整。
func TestNormalizeIDs(t *testing.T) {
	if got := normalizeIDs("0:1, 52:3253"); got != "0:1,52:3253" {
		t.Errorf("空格应规整为逗号，实际 %q", got)
	}
}

// TestToolRiskDeclaration 验证工具风险分级声明（不依赖网络，纯本地断言）：
//   - get_node / get_variables 纯读 → RiskNone，审批闸自动放行；
//   - export_image 有副作用（消耗配额/可能触发限流/产出外部 URL）→ RiskMedium，
//     达到内核默认审批阈值（RiskMedium）即进人工评审闸门。
//
// 这是"导图任务交用户评审"的产品层落地：工具作者只声明级别，判定/拦截归内核。
// JS/TS ↔ Go 差异：TS 里"此操作需审批"常用装饰器或中间件标注；Go 用 struct 字段
// （Tool.BaseRisk）静态声明，配合内核 RiskEvaluator 运行时修正——声明与判定分离。
func TestToolRiskDeclaration(t *testing.T) {
	tools := NewTools("test-file-key", "test-token")
	want := map[string]agent.RiskLevel{
		"figma_get_node":      agent.RiskNone,
		"figma_export_image":  agent.RiskMedium,
		"figma_get_variables": agent.RiskNone,
	}
	got := map[string]agent.RiskLevel{}
	for _, tl := range tools.All() {
		got[tl.Name] = tl.BaseRisk
	}
	for name, lv := range want {
		if got[name] != lv {
			t.Errorf("工具 %s 基础风险应声明为 %v，实际 %v", name, lv, got[name])
		}
	}
	if len(got) != len(want) {
		t.Errorf("工具数量不符：应有 %d 个，实际 %d（%v）", len(want), len(got), got)
	}
}

// TestExportImageApprovalGate 端到端验证"导图需人工评审"在产品层生效：
// agent 内核 + figma 工具 + 审批决策器组合跑完整 loop，证明
// figma_export_image 达审批阈值（RiskMedium）后：
//   - DenyApprover（拒绝）→ 工具不执行（mock server 零请求），历史回填"被拒绝"；
//   - AutoApprover（放行）→ 工具执行（mock server 收到导出请求）。
//
// 说明：纯读工具（figma_get_node）低于阈值自动放行，不经过此闸门（见
// TestToolRiskDeclaration 的风险声明）；这里只验证导出这一个带副作用动作。
// JS/TS ↔ Go 差异：这类横切关注点在 TS 生态常靠中间件/装饰器隐式拦截；
// Go 里"声明（BaseRisk）+ 判定（loop 审批点）+ 决策器（Approver）"是显式
// 组装的三段式，调用方用 WithApprover 表达意图，职责边界更清楚。
func TestExportImageApprovalGate(t *testing.T) {
	newAgent := func(ap agent.Approver) (*agent.Agent, *int) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			fmt.Fprint(w, `{"err":null,"images":{"1:2":"http://example.com/out.png"}}`)
		}))
		t.Cleanup(srv.Close)
		tools := NewTools("test-file-key", "test-token")
		tools.baseURL = srv.URL
		reg := agent.NewRegistry()
		for _, tl := range tools.All() {
			reg.Register(tl)
		}
		llm := agent.NewMockLLM([]agent.MockDecision{
			{ToolName: "figma_export_image", ToolInput: `{"ids":"1:2","format":"png"}`},
			{Content: "导出流程结束"},
		})
		a := agent.NewAgent(llm, reg, agent.WithMaxSteps(3), agent.WithApprover(ap))
		return a, &hits
	}

	t.Run("拒绝则不执行", func(t *testing.T) {
		a, hits := newAgent(agent.DenyApprover{})
		r := a.Run("导出节点 1:2 的图片")
		if *hits != 0 {
			t.Errorf("审批拒绝后工具不应执行（mock server 收到 %d 次请求）", *hits)
		}
		if r.Status != agent.StatusCompleted {
			t.Errorf("拒绝是软停止（可继续对话），应 completed，实际 %s / %s", r.Status, r.Reason)
		}
		rejected := false
		for _, m := range a.History() {
			if strings.Contains(m.Content, "被拒绝") {
				rejected = true
				break
			}
		}
		if !rejected {
			t.Error("拒绝结果应以 tool 消息回填给模型（含'被拒绝'），否则模型不知道动作未执行")
		}
	})

	t.Run("放行才执行", func(t *testing.T) {
		a, hits := newAgent(agent.AutoApprover{})
		r := a.Run("导出节点 1:2 的图片")
		if *hits != 1 {
			t.Errorf("审批通过后工具应执行（mock server 收到 %d 次请求，期望 1）", *hits)
		}
		if r.Status != agent.StatusCompleted {
			t.Errorf("应正常完成，实际 %s / %s", r.Status, r.Reason)
		}
	})
}
