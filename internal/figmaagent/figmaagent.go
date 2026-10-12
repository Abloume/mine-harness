// Package figmaagent 提供 Figma 设计稿读取工具（PLAN 第 3 步：Figma 接入）。
//
// 与 fsagent/cmdagent 同模式：独立包、只依赖 internal/agent 的 Tool 类型，
// 机制归内核、工具归应用层。风险分级（读 vs 导出要区分开）：
//   - figma_get_node:     读取指定节点子树 → 结构化摘要（名称/类型/尺寸/颜色/文本）
//                         RiskNone（纯读，自动放行）
//   - figma_export_image: 导出节点为 PNG/SVG/PDF 图片 URL
//                         RiskMedium（有副作用：消耗 API 配额、可能触发 rate limit、
//                         生成可访问外部 URL；但可撤销——重导即可 → Medium 而非 High）
//   - figma_get_variables:读取设计变量（颜色/字号等 design tokens → 样式变量）
//                         RiskNone（纯读，自动放行）
//
// token 由宿主注入（main.go 从 .env 读 FIGMA_TOKEN），包内不隐式读环境变量——
// 与 fsagent 的 root、cmdagent 的 root 一样是"构造时显式传入"。
//
// JS/TS ↔ Go 差异总览（详见各函数注释）：
//   - JS fetch 不因 HTTP 4xx/5xx 拒绝（要自己 res.ok）；Go http 默认也不报——
//     但 Go 更"裸"：连 body 都不读，非 2xx 全靠自己检查 resp.StatusCode
//   - JS 全量 JSON.parse 再挑字段；Go 声明 struct + json.Unmarshal 自动丢弃
//     未声明字段——处理 Figma 这种 40MB 响应是天然优势（只声明需要的字段，
//     解析器不浪费内存也不让你看到噪音）
//   - JS 字符串 += 拼接常见；Go 字符串不可变，循环拼接要用 strings.Builder
//     （+= 是 O(n²) 拷贝）
//   - 工具风险声明：TS 生态常用装饰器/中间件声明"此操作需审批"；Go 用 struct
//     字段（Tool.BaseRisk）静态声明 + 运行时 RiskEvaluator 修正——声明归声明、
//     判定归判定，职责更清晰。
package figmaagent

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"mine-harness/internal/agent"
)

// 默认值：HTTP 超时 30s（Figma nodes 端点要解析整个文件，大文件慢）；
// 节点摘要的深度/宽度上限——防 40MB 原始 JSON 撑爆上下文（与内核压缩同动机）。
const (
	defaultHTTPTimeout = 30 * time.Second
	defaultMaxDepth    = 4
	defaultMaxChildren = 20
	defaultMaxOutput   = 12000
	baseURL            = "https://api.figma.com/v1"
)

// Tools 是 Figma 工具的宿主，绑定一个文件（fileKey）+ 访问凭据（token）。
type Tools struct {
	fileKey string
	token   string
	baseURL string // 测试时替换为 httptest server
	client  *http.Client
}

