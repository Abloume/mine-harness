package main

import (
	"os"
	"path/filepath"
	"strings"

	"mine-harness/internal/agent"
)

// dirSkillProvider 是宿主（harness 层）实现的 agent.SkillProvider：
// 从 skills/ 目录发现技能——每个技能一个子目录，内含 SKILL.md，
// 文件头部 YAML frontmatter 的 name 字段作为技能名（与 A 方案示例同构）。
//
// 边界（README 约定）：机制归内核（internal/agent），目录扫描/frontmatter
// 解析归宿主。宿主换存储（数据库/远程仓库）只需重写这个结构体。
type dirSkillProvider struct {
	root string
}

// newDirSkillProvider 构造目录技能源；root 是 skills 根目录（相对 main 包运行目录）。
func newDirSkillProvider(root string) *dirSkillProvider {
	return &dirSkillProvider{root: root}
}

// List 返回全部技能的常驻元数据（渐进式披露第一层）：
// 扫描 root 下每个子目录的 SKILL.md，取 frontmatter 的 name/description/tags。
func (p *dirSkillProvider) List() []agent.SkillMeta {
	var out []agent.SkillMeta
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.root, e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		meta, _, ok := parseFrontmatter(string(data))
		if !ok || strings.TrimSpace(meta["name"]) == "" {
			continue // 无 frontmatter 或无名：不是合法技能，跳过
		}
		out = append(out, agent.SkillMeta{
			Name:        strings.TrimSpace(meta["name"]),
			Description: strings.TrimSpace(meta["description"]),
			Tags:        splitTags(meta["tags"]),
		})
	}
	return out
}

// LoadSkill 按技能名查找并返回 SKILL.md 正文（剥离 frontmatter）。
// 每次调用现扫目录（demo 够用）；生产可加缓存/mtime 感知。
func (p *dirSkillProvider) LoadSkill(name string) (string, bool) {
	dir, ok := p.dirOf(name)
	if !ok {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", false
	}
	_, body, ok := parseFrontmatter(string(data))
	return strings.TrimSpace(body), ok
}

// LoadReference 返回技能附属资源正文（L3 按需引用）。
// 安全约束：refPath 必须是技能目录内的相对路径——filepath.IsLocal 拒绝
// 绝对路径和 .. 穿越（skill 目录里如果有恶意/误写的引用，不能借此读仓库外文件）。
func (p *dirSkillProvider) LoadReference(skillName, refPath string) (string, bool) {
	dir, ok := p.dirOf(skillName)
	if !ok {
		return "", false
	}
	clean := filepath.Clean(refPath)
	if !filepath.IsLocal(clean) {
		return "", false // 绝对路径或 .. 穿越：拒绝
	}
	data, err := os.ReadFile(filepath.Join(dir, clean))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// dirOf 按技能名（frontmatter 的 name）找到技能所在目录。
func (p *dirSkillProvider) dirOf(name string) (string, bool) {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.root, e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		meta, _, ok := parseFrontmatter(string(data))
		if !ok {
			continue
		}
		if strings.TrimSpace(meta["name"]) == name {
			return filepath.Join(p.root, e.Name()), true
		}
	}
	return "", false
}

// parseFrontmatter 极简解析 "---\nkey: value\n---\n正文"。
// 生产建议用 yaml 库；这里手写够 demo 用，且不引入依赖。
// JS/TS ↔ Go 差异：TS 常用逐行 split + trim 实现小解析器；Go 的 strings.Cut
// 一次拆出 key/value（对应 JS 的 indexOf+slice，但少一次查找）。
func parseFrontmatter(s string) (map[string]string, string, bool) {
	s = strings.TrimPrefix(s, "\ufeff") // BOM（Windows 编辑器可能写入）
	if !strings.HasPrefix(s, "---") {
		return nil, s, false
	}
	rest := s[3:] // 跳过开头的 ---
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, s, false
	}
	head := rest[:idx]
	body := strings.TrimPrefix(rest[idx+4:], "\n") // 跳过 \n--- 和后续换行

	meta := map[string]string{}
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return meta, body, true
}

// splitTags 解析 frontmatter 的 tags 字段：容忍 `[a, b]`、`a, b`、`a b` 三种写法。
func splitTags(s string) []string {
	s = strings.TrimSpace(strings.Trim(s, "[]"))
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// compile-time 断言：dirSkillProvider 确实实现了内核接口。
// JS/TS ↔ Go 差异：TS 是结构化类型（鸭子类型）自动满足；Go 需要显式接口，
// 常用 `var _ Interface = T{}` 让编译器在编译期校验实现，错误更早暴露。
var _ agent.SkillProvider = (*dirSkillProvider)(nil)
