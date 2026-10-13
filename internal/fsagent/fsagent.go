// Package fsagent 提供真实文件系统工具集（PLAN 第 1 步：真实文件工具集）。
//
// 与内核解耦：只依赖 internal/agent 的 Tool 类型，工具如何注册、审批、
// 并行执行由宿主决定。边界哲学不变：机制归内核，工具归应用层——本包就是
// "应用层第一个真实工具包"，后续命令执行、前端场景工具沿用同样模式。
//
// JS/TS ↔ Go 差异总览（详见各函数注释）：
//   - node:fs 是异步回调/Promise；Go 的 os 包是同步调用 + 显式 error 返回
//   - JS 没有路径安全内建概念；Go 的 filepath.IsLocal 明确判定"本地相对路径"
//   - JS writeFile 直接覆盖（写一半断电就坏）；Go 可用同目录 temp + rename 原子替换
package fsagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mine-harness/internal/agent"
)

// Tools 是文件系统工具集的宿主，绑定一个项目根目录。
// 所有 path 参数都是相对 root 的路径，越界（绝对路径 / .. 穿越）被拒——
// 工具能碰的最大范围 = root，这就是"工具边界"的最小落地。
type Tools struct {
	root string
}

// NewTools 构造绑定指定根目录的工具集。
// root 通常传当前工作目录（.），所有文件操作被限制在 root 内。
func NewTools(root string) *Tools { return &Tools{root: root} }

// All 返回全部工具定义（宿主逐个注册：for _, t := range fsagent.NewTools(".").All()）。
//
// 风险分级（见 internal/agent/risk.go）：
//   - read_file / glob / grep：RiskNone（只读，自动放行，不打扰用户）
//   - write_file / edit_file：RiskMedium（有副作用但有备份可回滚）——
//     达到默认审批阈值（Medium 及以上），必须走审批闸门（fail-closed）
//
// 并发安全：读类工具声明 ConcurrentSafe=true 可并行；写类声明 false
// （共享 backup/ 目录与目标文件，保持顺序执行——并发是优化，正确性优先）。
func (t *Tools) All() []agent.Tool {
	return []agent.Tool{
		{
			Name:           "read_file",
			Description:    "读取项目内文件内容。参数是 JSON：{\"path\":\"相对路径\",\"max_len\":可选截断长度}。超过 max_len 截断并提示总长。",
			BaseRisk:       agent.RiskNone,
			ConcurrentSafe: true,
			Execute:        t.readFile,
		},
		{
			Name:        "write_file",
			Description: "写入文件（覆盖已有文件前自动备份到 backup/，原子替换不留半截文件）。参数是 JSON：{\"path\":\"相对路径\",\"content\":\"新内容\"}。写入/覆盖需审批。",
			BaseRisk:    agent.RiskMedium,
			Execute:     t.writeFile,
		},
		{
			Name:        "edit_file",
			Description: "精确替换文件中的一段文本（old 必须恰好出现一次，否则报错——防幻觉替换）。自动备份 + 原子替换。参数是 JSON：{\"path\":\"相对路径\",\"old\":\"原文\",\"new\":\"新文\"}。修改需审批。",
			BaseRisk:    agent.RiskMedium,
			Execute:     t.editFile,
		},
		{
			Name:           "glob",
			Description:    "按模式列出项目内文件（返回相对路径，每行一个）。参数是 JSON：{\"pattern\":\"*.go\"}。支持 * ? []（filepath.Match 语义），**/ 前缀表示任意深度。",
			BaseRisk:       agent.RiskNone,
			ConcurrentSafe: true,
			Execute:        t.glob,
		},
		{
			Name:           "grep",
			Description:    "在项目内搜索文本，返回匹配行（路径:行号:内容）。参数是 JSON：{\"pattern\":\"关键词\",\"path\":\"可选，只搜该文件\",\"max_results\":可选上限，默认50}。",
			BaseRisk:       agent.RiskNone,
			ConcurrentSafe: true,
			Execute:        t.grep,
		},
		{
			Name: "detect_tech_stack",
			Description: "感知项目工程的技术栈（纯读，自动放行）：扫描 package.json / tsconfig / 框架配置 / 源文件后缀，" +
				"推断前端代码生成目标（react / vue / svelte / html / unknown），输出 stack + confidence + evidence 信号依据。" +
				"置信度低或信号冲突（如 package.json 无 react 但目录全是 .tsx）时输出需评审提示——" +
				"技术栈是工程上下文，Figma 里读不出来，必须从项目文件感知。参数 JSON：{}。",
			BaseRisk:       agent.RiskNone,
			ConcurrentSafe: true,
			Execute:        t.detectTechStack,
		},
	}
}