// NewTools 构造 Figma 工具。token 是 Figma Personal Access Token（宿主注入）。
func NewTools(fileKey, token string) *Tools {
	return &Tools{
		fileKey: fileKey,
		token:   token,
		baseURL: baseURL,
		client:  &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// All 返回全部工具定义。风险分级：
//   - figma_get_node / figma_get_variables 纯读无副作用 → RiskNone，审批闸不打扰；
//   - figma_export_image 有副作用（消耗配额/可能触发限流/产出外部 URL），
//     但可撤销（重新导出即可）→ RiskMedium：默认审批阈值（RiskMedium）下
//     每次导出都过人工评审闸门——"导图任务交给用户评审"在产品层的落地。
//     这是"工具作者声明 + 内核判定"的分工：包内不自己弹窗，只把风险级别说清楚。
func (t *Tools) All() []agent.Tool {
	return []agent.Tool{
		{
			Name: "figma_get_node",
			Description: "读取 Figma 文件指定节点（逗号分隔多个 id）的子树摘要：节点名/类型/尺寸/填充颜色/文本内容。" +
				"参数 JSON：{\"ids\":\"0:1\",\"max_depth\":可选层数,\"max_children\":可选每层节点数}。" +
				"只返回结构化摘要而非原始 JSON（大文件原始响应可达数十 MB）。",
			BaseRisk: agent.RiskNone,
			Execute:  t.getNode,
		},
		{
			Name: "figma_export_image",
			Description: "导出 Figma 节点为图片 URL（png/svg/pdf，默认 png）。" +
				"参数 JSON：{\"ids\":\"0:1,1:2\",\"format\":\"png|svg|pdf\",\"scale\":可选倍率}。" +
				"返回可直接访问的图片链接（可下载落盘或交给视觉模型验证）。" +
				"注意：导出有副作用（消耗 API 配额、可能触发限流），执行前会触发人工审批。",
			BaseRisk: agent.RiskMedium, // 导图需人工评审（有副作用但可撤销）；读取类保持 RiskNone
			Execute:  t.exportImage,
		},
		{
			Name: "figma_get_variables",
			Description: "读取 Figma 文件的设计变量（design tokens）：颜色/字号等变量名与取值，" +
				"可用于生成样式变量/主题。注意：该端点需要 file_variables:read scope，" +
				"Free 计划 token 可能无此权限（返回 403 属预期）。参数 JSON：{}。",
			BaseRisk: agent.RiskNone,
			Execute:  t.getVariables,
		},
	}
}

// ---- figma_get_node ----

type getNodeReq struct {
	IDs         string `json:"ids"`                    // 逗号分隔，如 "0:1,52:3253"
	MaxDepth    int    `json:"max_depth,omitempty"`    // 0 = 默认 4
	MaxChildren int    `json:"max_children,omitempty"` // 0 = 默认 20
}

// nodes 端点的响应中我们只声明需要的字段。
// JS/TS 对比：TS 要么 any、要么手写完整 interface 全量对齐；Go struct 只声明
// 用到的字段，json.Unmarshal 忽略其余——Figma 单节点文档可能带几十个字段，
// 这里 6 个就够，天然省内存省解析时间。
type nodesResp struct {
	Name  string              `json:"name"`
	Nodes map[string]nodeInfo `json:"nodes"`
}

type nodeInfo struct {
	Document figmaNode `json:"document"`
}

type figmaNode struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	BBox       *boundingBox `json:"absoluteBoundingBox"`
	Fills      []fill       `json:"fills"`
	Characters string       `json:"characters"`
	Children   []figmaNode  `json:"children"`
}

type boundingBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type fill struct {
	Visible bool   `json:"visible"`
	Type    string `json:"type"`
	Color   *color `json:"color"`
}

// color 的 RGB 是 0..1 浮点，转 hex 前要乘 255 取整。
type color struct {
	R float64 `json:"r"`
	G float64 `json:"g"`
	B float64 `json:"b"`
	A float64 `json:"a"`
}

func (t *Tools) getNode(input string) (string, error) {
	var req getNodeReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"ids\":\"0:1\"}）", err)
	}
	if strings.TrimSpace(req.IDs) == "" {
		return "", fmt.Errorf("ids 不能为空（逗号分隔，如 \"0:1\"）")
	}
	maxDepth, maxChildren := defaultMaxDepth, defaultMaxChildren
	if req.MaxDepth > 0 {
		maxDepth = req.MaxDepth
	}
	if req.MaxChildren > 0 {
		maxChildren = req.MaxChildren
	}

	q := t.baseURL + "/files/" + t.fileKey + "/nodes?ids=" + normalizeIDs(req.IDs)
	var resp nodesResp
	if err := t.doJSON(http.MethodGet, q, &resp); err != nil {
		return "", err
	}

	if len(resp.Nodes) == 0 {
		return "", fmt.Errorf("未找到节点 %q（检查 id 是否正确、文件是否对 token 可见）", req.IDs)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "文件: %s\n", resp.Name)
	for id, info := range resp.Nodes {
		fmt.Fprintf(&b, "=== 节点 %s ===\n", id)
		// 深度优先：栈/显式 depth 传参（JS 递归同理，但 Go 注意别隐式共享状态）
		writeNodeTree(&b, info.Document, 0, maxDepth, maxChildren)
	}
	out := b.String()
	if len(out) > defaultMaxOutput {
		out = truncateKeepEdges(out, defaultMaxOutput)
	}
	return out, nil
}

