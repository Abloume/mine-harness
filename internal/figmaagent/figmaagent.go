// Package figmaagent 提供 Figma 设计稿读取工具（PLAN 第 3 步：Figma 接入）。
//
// 与 fsagent/cmdagent 同模式：独立包、只依赖 internal/agent 的 Tool 类型，
// 机制归内核、工具归应用层。风险分级（读 vs 导出要区分开）：
//   - figma_get_node:     读取指定节点子树 → 结构化摘要（名称/类型/尺寸/颜色/文本）
//     RiskNone（纯读，自动放行）
//   - figma_export_image: 导出节点为 PNG/SVG/PDF 图片 URL
//     RiskMedium（有副作用：消耗 API 配额、可能触发 rate limit、
//     生成可访问外部 URL；但可撤销——重导即可 → Medium 而非 High）
//   - figma_get_variables:读取设计变量（颜色/字号等 design tokens → 样式变量）
//     RiskNone（纯读，自动放行）
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
		{
			Name: "figma_classify_node",
			Description: "对指定节点做资产决策分类（自动决策管线第 1 步）：按决策树判断每个节点" +
				"\"该导出图片还是写 CSS\"——含 IMAGE 位图填充 → 导出 PNG；TEXT/简单矢量 → CSS 还原；" +
				"复杂嵌套 → 逐层递归。返回每节点的 kind/recommendation/reason，以及 review_required" +
				"（数据不足或需要人工评审的点）。参数 JSON：{\"ids\":\"0:1\"}。纯读无副作用。",
			BaseRisk: agent.RiskNone,
			Execute:  t.classifyNode,
		},
		{
			Name: "figma_plan_page",
			Description: "自动决策管线编排（第 2 步）：输入页面节点 id，输出完整还原计划——" +
				"① 位图资产清单（需导出 PNG 的节点：id/名称/尺寸/理由）；② CSS 样式 token 清单" +
				"（颜色/尺寸/圆角，从节点数据提取）；③ 评审点清单（reviewPoints：需人工评审或" +
				"行不通的点，如导出需审批、REST 429 需浏览器接管、数据不足）。" +
				"参数 JSON：{\"node_id\":\"0:1\",\"max_depth\":可选层数}。纯读无副作用。" +
				"原则：能自动决策的全自动，不懂的/行不通的显式列出交给用户评审。",
			BaseRisk: agent.RiskNone,
			Execute:  t.planPage,
		},
	}
}

// ---- figma_get_node ----

// fetchNodes 是 nodes 端点的公共取数入口：getNode（文本摘要）、classifyNode（资产决策）、
// planPage（编排）都复用同一份解析——JS/TS 对比：避免每个工具各写一遍 HTTP+解析，
// Go 用显式 helper 复用，接口收敛在包内，外部只暴露工具。
func (t *Tools) fetchNodes(ids string) (nodesResp, error) {
	q := t.baseURL + "/files/" + t.fileKey + "/nodes?ids=" + normalizeIDs(ids)
	var resp nodesResp
	if err := t.doJSON(http.MethodGet, q, &resp); err != nil {
		return nodesResp{}, err
	}
	if len(resp.Nodes) == 0 {
		return nodesResp{}, fmt.Errorf("未找到节点 %q（检查 id 是否正确、文件是否对 token 可见）", ids)
	}
	return resp, nil
}

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
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	BBox         *boundingBox `json:"absoluteBoundingBox"`
	Fills        []fill       `json:"fills"`
	Characters   string       `json:"characters"`
	CornerRadius *float64     `json:"cornerRadius"` // RECTANGLE/FRAME 圆角（plan_page 提 token 用）
	Effects      []effect     `json:"effects"`      // 阴影/模糊等（token 提取用，只取类型枚举）
	Children     []figmaNode  `json:"children"`
}