// resolve 把模型给的路径解析为 root 内的绝对路径。
//
// 安全：filepath.Clean 归一化后必须 filepath.IsLocal——拒绝绝对路径和
// .. 穿越（与 read_skill_ref 的防护同一套路，见 skillhost.go LoadReference）。
//
// 生产增强：root 内若有 symlink 指向 root 外，IsLocal 拦不住（路径本身合法），
// 需要再 EvalSymlinks 校验最终路径仍在 root 内——本实现先不引入，注释留点。
//
// JS/TS ↔ Go 差异：JS 的 path.resolve + startsWith 判断也能做，但没有
// IsLocal 这种"语言内置的路径安全判定"，容易漏（如 Windows 盘符、UNC）。
func (t *Tools) resolve(p string) (string, error) {
	clean := filepath.Clean(p)
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("路径越界被拒: %q（只允许项目根内的相对路径，禁止绝对路径和 ..）", p)
	}
	return filepath.Join(t.root, clean), nil
}

// ---- read_file ----

type readReq struct {
	Path   string `json:"path"`
	MaxLen int    `json:"max_len,omitempty"` // 0 = 不截断
}

func (t *Tools) readFile(input string) (string, error) {
	var req readReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"path\":\"...\"}）", err)
	}
	full, err := t.resolve(req.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("读取失败: %v", err)
	}
	s := string(data)
	if req.MaxLen > 0 && len(s) > req.MaxLen {
		// 截断必须显式告知：模型不知道内容被截了，会基于不完整内容做错误决策
		return s[:req.MaxLen] + fmt.Sprintf("\n\n...[已截断：共 %d 字符，返回前 %d，如需更多请用 max_len 继续读取]", len(s), req.MaxLen), nil
	}
	return s, nil
}

// ---- write_file（自动备份 + 原子写） ----

type writeReq struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *Tools) writeFile(input string) (string, error) {
	var req writeReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"path\":\"...\",\"content\":\"...\"}）", err)
	}
	full, err := t.resolve(req.Path)
	if err != nil {
		return "", err
	}

	// 写前备份（file-ops-policy 原则落地）：只备份已存在的文件。
	// 自动备份兜底，不依赖"模型记得先调 backup 工具"的纪律。
	bakNote := "新文件，无需备份"
	if _, err := os.Stat(full); err == nil {
		bak, err := t.backup(full)
		if err != nil {
			return "", fmt.Errorf("备份失败，已停止写入（备份失败即停止）: %v", err)
		}
		bakNote = "已备份 → " + bak
	}
	if err := atomicWrite(full, []byte(req.Content)); err != nil {
		return "", fmt.Errorf("写入失败: %v", err)
	}
	return "写入成功（" + bakNote + "）", nil
}

// backup 把目标文件备份到 root/backup/ 下（镜像相对路径 + 时间戳后缀）。
// 返回给模型看的是相对 root 的路径（backup/xxx.ts.bak）。
func (t *Tools) backup(full string) (string, error) {
	rel, err := filepath.Rel(t.root, full)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s.%d.bak", rel, time.Now().Unix())
	bak := filepath.Join(t.root, "backup", name)
	if err := os.MkdirAll(filepath.Dir(bak), 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(bak, data, 0o644); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join("backup", name)), nil
}

// atomicWrite 原子写：同目录临时文件 + rename 替换。
//
// 为什么必须同目录：os.Rename 跨文件系统（如 /tmp → 项目目录）会失败，
// 同目录 rename 在 POSIX 上是原子操作——要么旧文件、要么新文件，绝无
// "写到一半"的半截文件。对应 JS 直接 writeFile 覆盖的断电/崩溃风险。
// 临时文件用 defer Remove 兜底清理，任何失败路径都不留垃圾。
func atomicWrite(full string, data []byte) error {
	dir := filepath.Dir(full)
	tmp, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}

// ---- edit_file（精确替换，防幻觉） ----

type editReq struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

