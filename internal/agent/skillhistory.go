package agent

import (
	"fmt"
	"strings"
)

// 本文件是"Skill History 短期工作记忆"（缺口 B）的实现：
// 环形 buffer 记录最近 N 步工具调用（工具名 / 参数摘要 / 成败），
// 压缩上下文时把快照注入 system——模型看到"已尝试过哪些操作、结果如何"，
// 就不会语义上重复同一失败路径。
//
// 与循环检测（LoopGuard）的分工（README 约定）：
//   - 循环检测拦「精确重复」：同一工具 + 完全相同参数；
//   - Skill History 拦「语义重复」：同一失败路径换着参数/工具再来一遍。
// 前者是硬护栏（命中即警告/中止），后者是软提醒（注入历史，靠模型收敛）。

// HistoryEntry 是一次工具调用的摘要记录（参数截断，防快照本身撑爆上下文）。
type HistoryEntry struct {
	Step  int    // 发生步数（定位用）
	Tool  string // 工具名
	Input string // 参数摘要（截断到 maxInputLen）
	OK    bool   // 是否成功（含 schema 校验：校验失败也算失败）
}

// SkillHistory 是固定容量的环形 buffer。
//
// JS/TS ↔ Go 差异：JS 无内置环形结构，常用"数组 + 头尾索引取模"手写，
// Go 同样手写——但 Go 的切片裁剪语义（buf[pos:] 视图）让"覆盖旧条目"
// 更直接：环形写入 = 按下标赋值，读取时按时间序拼接两个分段。
type SkillHistory struct {
	size int
	buf  []HistoryEntry // 长度恒为 size；未写满的槽位是零值
	next int            // 下一个写入位置（环形下标）
	full bool           // 是否已写满（决定读取起点）
}

// NewSkillHistory 构造容量 size 的环形历史（size<=0 时退化为禁用：
// Record 空操作、String 返回空——调用方判空即可）。
func NewSkillHistory(size int) *SkillHistory {
	if size <= 0 {
		return &SkillHistory{size: 0}
	}
	return &SkillHistory{size: size, buf: make([]HistoryEntry, size)}
}

// Record 写入一条调用记录。最新条目覆盖最旧条目（环形）。
func (h *SkillHistory) Record(step int, tool, input string, ok bool) {
	if h == nil || h.size == 0 {
		return
	}
	h.buf[h.next] = HistoryEntry{Step: step, Tool: tool, Input: truncate(input, 60), OK: ok}
	h.next = (h.next + 1) % h.size
	if h.next == 0 {
		h.full = true
	}
}

// Snapshot 返回按时间序（最旧 → 最新）的全部条目；未写满时跳过零值槽位。
func (h *SkillHistory) Snapshot() []HistoryEntry {
	if h == nil || h.size == 0 {
		return nil
	}
	out := make([]HistoryEntry, 0, h.size)
	if h.full {
		// 从 next（最旧）绕到 next-1（最新）
		for i := 0; i < h.size; i++ {
			out = append(out, h.buf[(h.next+i)%h.size])
		}
	} else {
		out = append(out, h.buf[:h.next]...)
	}
	return out
}

// String 格式化快照供 system 注入："step N: tool(参数摘要) [成功|失败]"。
// 空历史返回空串（调用方据此决定是否注入）。
func (h *SkillHistory) String() string {
	entries := h.Snapshot()
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		state := "成功"
		if !e.OK {
			state = "失败"
		}
		fmt.Fprintf(&b, "step %d: %s(%s) [%s]\n", e.Step, e.Tool, e.Input, state)
	}
	return b.String()
}

// truncate 把长参数截断成摘要（保留头部，避免快照占太多 token）。
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