type effect struct {
	Type    string `json:"type"` // DROPSHADOW / INNER_SHADOW / BACKGROUND_BLUR ...
	Visible bool   `json:"visible"`
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

	resp, err := t.fetchNodes(req.IDs)
	if err != nil {
		return "", err
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

// ---- 自动决策管线：figma_classify_node / figma_plan_page ----
//
// 目标（产品层）："调用 mine-harness 做前端页面开发时，能自动决策，不懂的/行不通的
// 再让用户评审"。这里把协作中沉淀的决策树固化为代码：
//   1. 含 IMAGE 位图填充（照片/AI 纹理/插画）→ 导出 PNG（CSS 复刻不了）
//   2. 文本 / 纯色 / 渐变矢量 → CSS 手动还原（取样式 token）
//   3. 容器节点 → 递归逐子决策
//   4. 数据不足（fills 缺失/API 异常）→ 升级人工评审（review_required）
// 每个决策都带可解释的 Reason——评审时用户一眼能看懂"为什么这么定"。
// JS/TS ↔ Go 对比：JS 常把这类规则写在 LLM prompt 里（自然语言决策，不可测试）；
// Go 固化成本地类型 + 纯函数，可用单测逐分支验证，错误路径显式枚举。

// AssetKind 是资产类型（决策树第一层判断结果）。
type AssetKind string

const (
	AssetImage     AssetKind = "image"     // 位图填充 → 导出 PNG
	AssetVector    AssetKind = "vector"    // 矢量/纯色/渐变 → CSS 还原
	AssetText      AssetKind = "text"      // 文本 → CSS 还原
	AssetComposite AssetKind = "composite" // 容器 → 递归逐子
	AssetUnknown   AssetKind = "unknown"   // 数据不足 → 人工评审
)

// AssetDecision 是单节点的资产决策。ReviewRequired 命中时 ReviewNote 说明
// 需要用户评审的具体点（导出审批 / 429 浏览器接管 / 数据缺失）。
type AssetDecision struct {
	NodeID         string
	Name           string
	Kind           AssetKind
	Recommendation string // export_png / svg_inline / css_manual / recurse / review
	Size           string // 宽x高（BBox 存在时）
	Reason         string
	ReviewRequired bool
	ReviewNote     string
}

// ---- figma_classify_node ----

type classifyReq struct {
	IDs string `json:"ids"`
}

func (t *Tools) classifyNode(input string) (string, error) {
	var req classifyReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"ids\":\"0:1\"}）", err)
	}
	if strings.TrimSpace(req.IDs) == "" {
		return "", fmt.Errorf("ids 不能为空（逗号分隔，如 \"0:1\"）")
	}
	resp, err := t.fetchNodes(req.IDs)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for id, info := range resp.Nodes {
		fmt.Fprintf(&b, "=== 节点 %s 资产决策 ===\n", id)
		writeDecisions(&b, info.Document, 0, defaultMaxDepth)
	}
	return b.String(), nil
}

// writeDecisions 深度优先输出每层节点的资产决策。depth 超限时显式标注省略——
// 与 getNode 同动机：防超大文件撑爆输出，且模型/用户知道自己没看到全貌。
func writeDecisions(b *strings.Builder, n figmaNode, depth, maxDepth int) {
	if depth > maxDepth {
		fmt.Fprintf(b, "%s…（超出深度 %d，已省略）\n", indent(depth), maxDepth)
		return
	}
	d := decideAsset(n)
	flag := ""
	if d.ReviewRequired {
		flag = " ⚠️ 需评审"
	}
	fmt.Fprintf(b, "%s[%s] %s（%s）→ %s%s\n", indent(depth), d.NodeID, d.Name, d.Kind, d.Recommendation, flag)
	fmt.Fprintf(b, "%s  原因: %s\n", indent(depth), d.Reason)
	if d.Size != "" {
		fmt.Fprintf(b, "%s  尺寸: %s\n", indent(depth), d.Size)
	}
	if d.ReviewRequired {
		fmt.Fprintf(b, "%s  评审点: %s\n", indent(depth), d.ReviewNote)
	}
	for _, c := range n.Children {
		writeDecisions(b, c, depth+1, maxDepth)
	}
}