func (t *Tools) editFile(input string) (string, error) {
	var req editReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"path\":\"...\",\"old\":\"...\",\"new\":\"...\"}）", err)
	}
	full, err := t.resolve(req.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("读取失败: %v", err)
	}
	content := string(data)

	// 防幻觉（fail-fast）：old 必须恰好出现一次。
	//   0 次 → 模型臆想的内容不存在（可能文件已变/描述有误），报错让模型
	//         用 read_file 重新看，不能静默"无事发生"；
	//   N>1 次 → 定位不精确，报错让模型先读上下文给出唯一片段。
	// 这是"宁可报错也不乱改"——AI 改代码最常见事故就是替换错位置。
	count := strings.Count(content, req.Old)
	if count == 0 {
		return "", fmt.Errorf("未找到要替换的原文（old 不存在于文件中，可能内容已变化或描述有误，请先用 read_file 查看）")
	}
	if count > 1 {
		return "", fmt.Errorf("old 在文件中出现 %d 次，定位不精确——请先用 read_file 查看上下文，给出唯一片段", count)
	}

	bak, err := t.backup(full)
	if err != nil {
		return "", fmt.Errorf("备份失败，已停止（备份失败即停止）: %v", err)
	}
	if err := atomicWrite(full, []byte(strings.Replace(content, req.Old, req.New, 1))); err != nil {
		return "", fmt.Errorf("写入失败: %v", err)
	}
	return "替换成功（已备份 → " + bak + "）", nil
}

// ---- glob（按模式列文件） ----

type globReq struct {
	Pattern string `json:"pattern"`
}

func (t *Tools) glob(input string) (string, error) {
	var req globReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"pattern\":\"...\"}）", err)
	}
	// **/ 前缀 = 任意深度（简化：把剩余模式对文件 basename 匹配）；
	// 否则对完整相对路径匹配。filepath.Match 的 * 不跨目录分隔符，
	// 所以 "*.go" 只匹配根层——完整 ** 语义生产可用 doublestar 库。
	recursive := strings.HasPrefix(req.Pattern, "**/")
	tail := strings.TrimPrefix(req.Pattern, "**/")

	var out []string
	err := filepath.WalkDir(t.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err // 子目录读失败：fail-fast 报出来，不静默跳过
		}
		if d.IsDir() {
			if p != t.root && filepath.Base(p) == "backup" {
				return filepath.SkipDir // 不列自己的备份目录
			}
			return nil
		}
		rel, err := filepath.Rel(t.root, p)
		if err != nil {
			return err
		}
		cand := rel
		if recursive {
			cand = filepath.Base(rel)
		}
		ok, err := filepath.Match(tail, filepath.ToSlash(cand))
		if err != nil {
			return fmt.Errorf("pattern 非法: %v", err)
		}
		if ok {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("遍历失败: %v", err)
	}
	if len(out) == 0 {
		return "（无匹配文件）", nil
	}
	return strings.Join(out, "\n"), nil
}

// ---- grep（按关键词搜文件） ----

type grepReq struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"` // 可选：只搜该文件
	MaxResults int    `json:"max_results,omitempty"`
}

func (t *Tools) grep(input string) (string, error) {
	var req grepReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"pattern\":\"...\"}）", err)
	}
	if req.MaxResults <= 0 {
		req.MaxResults = 50 // 默认上限：防止海量匹配撑爆上下文
	}

	if req.Path != "" {
		full, err := t.resolve(req.Path)
		if err != nil {
			return "", err
		}
		res, err := grepFile(full, req.Pattern, req.MaxResults, req.Path)
		if err != nil {
			return "", err
		}
		if res == "" {
			return "（无匹配）", nil
		}
		return res, nil
	}

	var out []string
	err := filepath.WalkDir(t.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != t.root && filepath.Base(p) == "backup" {
				return filepath.SkipDir // 不搜自己的备份目录
			}
			return nil
		}
		rel, err := filepath.Rel(t.root, p)
		if err != nil {
			return err
		}
		res, err := grepFile(p, req.Pattern, req.MaxResults-len(out), rel)
		if err != nil {
			return err
		}
		if res != "" {
			out = append(out, res)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("搜索失败: %v", err)
	}
	if len(out) == 0 {
		return "（无匹配）", nil
	}
	return strings.Join(out, "\n"), nil
}

