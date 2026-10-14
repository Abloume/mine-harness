package evolve

import "strings"

// Optimize 基于改写建议，产出新版本的 SKILL.md 文本。
//
// 最小修改原则（对应 SkillForge 的 "minimal modification"）：
//   - 只追加新段落，不修改或删除原有内容
//   - 这样保证原来正确的行为不被破坏（论文里验证过：大改会引入回归）
//
// JS/TS ↔ Go 差异：TS 里拼字符串常用模板字面量；
// Go 用 strings.Builder（对应 TS 的数组 join，但更高效——Builder 内部按需扩容）。
func Optimize(oldBody string, suggestions []SuggestedChange) string {
	if len(suggestions) == 0 {
		return oldBody
	}

	var b strings.Builder
	b.WriteString(strings.TrimSpace(oldBody))
	b.WriteString("\n\n---\n\n")

	for i, s := range suggestions {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s.Content)
	}

	return b.String()
}