// decideAsset 是决策树的核心纯函数：一个节点 → 资产分类 + 行动建议 + 可解释理由。
// 顺序即优先级：位图 → 文本 → 容器 → 纯色/渐变矢量 → 数据缺失兜底。
func decideAsset(n figmaNode) AssetDecision {
	d := AssetDecision{NodeID: n.ID, Name: n.Name}
	if n.BBox != nil {
		d.Size = fmt.Sprintf("%dx%d", int(n.BBox.Width), int(n.BBox.Height))
	}
	switch {
	case hasFillType(n.Fills, "IMAGE"):
		d.Kind = AssetImage
		d.Recommendation = "export_png"
		d.Reason = "含 IMAGE 位图填充（照片/AI 纹理/插画），CSS 无法复刻，导出 PNG 挂载为背景或图标"
		d.ReviewRequired = true
		d.ReviewNote = "导出需过人工审批（RiskMedium）；若 REST /v1/images 触发 429 限流，改用接管系统浏览器以登录态导出"
	case n.Type == "TEXT":
		d.Kind = AssetText
		d.Recommendation = "css_manual"
		d.Reason = "文本节点：HTML/CSS 还原（characters 文案 + 颜色/字号 token），不导出图片"
	case len(n.Children) > 0:
		d.Kind = AssetComposite
		d.Recommendation = "recurse"
		d.Reason = fmt.Sprintf("容器节点（%d 个子节点）：递归逐子决策，位图/文本/矢量分别归类", len(n.Children))
	case hasFillType(n.Fills, "SOLID") || hasFillPrefix(n.Fills, "GRADIENT"):
		d.Kind = AssetVector
		d.Recommendation = "css_manual"
		d.Reason = "矢量形状节点：纯色/渐变填充，CSS 可还原（颜色/圆角/尺寸 token）"
	case n.Fills == nil:
		d.Kind = AssetUnknown
		d.Recommendation = "review"
		d.Reason = "fills 数据缺失（API 未返回填充信息），无法判断内容"
		d.ReviewRequired = true
		d.ReviewNote = "数据不足：打开设计稿查看该节点实际内容后补决策"
	default:
		d.Kind = AssetVector
		d.Recommendation = "css_manual"
		d.Reason = "无子节点、无位图填充：按矢量还原"
	}
	return d
}

// ---- figma_plan_page ----

type planReq struct {
	NodeID   string `json:"node_id"`
	MaxDepth int    `json:"max_depth,omitempty"`
	// TechStack 是目标技术栈（html / react / vue / svelte / angular）。
	// 空 = 未指定：技术栈属于工程上下文，Figma 里读不出来——缺省时输出评审点，
	// 编排层可先用 detect_tech_stack 感知工程后自动填入（"能自动的全自动"），
	// 感知不到 / 信号冲突时留给用户确认（"行不通的让你评审"）。
	TechStack string `json:"tech_stack,omitempty"`
}

// PlanResult 是页面级还原计划的汇总：要导出的位图、可复用的 CSS token、评审点。
type PlanResult struct {
	Assets       []AssetDecision
	Tokens       []string
	ReviewPoints []string
	DataContract []DataField // 字段 → 数据源契约（文本节点逐一标注归属）
}

// DataSource 是字段数据源归属：前端生成时决定"写死 / mock / 占位 / 问你"。
type DataSource string

const (
	// DataStatic 文案，属于设计本身 → 直接写死进组件（标题/按钮/标签/导航…）。
	DataStatic DataSource = "STATIC"
	// DataMock 运行时数据但暂未定接口 → 数据层 mock 占位（重复列表内的普通文本）。
	DataMock DataSource = "MOCK"
	// DataAPI 明确的服务端字段 → 前端占位 + 类型定义，接真实接口时替换 mock。
	DataAPI DataSource = "API"
	// DataPending 无法从设计稿判断归属 → 评审点，请用户确认（不静默猜）。
	DataPending DataSource = "PENDING"
)

// DataField 是数据契约的条目：设计稿里的一个文本 → 数据源归属 + 依据。
type DataField struct {
	NodeID string
	Name   string
	Text   string
	Source DataSource
	Reason string
}