// grepFile 逐行搜索单个文件，返回 "路径:行号:内容" 多行文本。
//
// 流式读取（bufio.Scanner）vs JS readFile+split：对大文件，Go 用扫描器
// 边读边判断，内存 O(1)；JS 一次性读入再 split 会整文件驻留内存。
// 文件打不开（无权限等）返回空结果跳过——目录级搜索不应因单个文件
// 失败而终止整个搜索（fail-soft 只对"读不了的文件"，与 fail-fast 对
// "路径越界"不同：后者是模型输入错误，前者是环境噪声）。
func grepFile(full, pattern string, max int, displayPath string) (string, error) {
	if max <= 0 {
		return "", nil
	}
	f, err := os.Open(full)
	if err != nil {
		return "", nil
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024) // 行长上限放宽到 1MB（默认 64KB 会误报超长行）
	line := 0
	for sc.Scan() {
		line++
		if strings.Contains(sc.Text(), pattern) {
			out = append(out, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(displayPath), line, strings.TrimRight(sc.Text(), "\r\n")))
			if len(out) >= max {
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return strings.Join(out, "\n"), nil
}

// ---- detect_tech_stack（工程技术栈感知） ----
//
// 目标：Figma 决定"还原什么"，工程上下文决定"用什么还原"——技术栈在
// package.json / tsconfig / 框架配置 / 源文件后缀里，不在设计稿里。
// 本工具把「读文件 + 规则匹配」的感知固化为可测试的纯函数：
//   - 高置信（react 依赖 + jsx 配置 + .tsx 文件齐了）→ 直接自动决策；
//   - 低置信 / 信号冲突（package.json 无 react 但全是 .tsx）→ 显式列证据，
//     升级用户评审——"能自动的全自动，行不通的让你评审"在这一层的落地。
//
// JS/TS ↔ Go 对比：JS 常靠 npm 包（which-pm-runs / get-framework）做检测，
// Go 这里手写规则枚举——依赖少、行为可测试；代价是框架清单要自己维护，
// 新增框架 = 加一个 map 条目 + 一个测试用例。

// techStack 是推断结果：stack + 置信度 + 证据/冲突清单。
type techStack struct {
	Stack      string   // react / vue / svelte / angular / html / unknown
	Confidence string   // high / medium / low
	Evidence   []string // 支持推断的信号（给模型/用户看依据）
	Conflicts  []string // 冲突信号（存在则需评审）
}

// detectTechStack 工具实现：输入 JSON {}，对绑定 root 扫描推断。
func (t *Tools) detectTechStack(input string) (string, error) {
	// 无参数工具：输入预期 "{}"，忽略内容（与 figma_get_variables 同模式）
	st := detectStack(t.root)
	return formatStack(st), nil
}

// formatStack 把推断结果格式化为人类可读输出（评审时用户一眼看懂依据）。
func formatStack(st techStack) string {
	var b strings.Builder
	flag := ""
	if st.Stack == "unknown" || len(st.Conflicts) > 0 {
		flag = "（⚠️ 需评审）"
	}
	fmt.Fprintf(&b, "技术栈推断: %s（置信度: %s）%s\n", st.Stack, st.Confidence, flag)
	if len(st.Evidence) > 0 {
		b.WriteString("信号:\n")
		for _, e := range st.Evidence {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
	}
	if len(st.Conflicts) > 0 {
		b.WriteString("冲突信号（需你确认）:\n")
		for _, c := range st.Conflicts {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	if len(st.Conflicts) > 0 {
		b.WriteString("评审点: 信号冲突，请确认目标技术栈后再生成（避免自动猜错整段代码作废）")
	} else if st.Stack == "unknown" {
		b.WriteString("评审点: 未检测到可靠的前端技术栈信号，请确认生成目标（html / react / vue / 其他）")
	}
	return b.String()
}

// detectStack 核心感知逻辑：扫描 root 的工程文件，输出推断。
func detectStack(root string) techStack {
	var st techStack
	// scores 按"信号条数"独立累加：react 依赖 + jsx 配置 + .tsx 文件 = 3 条信号，
	// 而不是 map 覆盖成 1 条——置信度由信号数量决定（JS/TS 对比：map 值去重
	// 会把同框架多信号合并，这里用计数表，每个信号事件都算数）。
	scores := map[string]int{}

	// ① 依赖信号：package.json 的 dependencies + devDependencies
	deps := readPackageDeps(root)
	for name := range deps {
		fw := ""
		switch {
		case name == "react" || name == "react-dom" || name == "next" || strings.HasPrefix(name, "@react"):
			fw = "react"
		case name == "vue" || name == "nuxt":
			fw = "vue"
		case name == "svelte" || name == "@sveltejs/kit":
			fw = "svelte"
		case name == "@angular/core":
			fw = "angular"
		}
		if fw != "" {
			scores[fw]++
			st.Evidence = append(st.Evidence, "package.json 依赖含 "+name)
		}
	}

	// ② tsconfig：jsx 编译选项 → React+TS 的强信号
	if jsx := readTSConfigJSX(root); jsx != "" {
		st.Evidence = append(st.Evidence, fmt.Sprintf("tsconfig.json 设置 jsx=%q", jsx))
		if jsx == "react-jsx" || jsx == "react" || jsx == "preserve" {
			scores["react"]++
		}
	}

	// ③ 源文件后缀计数 + 框架配置文件存在性（跳过 node_modules/.git/backup）
	extCount := map[string]int{}
	hasConfig := map[string]bool{}
	walkIgnore(root, func(rel string) {
		ext := strings.ToLower(filepath.Ext(rel))
		extCount[ext]++
		base := filepath.Base(rel)
		cfg := map[string]string{
			"vite.config": "vite", "next.config": "next", "nuxt.config": "nuxt",
			"svelte.config": "svelte", "astro.config": "astro", "angular.json": "angular",
		}
		for prefix, framework := range cfg {
			if strings.HasPrefix(base, prefix) {
				hasConfig[framework] = true
				st.Evidence = append(st.Evidence, fmt.Sprintf("存在 %s（%s）", base, framework))
			}
		}
	})
	for ext, n := range extCount {
		switch ext {
		case ".tsx":
			st.Evidence = append(st.Evidence, fmt.Sprintf("存在 %d 个 .tsx 源文件", n))
			scores["react"]++
		case ".vue":
			st.Evidence = append(st.Evidence, fmt.Sprintf("存在 %d 个 .vue 源文件", n))
			scores["vue"]++
		case ".svelte":
			st.Evidence = append(st.Evidence, fmt.Sprintf("存在 %d 个 .svelte 源文件", n))
			scores["svelte"]++
		}
	}
	// 框架配置文件也计入对应框架信号
	for fw, has := range hasConfig {
		if has {
			scores[fw]++
		}
	}

	// ④ 纯静态信号：有 index.html 但无任何框架信号
	_, htmlErr := os.Stat(filepath.Join(root, "index.html"))
	hasHTML := htmlErr == nil
	if hasHTML && len(scores) == 0 {
		st.Evidence = append(st.Evidence, "存在 index.html，且无框架依赖/框架源文件")
		scores["html"]++
	}

	// ⑤ 汇总结论：多框架并存 → 冲突；单一 → 按信号条数定置信度；全无 → unknown
	best, bestScore := "", 0
	for fw, n := range scores {
		if n > bestScore {
			best, bestScore = fw, n
		}
	}
	if bestScore == 0 {
		st.Stack, st.Confidence = "unknown", "low"
		return st
	}
	// 冲突检测：第二名也有信号（且非 html——html 是"无框架"的兜底，不算冲突）
	second := 0
	for fw, n := range scores {
		if fw != best && n > second {
			second = n
		}
	}
	if second > 0 && best != "html" {
		st.Stack, st.Confidence = "unknown", "low"
		for fw := range scores {
			if fw != best {
				st.Conflicts = append(st.Conflicts, fmt.Sprintf("检测到 %s 与 %s 双框架信号", best, fw))
			}
		}
		return st
	}
	st.Stack = best
	switch {
	case bestScore >= 3 || (best == "react" && hasConfig["vite"]):
		st.Confidence = "high"
	case bestScore == 2:
		st.Confidence = "medium"
	default:
		st.Confidence = "medium"
	}
	return st
}

// readPackageDeps 读取 package.json 的 dependencies + devDependencies 键集合。
// 解析失败/文件缺失 → 空集合（不 panic；JSON 语法错误也算"无依赖信号"）。
func readPackageDeps(root string) map[string]string {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}
	out := make(map[string]string)
	for k, v := range pkg.Dependencies {
		out[k] = v
	}
	for k, v := range pkg.DevDependencies {
		out[k] = v
	}
	return out
}

// readTSConfigJSX 读取 tsconfig.json 的 compilerOptions.jsx 值。
func readTSConfigJSX(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "tsconfig.json"))
	if err != nil {
		return ""
	}
	var ts struct {
		CompilerOptions struct {
			JSX string `json:"jsx"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(data, &ts); err != nil {
		return ""
	}
	return ts.CompilerOptions.JSX
}

// walkIgnore 遍历 root，跳过 node_modules/.git/backup/dist，对每个文件
// 调 fn(rel)。错误静默跳过（探测场景容错优先：读不了的目录不致命）。
func walkIgnore(root string, fn func(rel string)) {
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch filepath.Base(p) {
			case "node_modules", ".git", "backup", "dist", ".next", "build", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		fn(filepath.ToSlash(rel))
		return nil
	})
}
