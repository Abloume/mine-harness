// Package tree 实现第 5 步阶段二 · 树索引 + 推理检索（PLAN 第 5 步②）。
//
// 决策依据（2026-10-13）：PageIndex 观察定路线（树索引 + 推理检索优先，
// 「相似不等于相关」，复用既有工具沿树定位）；OpenViking 观察补分层规格
// （L0 摘要 ~100 token / L1 概览 ~2000 token / L2 详情按需加载）并排除
// 反例（目录分数传播，其最新 release 已移除）。
//
// 核心设计：
//   - 检索 = 工具调用序列：模型 browse 沿树下钻 → overview 确认 → read 取
//     详情——复用"看目录 → 翻文件 → 扫内容"的人类翻书方式；
//   - 渐进披露：browse / overview 只给摘要（L0/L1），read 才给全文（L2），
//     成本只随"命中路径上的节点"增长，不整读代码库；
//   - 引用可追溯：read 返回带行号内容，最终回答可引用 file:line；
//   - 索引侧启发式摘要（文件名/行数/头部）生成，"由基础模型精修"是可选
//     增强——检索的关键路径（browse/overview）不经过 LLM。
//
// JS/TS ↔ Go 差异：TS 常用递归 walk 闭包收集；Go 用 filepath.WalkDir +
// 显式 map 索引（nodes/children 双 map：O(1) 定位 + 父子关系），且返回值
// 显式带 error（文件系统操作没有"静默失败"）。
package tree

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Node 是树索引的一个节点（目录或文件），携带 L0/L1 摘要。
// L0 是 browse 列表里的一行摘要（定位/过滤用）；L1 是 overview 的
// 概览（导航/决策用）；L2 由 Read 按需返回（完整内容）。
type Node struct {
	Path  string // 绝对路径
	IsDir bool
	L0    string // 一行摘要
	L1    string // 概览（dir=子项 L0 清单；file=头部行 + 行数）
}

// TreeIndex 是把目录树组织成可浏览层级结构的索引。
//
// 构建是"索引侧全量一次"（walk 全树、统计行数），之后 browse/overview
// 只读内存摘要（O(节点数) 成本），read 才按需返回文件全文——
// 对应"把 LLM 请出每次检索的关键路径"。
type TreeIndex struct {
	root     string
	nodes    map[string]*Node    // 绝对路径 → 节点
	children map[string][]string // 目录绝对路径 → 子路径（已排序）
	lines    map[string][]string // 文件绝对路径 → 行内容（构建时读入）
	ignore   []string            // 忽略的路径片段（如 .git）
}

// NewTreeIndex 从 root 构建树索引。ignore 里的子串匹配的路径（文件或目录）
// 会被排除。默认忽略清单对齐 .gitignore 的"不该进索引"语义：
//   - .git / node_modules / sessions：仓库与运行时产物；
//   - .DS_Store：macOS 元数据；
//   - .env：**含密钥/令牌**——browse 摘要会把首行打进工具结果（进模型上下文
//     与日志），必须默认排除（安全护栏，与 fsagent 的 secrets 处理一致）。
func NewTreeIndex(root string, ignore []string) (*TreeIndex, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析根路径失败: %w", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("根路径不存在: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("根路径不是目录: %s", root)
	}
	t := &TreeIndex{
		root:     root,
		nodes:    map[string]*Node{},
		children: map[string][]string{},
		lines:    map[string][]string{},
		ignore:   append([]string{".git", ".DS_Store", "node_modules", "sessions", ".env"}, ignore...),
	}
	if err := t.build(); err != nil {
		return nil, err
	}
	return t, nil
}

