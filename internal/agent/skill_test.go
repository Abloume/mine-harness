package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempSkill 在临时目录写一个 SKILL.md 并返回路径（测试不依赖仓库布局）。
func writeTempSkill(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写临时 SKILL.md 失败: %v", err)
	}
	return p
}

// TestSkillAsToolReadsFile 验证：Skill 包装为工具后，Call 返回 SKILL.md 全文。
func TestSkillAsToolReadsFile(t *testing.T) {
	path := writeTempSkill(t, "---\nname: demo\n---\n# 规则\n1. 先备份再删除\n")

	reg := NewRegistry()
	reg.Register(SkillAsTool(Skill{
		Name:        "demo-skill",
		Description: "演示技能",
		Path:        path,
	}))

	out, err := reg.Call("demo-skill", `{}`)
	if err != nil {
		t.Fatalf("调用 skill 工具失败: %v", err)
	}
	if !strings.Contains(out, "先备份再删除") {
		t.Errorf("工具结果应包含 SKILL.md 正文, got: %q", out)
	}
}

// TestSkillAsToolMissingPath 验证：SKILL.md 不存在 → 返回 error（fail-fast，
// 不能让模型在"规范缺失"下继续自由发挥）。
func TestSkillAsToolMissingPath(t *testing.T) {
	reg := NewRegistry()
	reg.Register(SkillAsTool(Skill{
		Name: "broken-skill",
		Path: filepath.Join(t.TempDir(), "not-exist.md"),
	}))

	if _, err := reg.Call("broken-skill", `{}`); err == nil {
		t.Fatal("SKILL.md 不存在时应返回错误")
	}
}

// TestSkillMetadataConstant 验证：注册后工具的 Description（第一层 metadata）
// 随 List 常驻可见，模型靠它触发加载——对应 Agent Skills 的 metadata 常驻。
func TestSkillMetadataConstant(t *testing.T) {
	reg := NewRegistry()
	reg.Register(SkillAsTool(Skill{
		Name:        "demo-skill",
		Description: "文件操作规范：删除前必须备份",
		BaseRisk:    RiskLow,
		Path:        writeTempSkill(t, "# 正文"),
	}))

	tools := reg.List()
	found := false
	for _, tl := range tools {
		if tl.Name == "demo-skill" {
			found = true
			if !strings.Contains(tl.Description, "删除前必须备份") {
				t.Errorf("Description 应作为常驻 metadata 保留, got: %q", tl.Description)
			}
			if tl.BaseRisk != RiskLow {
				t.Errorf("BaseRisk 应透传, got %v", tl.BaseRisk)
			}
		}
	}
	if !found {
		t.Fatal("skill 包装的工具应出现在注册表")
	}
}

// TestSkillLoadedIntoHistory 端到端验证：loop 中模型调用 skill 工具后，
// SKILL.md 正文确实进入对话历史（History 的 tool 消息包含规范内容）——
// 这就是 A 方案"第二层按需加载"落地为可观察的事实。
func TestSkillLoadedIntoHistory(t *testing.T) {
	path := writeTempSkill(t, "---\nname: demo\n---\n# 安全规则\n删除前必须先调用 backup 工具。\n")

	reg := NewRegistry()
	reg.Register(SkillAsTool(Skill{Name: "policy", Description: "安全规范", Path: path}))
	reg.Register(Tool{
		Name: "backup",
		Execute: func(input string) (string, error) {
			return "backup ok", nil
		},
	})

	// 模型脚本：加载 skill → 按规范先备份 → 回答
	llm := NewMockLLM([]MockDecision{
		{ToolName: "policy", ToolInput: `{}`},
		{ToolName: "backup", ToolInput: `{"file":"a.txt"}`},
		{Content: "已备份后完成。"},
	})

	a := NewAgent(llm, reg, WithMaxSteps(6))
	res := a.Run("删除 a.txt")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s: %s", res.Status, res.Reason)
	}

	// 验证：历史里出现过 skill 正文（说明正文确实进入了模型可见的上下文）
	hist := a.History()
	seen := false
	for _, m := range hist {
		if m.Role == "tool" && strings.Contains(m.Content, "删除前必须先调用 backup 工具") {
			seen = true
		}
	}
	if !seen {
		t.Error("SKILL.md 正文应出现在历史中（作为工具结果注入上下文）")
	}
}