// writeNodeTree 深度优先输出节点树摘要。depth 从 0 开始，超过 maxDepth 或
// 每层超过 maxChildren 就省略——防"267 个直接子节点的页面"撑爆输出。
func writeNodeTree(b *strings.Builder, n figmaNode, depth, maxDepth, maxChildren int) {
	if depth > maxDepth {
		fmt.Fprintf(b, "%s…（超出深度 %d，已省略）\n", indent(depth), maxDepth)
		return
	}
	line := fmt.Sprintf("%s[%s] %s %q", indent(depth), n.ID, n.Type, n.Name)
	if n.BBox != nil {
		line += fmt.Sprintf(" %dx%d", int(n.BBox.Width), int(n.BBox.Height))
	}
	if c := fillColorHex(n.Fills); c != "" {
		line += " 填充=" + c
	}
	if n.Type == "TEXT" && n.Characters != "" {
		// 文本只取前 40 字，防长文案刷屏。
		// Go 坑（JS/TS 对比）：len(s) 是"字节数"不是"字符数"——中文 UTF-8 每
		// 字 3 字节，直接 s[:40] 会截断半个汉字（输出乱码 �）。必须 []rune 转
		// 换后按字符截。JS 的 .length 是 UTF-16 码元（中文 1 码元，勉强可用），
		// 但遇到 emoji 同样会劈开——JS 里正确做法也是 Array.from(s)。
		chars := n.Characters
		runes := []rune(chars)
		if len(runes) > 40 {
			chars = string(runes[:40]) + "…"
		}
		line += " 文本=\"" + chars + "\""
	}
	b.WriteString(line)
	b.WriteString("\n")

	kids := n.Children
	if len(kids) > maxChildren {
		fmt.Fprintf(b, "%s（%d 个子节点，仅展示前 %d）\n", indent(depth+1), len(kids), maxChildren)
		kids = kids[:maxChildren]
	}
	for _, c := range kids {
		writeNodeTree(b, c, depth+1, maxDepth, maxChildren)
	}
}

// fillColorHex 提取第一个可见的纯色填充为 #RRGGBB（含透明度时带 /opacity）。
// Figma 的 color 是 0..1 浮点；hex 是前端工程师的日常格式，输出直接用。
func fillColorHex(fills []fill) string {
	for _, f := range fills {
		if !f.Visible || f.Type != "SOLID" || f.Color == nil {
			continue
		}
		c := f.Color
		hex := fmt.Sprintf("#%02X%02X%02X",
			int(math.Round(c.R*255)), int(math.Round(c.G*255)), int(math.Round(c.B*255)))
		if c.A < 1 {
			hex += fmt.Sprintf(" α%.2f", c.A)
		}
		return hex
	}
	return ""
}

// ---- figma_export_image ----

type exportReq struct {
	IDs    string `json:"ids"`
	Format string `json:"format"` // png | svg | pdf（默认 png）
	Scale  int    `json:"scale"`  // png 倍率 0.5/1/2/3/4（默认 1）
}

// images 导出端点（/v1/images/:key）真实响应（实测）：
// {"err":null,"images":{"<node-id>":"<url>"}}——顶层 images 是 node id → URL 映射，
// 未渲染节点 value 为 null。注意：这与 /v1/files/:key/images（image fills，meta.images
// 包 imageRef）是两套结构，别混。
type imagesResp struct {
	Err    any               `json:"err"`
	Images map[string]string `json:"images"`
}