// build 全量构建：walk 目录树 → 建 nodes/children → 统计行数与头部。
// 单文件读取成本 O(文件大小) 只发生在构建期；之后 read 走内存 lines。
func (t *TreeIndex) build() error {
	return filepath.WalkDir(t.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != t.root {
			for _, ig := range t.ignore {
				if strings.Contains(path, ig) {
					if d.IsDir() {
						return filepath.SkipDir // 忽略整个目录子树
					}
					return nil
				}
			}
		}
		rel, err := filepath.Rel(t.root, path)
		if err != nil {
			return err
		}
		node := &Node{Path: path, IsDir: d.IsDir()}
		if d.IsDir() {
			node.L0 = fmt.Sprintf("dir · %s（目录）", rel)
			node.L1 = "（目录概览：见子项）"
		} else {
			lines, err := t.readLines(path)
			if err != nil {
				return nil // 不可读文件（如二进制/权限）跳过，不阻塞索引
			}
			t.lines[path] = lines
			head := ""
			if len(lines) > 0 {
				head = strings.TrimSpace(lines[0])
				if len(head) > 60 {
					head = head[:57] + "…"
				}
			}
			// L0：文件名 + 行数 + 首行（一行内可浏览，~100 token 预算）
			node.L0 = fmt.Sprintf("file · %s（%d 行）首行: %s", rel, len(lines), head)
			// L1：头部 N 行（overview 用，~2000 token 预算内截断）
			preview := lines
			if len(preview) > 40 {
				preview = preview[:40]
			}
			node.L1 = fmt.Sprintf("file · %s（共 %d 行）\n%s\n…（其余 %d 行按需读取）",
				rel, len(lines), strings.Join(preview, "\n"), len(lines)-len(preview))
		}
		t.nodes[path] = node
		if parent := filepath.Dir(path); parent != path {
			t.children[parent] = append(t.children[parent], path)
		}
		return nil
	})
}

// readLines 读取文件全部行（构建期一次性；lines 缓存供后续 read）。
func (t *TreeIndex) readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.Split(string(data), "\n")
	if len(s) > 0 && s[len(s)-1] == "" {
		s = s[:len(s)-1] // 去掉结尾空行（文件以换行结尾时）
	}
	return s, nil
}

// Root 返回索引根路径。
func (t *TreeIndex) Root() string { return t.root }

// Nodes 返回索引节点总数（统计/展示用，如"索引 N 个节点"的成本对比）。
func (t *TreeIndex) Nodes() int { return len(t.nodes) }

// TotalLines 返回索引内全部文件的总行数（成本对比展示用：
// 展示"全量整读"与"命中节点按需读"的量级差异）。
func (t *TreeIndex) TotalLines() int {
	n := 0
	for _, l := range t.lines {
		n += len(l)
	}
	return n
}

// node 按路径查节点；不存在返回 error（检索工具据此给模型明确指引）。
func (t *TreeIndex) node(path string) (*Node, error) {
	if path == "" || path == "." {
		path = t.root
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(t.root, path)
	}
	path = filepath.Clean(path)
	n, ok := t.nodes[path]
	if !ok {
		return nil, fmt.Errorf("路径 %s 不在索引中（browse 只到目录，文件用 read 按需读取）", path)
	}
	return n, nil
}

// Browse 返回目录的直接子节点列表（L0 摘要，已按名字排序）——
// 对应模型"先看目录"：拿到一屏摘要决定下钻方向。
func (t *TreeIndex) Browse(dir string) ([]Node, error) {
	n, err := t.node(dir)
	if err != nil {
		return nil, err
	}
	if !n.IsDir {
		return nil, fmt.Errorf("%s 是文件不是目录，读内容用 read", n.Path)
	}
	kids := t.children[n.Path]
	sort.Strings(kids)
	out := make([]Node, 0, len(kids))
	for _, k := range kids {
		if c, ok := t.nodes[k]; ok {
			out = append(out, *c)
		}
	}
	return out, nil
}

// Overview 返回节点 L1 概览——确认"这层有没有我要的东西"，再决定
// 继续下钻还是 read 详情。
func (t *TreeIndex) Overview(path string) (string, error) {
	n, err := t.node(path)
	if err != nil {
		return "", err
	}
	if n.IsDir {
		// 目录 L1：子项 L0 清单（导航视图）
		kids := t.children[n.Path]
		sort.Strings(kids)
		var b strings.Builder
		fmt.Fprintf(&b, "目录 %s（%d 项）：\n", filepath.Base(n.Path), len(kids))
		for _, k := range kids {
			if c, ok := t.nodes[k]; ok {
				fmt.Fprintf(&b, "  %s\n", c.L0)
			}
		}
		return b.String(), nil
	}
	return n.L1, nil
}

// Read 返回文件完整内容（带行号，L2 按需加载）——引用可追溯的载体：
// 模型在最终回答里引用 "路径:行号" 即为可验证证据。
func (t *TreeIndex) Read(path string) (string, error) {
	n, err := t.node(path)
	if err != nil {
		return "", err
	}
	if n.IsDir {
		return "", fmt.Errorf("%s 是目录，读子项用 browse/overview", n.Path)
	}
	lines, ok := t.lines[n.Path]
	if !ok {
		return "", fmt.Errorf("文件 %s 无缓存内容（构建时不可读?）", n.Path)
	}
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%4d: %s\n", i+1, ln)
	}
	return b.String(), nil
}
