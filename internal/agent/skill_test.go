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

// ---- B 方案：真渐进式披露（load_skill + system 注入）----

// stubSkillProvider 是测试用 SkillProvider（不依赖文件系统）。
type stubSkillProvider struct {
	skills  map[string]string            // name → 正文
	refs    map[string]string            // "skill/ref" → 引用正文
	meta    []SkillMeta                  // 常驻清单（第一层）
}

func (s *stubSkillProvider) List() []SkillMeta {
	if s.meta != nil {
		return s.meta
	}
	// 未显式给清单时，从 skills map 推断（测试省事）
	var out []SkillMeta
	for name := range s.skills {
		out = append(out, SkillMeta{Name: name})
	}
	return out
}

func (s *stubSkillProvider) LoadSkill(name string) (string, bool) {
	body, ok := s.skills[name]
	return body, ok
}

func (s *stubSkillProvider) LoadReference(skill, ref string) (string, bool) {
	body, ok := s.refs[skill+"/"+ref]
	return body, ok
}

// TestLoadSkillBInjectsSystem 验证 B 方案核心行为：
// 模型调用 load_skill 后，SKILL.md 正文以 **system 角色**进入上下文
// （不是 A 方案的 tool 角色），且 tool 消息协议闭合（关联 tool_call_id）。
func TestLoadSkillBInjectsSystem(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewLoadSkillTool())

	llm := NewMockLLM([]MockDecision{
		{ToolName: LoadSkillToolName, ToolInput: `{"name":"policy"}`},
		{Content: "按规范执行完毕。"},
	})

	sp := &stubSkillProvider{skills: map[string]string{
		"policy": "# 安全规范\n删除前必须先调用 backup 工具。\n",
	}}
	a := NewAgent(llm, reg,
		WithMaxSteps(4),
		WithSkillProvider(sp),
	)
	res := a.Run("删除 a.txt")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s: %s", res.Status, res.Reason)
	}

	var sysSeen, toolSeen, closed bool
	for _, m := range a.History() {
		if m.Role == "system" && strings.Contains(m.Content, "删除前必须先调用 backup 工具") {
			sysSeen = true
		}
		if m.Role == "tool" && strings.Contains(m.Content, "已加载") {
			toolSeen = true
			closed = m.ToolCallID != ""
		}
	}
	if !sysSeen {
		t.Error("B 方案：SKILL.md 正文应以 system 角色注入上下文")
	}
	if !toolSeen {
		t.Error("B 方案：应回填一条 tool 消息告知模型技能已加载")
	}
	if !closed {
		t.Error("B 方案：tool 消息应关联 tool_call_id（协议闭合）")
	}
}

// TestLoadSkillBDedupe 验证去重：同一技能重复加载只注入一次 system，
// 第二次回填"已加载过"，模型不会反复刷规范进上下文。
func TestLoadSkillBDedupe(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewLoadSkillTool())

	llm := NewMockLLM([]MockDecision{
		{ToolName: LoadSkillToolName, ToolInput: `{"name":"policy"}`},
		{ToolName: LoadSkillToolName, ToolInput: `{"name":"policy"}`},
		{Content: "完成。"},
	})

	sp := &stubSkillProvider{skills: map[string]string{"policy": "# 规范\n正文内容"}}
	a := NewAgent(llm, reg, WithMaxSteps(5), WithSkillProvider(sp))
	res := a.Run("删除 a.txt")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s: %s", res.Status, res.Reason)
	}

	sysCount := 0
	for _, m := range a.History() {
		if m.Role == "system" && strings.Contains(m.Content, "正文内容") {
			sysCount++
		}
	}
	if sysCount != 1 {
		t.Errorf("同一技能应只注入一次 system 正文, got %d 次", sysCount)
	}
}

// TestLoadSkillBNotFound 验证兜底：技能不存在 → 回填错误 tool 消息、不注入 system，
// 模型拿到反馈后可以换技能或直接完成任务（不中断 loop）。
func TestLoadSkillBNotFound(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewLoadSkillTool())

	llm := NewMockLLM([]MockDecision{
		{ToolName: LoadSkillToolName, ToolInput: `{"name":"ghost"}`},
		{Content: "技能不存在，我直接完成任务。"},
	})

	sp := &stubSkillProvider{skills: map[string]string{"policy": "x"}}
	a := NewAgent(llm, reg, WithMaxSteps(4), WithSkillProvider(sp))
	res := a.Run("完成任务")
	if res.Status != StatusCompleted {
		t.Fatalf("找不到技能不应中断 loop, got %s: %s", res.Status, res.Reason)
	}

	for _, m := range a.History() {
		if m.Role == "system" && strings.Contains(m.Content, "ghost") {
			t.Error("找不到的技能不应注入 system 消息")
		}
	}
}