func (t *Tools) exportImage(input string) (string, error) {
	var req exportReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"ids\":\"0:1\",\"format\":\"png\"}）", err)
	}
	if strings.TrimSpace(req.IDs) == "" {
		return "", fmt.Errorf("ids 不能为空")
	}
	format := req.Format
	if format == "" {
		format = "png"
	}
	switch format {
	case "png", "svg", "pdf":
	default:
		return "", fmt.Errorf("format 仅支持 png/svg/pdf，实际 %q", format)
	}
	scale := req.Scale
	if scale == 0 {
		scale = 1
	}

	// 节点导出走 /v1/images/:key（注意：没有 /files/ 段）。
	// 踩坑实录：/v1/files/:key/images 是另一个端点（image fills——返回文件内
	// 所有图片引用的 URL 映射，无 ids 参数），打错端点会"成功"但返回一堆无关
	// 图片且 ids 被静默忽略——HTTP 200 不代表参数生效，这是 REST API 的经典陷阱。
	q := fmt.Sprintf("%s/images/%s?ids=%s&format=%s&scale=%d",
		t.baseURL, t.fileKey, normalizeIDs(req.IDs), format, scale)
	var resp imagesResp
	if err := t.doJSON(http.MethodGet, q, &resp); err != nil {
		return "", err
	}
	if resp.Err != nil {
		return "", fmt.Errorf("Figma 导出失败: %v", resp.Err)
	}
	if len(resp.Images) == 0 {
		return "", fmt.Errorf("导出失败：未返回图片（节点可能不可导出，如容器节点无渲染内容）")
	}

	var b strings.Builder
	b.WriteString("图片导出 URL（可下载落盘，或交给视觉模型验证）：\n")
	ready := 0
	for id, u := range resp.Images {
		if u == "" {
			// Figma 对未渲染过的节点返回 null：图片仍在生成，需稍后重试该节点
			fmt.Fprintf(&b, "  %s → （尚未渲染，稍后重试导出）\n", id)
			continue
		}
		ready++
		fmt.Fprintf(&b, "  %s → %s\n", id, u)
	}
	if ready == 0 {
		return "", fmt.Errorf("导出失败：所有请求节点均未渲染（Figma 图片生成是异步的，稍后重试）")
	}
	return b.String(), nil
}

// ---- figma_get_variables ----

// variables 响应：meta.variables 是变量 id → 变量定义。
type variablesResp struct {
	Meta struct {
		Variables map[string]figmaVariable `json:"variables"`
	} `json:"meta"`
}

type figmaVariable struct {
	Name         string         `json:"name"`
	Type         string         `json:"type"` // COLOR | FLOAT | STRING | BOOLEAN
	ResolvedType string         `json:"resolvedType"`
	ValuesByMode map[string]any `json:"valuesByMode"`
}

func (t *Tools) getVariables(input string) (string, error) {
	// input 预期是 "{}"，忽略内容（无参数工具）
	// 读取本地变量端点：/variables/local（踩坑实录：/variables 不带 /local 是
	// 404——文档里读变量只有 /local 变体，不带的是创建变量 POST 端点）
	q := t.baseURL + "/files/" + t.fileKey + "/variables/local"
	var resp variablesResp
	if err := t.doJSON(http.MethodGet, q, &resp); err != nil {
		return "", err
	}
	if len(resp.Meta.Variables) == 0 {
		return "该文件未定义设计变量（variables）。", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "设计变量（共 %d 个）：\n", len(resp.Meta.Variables))
	for _, v := range resp.Meta.Variables {
		// 只取第一个 mode 的值（默认 mode）——教学简化；生产按需展开所有 mode
		for _, raw := range v.ValuesByMode {
			val := formatVariableValue(v, raw)
			fmt.Fprintf(&b, "  %s (%s) = %s\n", v.Name, v.Type, val)
			break
		}
	}
	return b.String(), nil
}