func (t *Tools) planPage(input string) (string, error) {
	var req planReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"node_id\":\"104:17791\"}）", err)
	}
	if strings.TrimSpace(req.NodeID) == "" {
		return "", fmt.Errorf("node_id 不能为空（页面级节点，如 \"104:17791\"）")
	}
	maxDepth := defaultMaxDepth
	if req.MaxDepth > 0 {
		maxDepth = req.MaxDepth
	}
	resp, err := t.fetchNodes(req.NodeID)
	if err != nil {
		return "", err
	}

	// 单节点请求只取第一条（plan 面向单个页面/Frame 节点）
	var doc *figmaNode
	for _, info := range resp.Nodes {
		d := info.Document
		doc = &d
		break
	}
	if doc == nil {
		return "", fmt.Errorf("未找到节点 %q", req.NodeID)
	}

	var pl PlanResult
	collectPlan(*doc, maxDepth, &pl, false)

	// 技术栈未指定 → 评审点（不静默猜 html：猜错整段代码作废）
	if strings.TrimSpace(req.TechStack) == "" {
		pl.ReviewPoints = appendUnique(pl.ReviewPoints,
			"未指定目标技术栈，请确认（html / react / vue / 其他）；或先用 detect_tech_stack 感知工程上下文自动填写")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "还原计划: %s（%s）\n", doc.Name, req.NodeID)
	if req.TechStack != "" {
		fmt.Fprintf(&b, "目标技术栈: %s\n", req.TechStack)
	}
	b.WriteString("\n── 位图资产（export_png，需导出挂载）──\n")
	if len(pl.Assets) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, a := range pl.Assets {
		fmt.Fprintf(&b, "  [%s] %s", a.NodeID, a.Name)
		if a.Size != "" {
			fmt.Fprintf(&b, " %s", a.Size)
		}
		fmt.Fprintf(&b, "\n    理由: %s\n", a.Reason)
	}

	// token 段标题随技术栈调整（同一批颜色/尺寸 token 的落点不同）
	tokenTitle := "── CSS 样式 token（还原用）──"
	switch req.TechStack {
	case "react":
		tokenTitle = "── 样式常量（React：token.ts / CSS Modules / 内联样式）──"
	case "vue":
		tokenTitle = "── 样式常量（Vue：<style scoped> CSS 变量）──"
	case "svelte":
		tokenTitle = "── 样式常量（Svelte：<style> 变量）──"
	}
	b.WriteString("\n" + tokenTitle + "\n")
	if len(pl.Tokens) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, tk := range pl.Tokens {
		fmt.Fprintf(&b, "  %s\n", tk)
	}

	// 数据契约段：字段 → 数据源（STATIC 写死 / MOCK 先 mock / API 占位 / PENDING 评审）。
	// 这是"先用 Mock 数据即可"的落地——生成时按归属决定写死还是占位。
	b.WriteString("\n── 数据契约（字段 → 数据源）──\n")
	if len(pl.DataContract) == 0 {
		b.WriteString("  （无文本节点）\n")
	}
	groups := []struct {
		label string
		src   DataSource
	}{
		{"STATIC（写死文案）", DataStatic},
		{"MOCK（数据层占位，接 API 时替换）", DataMock},
		{"API（服务端字段，前端占位）", DataAPI},
		{"PENDING（需你评审）", DataPending},
	}
	for _, g := range groups {
		rows := []DataField{}
		for _, f := range pl.DataContract {
			if f.Source == g.src {
				rows = append(rows, f)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", g.label)
		for _, f := range rows {
			fmt.Fprintf(&b, "    [%s] %s「%s」%s\n", f.NodeID, f.Name, f.Text, f.Reason)
		}
	}

	// Mock 数据建议段：MOCK/API 字段 → 可落盘的 mock.ts（interface + 样例数组），
	// 生成页面时直接写数据层，接真实 API 时只替换这一处。
	if m := buildMockSuggestion(&pl); m != "" {
		b.WriteString("\n── Mock 数据建议（TS，MOCK/API 字段占位）──\n")
		b.WriteString(m)
	}

	// React 组件化分支：位图资产 → import 挂载、页面层级 → 组件拆分建议
	if req.TechStack == "react" {
		b.WriteString("\n── 组件拆分建议（React + TSX）──\n")
		if len(pl.Assets) == 0 {
			b.WriteString("  （无位图资产，纯 token 还原即可）\n")
		}
		for _, a := range pl.Assets {
			fmt.Fprintf(&b, "  [%s] %s：import 为图片资源挂载（如 <img src={assets/%s}> 背景层/图标）\n",
				a.NodeID, a.Name, safeAssetName(a.Name))
		}
		b.WriteString("  页面结构按 Figma 层级拆：Header / TabNav / CardList / ProductCard…\n")
	}

	b.WriteString("\n── 评审点（需你决策 / 行不通的地方）──\n")
	if len(pl.ReviewPoints) == 0 {
		b.WriteString("  （无，全部可自动决策）\n")
	}
	for i, rp := range pl.ReviewPoints {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, rp)
	}
	return b.String(), nil
}