// TestParseLoadSkillInput 验证参数解析的三种形态：JSON、裸引号名、空。
func TestParseLoadSkillInput(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`{"name":"file-ops-policy"}`, "file-ops-policy"},
		{`"policy"`, "policy"},
		{`policy`, "policy"},
		{`{"foo":"bar"}`, ""}, // 无 name 字段
		{`   `, ""},           // 空白
	}
	for _, c := range cases {
		if got := parseLoadSkillInput(c.input); got != c.want {
			t.Errorf("parseLoadSkillInput(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestBuildLoadSkillDescription 验证 metadata 层：清单（name + description + tags）
// 拼进 load_skill 的描述——模型每次请求都能"看到目录"，这是渐进式披露第一层。
func TestBuildLoadSkillDescription(t *testing.T) {
	desc := BuildLoadSkillDescription([]SkillMeta{
		{Name: "file-ops-policy", Description: "删除前必须备份", Tags: []string{"files", "safety"}},
		{Name: "draft-policy", Description: "先起草再发送"},
	})

	for _, want := range []string{
		"file-ops-policy",            // 技能名
		"删除前必须备份",                // description
		"[tags: files, safety]",      // tags
		"draft-policy", "先起草再发送",   // 第二个技能
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("描述应包含 %q, got: %s", want, desc)
		}
	}
}

// TestReadSkillRefInjectsSystem 验证 L3 端到端：先 load_skill 再 read_skill_ref，
// 引用正文以 system 角色注入、tool 消息协议闭合。
func TestReadSkillRefInjectsSystem(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewLoadSkillTool())
	reg.Register(NewReadSkillRefTool())

	llm := NewMockLLM([]MockDecision{
		{ToolName: LoadSkillToolName, ToolInput: `{"name":"policy"}`},
		{ToolName: ReadSkillRefToolName, ToolInput: `{"skill":"policy","ref":"references/safety.md"}`},
		{Content: "按引用清单核对后完成。"},
	})

	sp := &stubSkillProvider{
		skills: map[string]string{"policy": "# 规范\n删除前读取检查清单"},
		refs:   map[string]string{"policy/references/safety.md": "# 检查清单\n1. 已备份\n2. 非关键文件"},
	}
	a := NewAgent(llm, reg, WithMaxSteps(5), WithSkillProvider(sp))
	res := a.Run("删除 a.txt")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s: %s", res.Status, res.Reason)
	}

	var refSys, refClosed bool
	for _, m := range a.History() {
		if m.Role == "system" && strings.Contains(m.Content, "1. 已备份") {
			refSys = true
		}
		if m.Role == "tool" && strings.Contains(m.Content, "已加载技能 policy 的引用") {
			refClosed = m.ToolCallID != ""
		}
	}
	if !refSys {
		t.Error("L3：引用正文应以 system 角色注入上下文")
	}
	if !refClosed {
		t.Error("L3：tool 消息应关联 tool_call_id（协议闭合）")
	}
}

// TestReadSkillRefRequiresLoadedSkill 验证顺序约束：未加载技能就读引用 → 拒绝，
// 引用必须依附于已加载的正文（防绕过 L2 直接读资源）。
func TestReadSkillRefRequiresLoadedSkill(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewLoadSkillTool())
	reg.Register(NewReadSkillRefTool())

	llm := NewMockLLM([]MockDecision{
		{ToolName: ReadSkillRefToolName, ToolInput: `{"skill":"policy","ref":"references/safety.md"}`},
		{Content: "提示未加载，我先加载技能。"},
	})

	sp := &stubSkillProvider{skills: map[string]string{"policy": "x"}}
	a := NewAgent(llm, reg, WithMaxSteps(4), WithSkillProvider(sp))
	res := a.Run("删除 a.txt")
	if res.Status != StatusCompleted {
		t.Fatalf("顺序约束不应中断 loop, got %s: %s", res.Status, res.Reason)
	}

	seen := false
	for _, m := range a.History() {
		if m.Role == "tool" && strings.Contains(m.Content, "尚未加载") {
			seen = true
		}
		if m.Role == "system" && strings.Contains(m.Content, "safety.md") {
			t.Error("未加载技能时，引用不应注入 system")
		}
	}
	if !seen {
		t.Error("未加载技能时读引用，应回填'尚未加载'提示")
	}
}

// TestReadSkillRefDedupe 验证引用去重：同一 (skill, ref) 只注入一次 system。
func TestReadSkillRefDedupe(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewLoadSkillTool())
	reg.Register(NewReadSkillRefTool())

	llm := NewMockLLM([]MockDecision{
		{ToolName: LoadSkillToolName, ToolInput: `{"name":"policy"}`},
		{ToolName: ReadSkillRefToolName, ToolInput: `{"skill":"policy","ref":"references/safety.md"}`},
		{ToolName: ReadSkillRefToolName, ToolInput: `{"skill":"policy","ref":"references/safety.md"}`},
		{Content: "完成。"},
	})

	sp := &stubSkillProvider{
		skills: map[string]string{"policy": "正文"},
		refs:   map[string]string{"policy/references/safety.md": "# 清单\n内容"},
	}
	a := NewAgent(llm, reg, WithMaxSteps(6), WithSkillProvider(sp))
	res := a.Run("删除 a.txt")
	if res.Status != StatusCompleted {
		t.Fatalf("应正常完成, got %s: %s", res.Status, res.Reason)
	}

	count := 0
	for _, m := range a.History() {
		// system 注入标记格式：[技能 policy 引用 references/safety.md]
		if m.Role == "system" && strings.Contains(m.Content, "references/safety.md") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("同一引用应只注入一次, got %d 次", count)
	}
}

// TestParseReadSkillRefInput 验证 L3 参数解析：JSON 双字段、缺字段、非法。
func TestParseReadSkillRefInput(t *testing.T) {
	cases := []struct {
		input    string
		skill    string
		ref      string
		ok       bool
	}{
		{`{"skill":"file-ops-policy","ref":"references/safety.md"}`, "file-ops-policy", "references/safety.md", true},
		{`{"ref":"references/safety.md"}`, "", "", false}, // 缺 skill
		{`{"skill":"x"}`, "", "", false},                   // 缺 ref
		{`not-json`, "", "", false},                        // 非 JSON
	}
	for _, c := range cases {
		skill, ref, ok := parseReadSkillRefInput(c.input)
		if ok != c.ok || skill != c.skill || ref != c.ref {
			t.Errorf("parseReadSkillRefInput(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.input, skill, ref, ok, c.skill, c.ref, c.ok)
		}
	}
}
