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
