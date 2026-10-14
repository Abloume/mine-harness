package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mine-harness/internal/agent"
)

// writeSkillFile 在 root/<dir>/SKILL.md 写入内容并返回 root。
func writeSkillFile(t *testing.T, root, dir, content string) {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("建技能目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("写 SKILL.md 失败: %v", err)
	}
}

// TestDirSkillProviderList 验证宿主发现：List 返回 frontmatter 的
// name/description/tags，LoadSkill 按名返回正文。
func TestDirSkillProviderList(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, root, "example", `---
name: file-ops-policy
description: 文件操作安全规范
tags: [files, safety]
---
# 正文
删除前必须备份。`)
	// 第二个目录：无 frontmatter（应被跳过）
	writeSkillFile(t, root, "broken", "# 没有 frontmatter 的文件\n")

	p := newDirSkillProvider(root)
	metas := p.List()
	if len(metas) != 1 {
		t.Fatalf("应只发现 1 个合法技能, got %d: %+v", len(metas), metas)
	}
	m := metas[0]
	if m.Name != "file-ops-policy" || m.Description != "文件操作安全规范" {
		t.Errorf("List 应解析 name/description, got: %+v", m)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "files" || m.Tags[1] != "safety" {
		t.Errorf("List 应解析 tags, got: %+v", m.Tags)
	}

	body, ok := p.LoadSkill("file-ops-policy")
	if !ok || !strings.Contains(body, "删除前必须备份") {
		t.Errorf("LoadSkill 应命中并返回正文, ok=%v body=%q", ok, body)
	}
	if _, ok := p.LoadSkill("ghost"); ok {
		t.Error("LoadSkill 未知技能应返回 ok=false")
	}
}

// TestSplitTags 验证三种 tags 写法统一解析。
func TestSplitTags(t *testing.T) {	cases := []struct {
		in   string
		want []string
	}{
		{"[files, safety]", []string{"files", "safety"}},
		{"files, safety", []string{"files", "safety"}},
		{"files safety", []string{"files", "safety"}},
		{"", nil},
		{"[]", nil},
	}
	for _, c := range cases {
		got := splitTags(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitTags(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitTags(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// TestDirSkillProviderLoadReference 验证 L3 宿主读取：正常命中返回引用正文；
// 目录穿越（../ 或绝对路径）一律拒绝；未知技能/未知引用返回 ok=false。
func TestDirSkillProviderLoadReference(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, root, "example", `---
name: file-ops-policy
description: 安全规范
---
# 正文
删除前读取 references/safety.md`)
	// 引用文件在技能目录内
	if err := os.MkdirAll(filepath.Join(root, "example", "references"), 0o755); err != nil {
		t.Fatalf("建 references 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "example", "references", "safety.md"), []byte("# 检查清单\n1. 已备份"), 0o644); err != nil {
		t.Fatalf("写引用文件失败: %v", err)
	}

	p := newDirSkillProvider(root)

	body, ok := p.LoadReference("file-ops-policy", "references/safety.md")
	if !ok || !strings.Contains(body, "1. 已备份") {
		t.Errorf("正常引用应命中, ok=%v body=%q", ok, body)
	}

	// 目录穿越：../ 和绝对路径必须拒绝
	for _, bad := range []string{"../outside.md", "/etc/passwd", "references/../../x.md"} {
		if _, ok := p.LoadReference("file-ops-policy", bad); ok {
			t.Errorf("危险引用 %q 应被拒绝", bad)
		}
	}

	// 未知技能 / 未知引用
	if _, ok := p.LoadReference("ghost", "references/safety.md"); ok {
		t.Error("未知技能应返回 ok=false")
	}
	if _, ok := p.LoadReference("file-ops-policy", "references/nope.md"); ok {
		t.Error("未知引用应返回 ok=false")
	}
}

// TestRepoSkillsDiscoverable 第 6 步配套：仓库真实 skills/ 目录必须能发现
// 4 个开发工作流 Skill（fix-bug / implement-feature / run-tests-fix /
// visual-verify），且 LoadSkill 各能命中正文（流程骨架完整）。
// 这是"新能力必须配 demo + 单测"的落地——Skill 文件本身可被宿主加载。
func TestRepoSkillsDiscoverable(t *testing.T) {
	p := newDirSkillProvider("skills")
	metas := p.List()
	got := map[string]agent.SkillMeta{}
	for _, m := range metas {
		got[m.Name] = m
	}

	want := []string{"fix-bug", "implement-feature", "run-tests-fix", "visual-verify"}
	for _, name := range want {
		m, ok := got[name]
		if !ok {
			t.Errorf("skills/ 应发现 %s（实际清单: %v）", name, got)
			continue
		}
		if m.Description == "" {
			t.Errorf("%s 的 description 不能为空", name)
		}
		body, ok := p.LoadSkill(name)
		if !ok || body == "" {
			t.Errorf("LoadSkill(%s) 应命中正文", name)
			continue
		}
		// 每个工作流 Skill 必须包含：验证清单 + 失败回滚/停止路径
		for _, kw := range []string{"验证清单", "失败时怎么停"} {
			if !strings.Contains(body, kw) {
				t.Errorf("%s 正文缺少「%s」小节（工作流 Skill 硬要求）", name, kw)
			}
		}
	}

	// 规范型 Skill（fe-practices）：只要求被发现 + 按「场景/规则/理由/反例」组织
	m, ok := got["fe-practices"]
	if !ok {
		t.Fatalf("skills/ 应发现 fe-practices（实际清单: %v）", got)
	}
	if m.Description == "" {
		t.Error("fe-practices 的 description 不能为空")
	}
	body, ok := p.LoadSkill("fe-practices")
	if !ok || body == "" {
		t.Fatal("LoadSkill(fe-practices) 应命中正文")
	}
	for _, kw := range []string{"场景", "规则", "理由", "反例"} {
		if !strings.Contains(body, kw) {
			t.Errorf("fe-practices 正文缺少「%s」小节（规范型 Skill 硬要求）", kw)
		}
	}
}