// safeAssetName 把节点名转成安全的资源文件名，防路径注入。
// 资产名来自设计稿（不可信输入），进 import 路径前必须净化：
//   - 非 ASCII 字符（中文等）保留——macOS/Linux 文件系统与 Vite/Webpack
//     构建链均支持，无需转码；
//   - ASCII 中仅保留字母/数字/-/_；空白与 / \ . 等路径或文件系统非法字符
//     一律转 _（"../../etc/passwd" → "_______etc_passwd"，穿越结构被抹平）。
func safeAssetName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "asset"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r > 127:
			b.WriteRune(r)
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// collectPlan 递归汇总：位图进 Assets、评审点进 ReviewPoints、矢量/文本提取 token、
// 文本节点进 DataContract（数据源归属）。maxDepth 递减防爆；children 为空即叶子。
// inRepeat 表示当前节点处于重复同构结构（列表/卡片网格）内——里面的文本大概率
// 是运行时循环数据，不能当静态文案写死。
func collectPlan(n figmaNode, maxDepth int, pl *PlanResult, inRepeat bool) {
	d := decideAsset(n)
	switch {
	case d.Kind == AssetImage:
		pl.Assets = append(pl.Assets, d)
		if d.ReviewNote != "" {
			pl.ReviewPoints = append(pl.ReviewPoints,
				fmt.Sprintf("[%s] %s：%s", d.NodeID, d.Name, d.ReviewNote))
		}
	case d.Kind == AssetUnknown:
		pl.ReviewPoints = append(pl.ReviewPoints,
			fmt.Sprintf("[%s] %s：%s", d.NodeID, d.Name, d.ReviewNote))
	case d.Kind == AssetText:
		if df := classifyProvenance(n, inRepeat); df.Text != "" {
			pl.DataContract = append(pl.DataContract, df)
			if df.Source == DataPending {
				pl.ReviewPoints = append(pl.ReviewPoints,
					fmt.Sprintf("[%s] %s「%s」数据源待定：无法从设计稿判断是写死文案还是服务端数据，请确认",
						df.NodeID, df.Name, df.Text))
			}
		}
		fallthrough
	case d.Kind == AssetVector || d.Kind == AssetText:
		if c := fillColorHex(n.Fills); c != "" {
			pl.Tokens = appendUnique(pl.Tokens, "颜色: "+c+"（"+n.Name+"）")
		}
		if n.BBox != nil {
			pl.Tokens = appendUnique(pl.Tokens,
				fmt.Sprintf("尺寸: %dx%d（%s）", int(n.BBox.Width), int(n.BBox.Height), n.Name))
		}
		if n.CornerRadius != nil {
			pl.Tokens = appendUnique(pl.Tokens,
				fmt.Sprintf("圆角: %g（%s）", *n.CornerRadius, n.Name))
		}
	}
	if len(n.Children) == 0 || maxDepth <= 0 {
		return
	}
	// 重复同构结构检测：容器 children 里同一类型出现 ≥2 次（FRAME/GROUP/COMPONENT），
	// 视为列表/卡片网格 → 这些子树内的文本默认数据循环（inRepeat=true 向下传播）。
	repeatType := ""
	if len(n.Children) >= 2 {
		counts := map[string]int{}
		for _, c := range n.Children {
			counts[c.Type]++
		}
		for t, c := range counts {
			if c >= 2 && (t == "FRAME" || t == "GROUP" || t == "COMPONENT") {
				repeatType = t
				break
			}
		}
	}
	for _, c := range n.Children {
		collectPlan(c, maxDepth-1, pl, inRepeat || c.Type == repeatType)
	}
}