// formatVariableValue 把变量的原始值转成可读文本：COLOR 转 hex，其余直接展示。
func formatVariableValue(v figmaVariable, raw any) string {
	if v.ResolvedType == "COLOR" {
		if m, ok := raw.(map[string]any); ok {
			r := toFloat(m["r"])
			g := toFloat(m["g"])
			bl := toFloat(m["b"])
			return fmt.Sprintf("#%02X%02X%02X", int(math.Round(r*255)), int(math.Round(g*255)), int(math.Round(bl*255)))
		}
	}
	return fmt.Sprintf("%v", raw)
}

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// ---- 公共：HTTP + 工具函数 ----

// doJSON 发 GET 请求并解析 JSON 响应。
//
// JS/TS ↔ Go 对比（本函数集中了 Go HTTP 的三个"陷阱"）：
//   - ① 非 2xx 不自动报错：fetch 有 res.ok，Go 连这都没有——必须显式检查
//     resp.StatusCode，否则 403 的 JSON 会被当成正常结果解析（还解析不出错）
//   - ② 响应体必须 Close：JS 不用管；Go 不关 body 会泄漏连接（长跑 agent 必炸）
//   - ③ 错误信息要"对模型有用"：403/404/429 返回中文提示（权限/文件不存在/
//     rate limit），模型才知道下一步（要 token 权限、换文件、稍后重试）
func (t *Tools) doJSON(method, url string, out any) error {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败: %v", err)
	}
	req.Header.Set("X-Figma-Token", t.token)

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Figma API 失败: %v", err)
	}
	defer resp.Body.Close() // 坑②：不关泄漏连接

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500)) // 错误体通常小，读 500B 够
		return figmaStatusError(resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解析响应失败: %v", err)
	}
	return nil
}

// figmaStatusError 把 HTTP 状态码转成对模型有用的错误（坑③）。
func figmaStatusError(code int, body []byte) error {
	hint := map[int]string{
		401: "token 无效或已过期（重新生成 Figma Personal Access Token）",
		403: "token 权限不足：文件不可见，或端点所需 scope 未授权（详情见响应体，如 file_variables:read）",
		404: "文件或节点不存在（检查 fileKey / node id）",
		429: "请求过频触发 rate limit（稍后重试；免费档限制较严）",
	}
	msg, ok := hint[code]
	if !ok {
		msg = "未知错误"
	}
	detail := strings.TrimSpace(string(body))
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return fmt.Errorf("Figma API HTTP %d（%s）: %s", code, msg, detail)
}

// normalizeIDs 把用户输入的 id 列表规整为 Figma 接受的逗号分隔格式。
// 冒号（0:1）在 query 值里是合法字符，无需转义（JS 的 encodeURIComponent
// 在这里也多余）；用户可能写 "0:1, 1:2" 带空格——按逗号拆、逐段 TrimSpace、
// 再重组（"0:1, 52:3253" → "0:1,52:3253"，且不会出现双逗号）。
// JS/TS 对比：JS 常用 split().map().filter().join() 链；Go 没有一等链式，
// 手写循环 + 显式 append——可读性不差，且零分配技巧（预分配 out）在热路径有意义。
func normalizeIDs(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

// truncateKeepEdges 保留头尾、中间省略——与 cmdagent 同动机：防长输出
// 撑爆上下文，并显式标注截断让模型知道自己没看到全文。
func truncateKeepEdges(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max / 2
	tail := max - head
	return s[:head] + fmt.Sprintf("\n...[已截断 %d 字符，保留头 %d / 尾 %d]...\n", len(s)-max, head, tail) + s[len(s)-tail:]
}

// indent 生成树状缩进。
func indent(depth int) string {
	return strings.Repeat("  ", depth)
}