// 字段语义关键词：命中 → 明确的服务端字段（API）。
// 设计稿里的"名称/价格/描述/数量"是数据字段名，不是 UI 文案。
var dataSemanticWords = []string{
	"价格", "金额", "¥", "￥", "元", "数量", "销量", "库存", "编号", "日期",
	"时间", "地址", "电话", "名称", "品名", "描述", "评价", "评分",
}

// UI 控件关键词：命中 → 静态文案（STATIC）。
// 页面标题/按钮/Tab/标签属于设计的一部分，写死即可。
var uiControlWords = []string{
	"标题", "按钮", "Tab", "标签", "导航", "副标题", "提示", "徽标", "角标", "空态", "占位",
}

// classifyProvenance 判定一个文本节点的数据源归属。优先级从高到低：
//  1. 节点名是 UI 控件 → STATIC（写死文案）
//  2. 节点名/文本命中字段语义 → API（服务端字段，前端占位）
//  3. 处于重复列表内 → MOCK（循环数据，先造 mock）
//  4. 其他 → PENDING（评审点，不猜）
//
// 这个分类是"先用 Mock 数据即可"的落地：数据与文案边界清晰，接真实 API 时
// 只改数据层，UI 结构零改动。
func classifyProvenance(n figmaNode, inRepeat bool) DataField {
	df := DataField{NodeID: n.ID, Name: n.Name, Text: strings.TrimSpace(n.Characters)}
	if df.Text == "" {
		return df // 空文本不产生契约
	}
	name := strings.ToLower(n.Name)
	text := strings.ToLower(df.Text)

	// 1) UI 控件 → STATIC（控件语义优先于其他信号）
	for _, w := range uiControlWords {
		if strings.Contains(name, strings.ToLower(w)) {
			df.Source, df.Reason = DataStatic, "UI 控件文案（节点名含「"+w+"」），写死"
			return df
		}
	}
	// 2) 字段语义 → API
	for _, w := range dataSemanticWords {
		if strings.Contains(text, w) || strings.Contains(name, strings.ToLower(w)) {
			df.Source, df.Reason = DataAPI, "字段语义（"+w+"）→ 服务端返回，前端占位"
			return df
		}
	}
	// 3) 重复列表内 → MOCK
	if inRepeat {
		df.Source, df.Reason = DataMock, "重复卡片/列表内的循环数据，先 mock 占位，接 API 时替换"
		return df
	}
	// 4) 兜底 → PENDING
	df.Source, df.Reason = DataPending, "无法从设计稿判断是文案还是服务端数据"
	return df
}

// 字段语义 → TS 字段名映射：Figma 节点名（中文）转可运行的 interface 字段。
// 未命中映射的字段用 field_N 占位并注明"按真实接口重命名"。
var semanticToTSField = map[string]string{
	"名称": "name", "品名": "name", "标题": "title", "价格": "price", "金额": "amount",
	"数量": "quantity", "销量": "sales", "库存": "stock", "编号": "id", "日期": "date",
	"时间": "time", "地址": "address", "电话": "phone", "描述": "description",
	"说明": "description", "内容": "content", "标签": "tag", "状态": "status",
	"评价": "rating", "评分": "rating",
}

// tsFieldName 把 Figma 字段名映射为 TS 标识符（无映射返回 ""，调用方给 field_N 兜底）。
func tsFieldName(nodeName string) string {
	for kw, en := range semanticToTSField {
		if strings.Contains(nodeName, kw) {
			return en
		}
	}
	return ""
}

// mockGroup 是按 TS 字段名聚合的数据契约字段：en=字段名、label=原节点名、samples=样例值。
type mockGroup struct {
	en      string
	label   string
	samples []string
}

// buildMockSuggestion 把数据契约里的 MOCK/API 字段转成可落盘的 mock.ts 文本：
//   - 全部字段出现次数一致（每条记录字段齐）→ 生成 interface + 对象数组；
//   - 次数不一致 → 只生成 interface + 字段样例注释（让 LLM/开发者对齐填写）。
//
// 这是"先用 Mock 数据即可"的收尾：mock.ts 即数据层，接真实 API 只改这一处。
func buildMockSuggestion(pl *PlanResult) string {
	// 聚合：按 TS 字段名去重，样例值按出现顺序收集
	groups := []*mockGroup{}
	index := map[string]*mockGroup{}
	for _, f := range pl.DataContract {
		if f.Source != DataMock && f.Source != DataAPI {
			continue
		}
		en := tsFieldName(f.Name)
		if en == "" {
			en = fmt.Sprintf("field_%d", len(groups)+1) // 无映射字段按序兜底
		}
		g, ok := index[en]
		if !ok {
			g = &mockGroup{en: en, label: f.Name}
			groups = append(groups, g)
			index[en] = g
		}
		g.samples = append(g.samples, f.Text)
	}
	if len(groups) == 0 {
		return ""
	}

	// 记录对齐检测：每组样例数一致 → 可生成对象数组
	k := len(groups[0].samples)
	aligned := true
	for _, g := range groups {
		if len(g.samples) != k {
			aligned = false
			break
		}
	}

	var b strings.Builder
	if aligned && k > 0 {
		// interface
		b.WriteString("export interface " + mockInterfaceName(groups) + " {\n")
		for _, g := range groups {
			fmt.Fprintf(&b, "  %s: string // %s\n", g.en, g.label)
		}
		b.WriteString("}\n\n")
		// 对象数组
		fmt.Fprintf(&b, "export const %s: %s[] = [\n", mockListName(groups), mockInterfaceName(groups))
		for j := 0; j < k; j++ {
			fmt.Fprintf(&b, "  { ")
			for i, g := range groups {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%s: %q", g.en, g.samples[j])
			}
			b.WriteString(" },\n")
		}
		b.WriteString("]\n")
		b.WriteString("// 接真实接口时: 用 API 响应类型替换此 mock 数据，字段名按响应重命名\n")
	} else {
		b.WriteString("// 字段样例（记录未对齐，按字段填值即可）\n")
		for _, g := range groups {
			fmt.Fprintf(&b, "// %s（%s）: %s\n", g.en, g.label, strings.Join(g.samples, " / "))
		}
		b.WriteString("// 接真实接口时: 替换为 API 响应类型与数据\n")
	}
	return b.String()
}

// mockInterfaceName / mockListName 生成 TS 标识符：列表名 → 首字段映射英文的 PascalCase。
// 简化：无映射时用 MockData 兜底；标识符一律 ASCII（字段名来自中文节点名，不可直接做标识符）。
func mockInterfaceName(groups []*mockGroup) string {
	return "MockRecord"
}

func mockListName(groups []*mockGroup) string {
	return "mockList"
}

// appendUnique 线性去重追加（token 集合小，O(n) 足够；JS 的 Set 在 Go 里
// 用 map 或线性扫描皆可——此处可读性优先）。
func appendUnique(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}

// hasFillType / hasFillPrefix 判断节点填充里是否存在某类填充（visible 才计）。
// Figma fill.type 取值：SOLID / GRADIENT_LINEAR|RADIAL|ANGULAR|DIAMOND / IMAGE / VIDEO。
func hasFillType(fills []fill, typ string) bool {
	for _, f := range fills {
		if f.Visible && f.Type == typ {
			return true
		}
	}
	return false
}

func hasFillPrefix(fills []fill, prefix string) bool {
	for _, f := range fills {
		if f.Visible && strings.HasPrefix(f.Type, prefix) {
			return true
		}
	}
	return false
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
