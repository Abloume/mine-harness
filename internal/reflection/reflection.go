// Package reflection 实现 Agent 交付前的自检环节（Reflection）。
//
// 设计理念：Reflection 不是"及格线检查"，而是"高标准验收"。
// 检查标准不是固定的几条，而是根据任务类型动态生成的。
//
// 跟 eval 的区别：
//   - eval：外部评测，跑完任务后独立跑测试用例
//   - reflection：内部自检，Agent 交付前自己按标准检查输出
//
// JS/TS ↔ Go 差异：
//   - TS 常用策略模式（不同 Reflector 注入不同检查逻辑）；
//   - Go 用接口 + 结构体，编译期类型安全。
package reflection

import (
	"fmt"
	"strings"
)

// Issue 是一个发现的问题。
type Issue struct {
	Severity string `json:"severity"` // high / medium / low
	Standard string `json:"standard"` // 违反了哪条标准
	Message  string `json:"message"`  // 问题描述
}

// Result 是自检的结果。
type Result struct {
	Passed     bool    `json:"passed"`     // 是否通过
	Score      int     `json:"score"`      // 得分（0-100）
	Issues     []Issue `json:"issues"`     // 发现的问题列表
	TotalChecks int    `json:"total_checks"` // 总检查项数
}

// Message 是对话消息的简化版（跟 agent.Message 对应，但不耦合 agent 包）。
type Message struct {
	Role    string
	Content string
}

// CheckStandard 是一条检查标准。
//
// 不是"空不空"这种及格线，而是"这个任务应该达到什么标准"。
// 标准分两类：
//   - Required=true：必过项，缺了就不合格
//   - Required=false：质量项，有更好，没有也能过
type CheckStandard struct {
	ID          string `json:"id"`          // 标准 ID
	Dimension   string `json:"dimension"`   // 检查维度（正确性/完整性/规范性/可落地性）
	Description string `json:"description"` // 标准描述
	PassIf      string `json:"pass_if"`     // 怎么算通过
	Required    bool   `json:"required"`    // 是否为必过项
}

// TaskProfile 是任务画像——根据任务类型匹配对应的检查标准。
type TaskProfile struct {
	Type      string          `json:"type"`      // 任务类型：sql / bug-fix / report / code / general
	Name      string          `json:"name"`      // 类型名称
	Standards []CheckStandard `json:"standards"` // 这个类型的检查标准列表
}

// Reflector 是自检器接口。
type Reflector interface {
	Check(answer string, task string, history []Message) Result
}

// ============================================================
// 任务画像库——内置几类常见任务的高标准检查清单
// ============================================================

// TaskProfileLibrary 是任务画像库——根据任务描述匹配对应的检查标准。
type TaskProfileLibrary struct {
	profiles []TaskProfile
}

// NewDefaultLibrary 创建默认的任务画像库。
//
// 内置四类常见任务的高标准检查清单：
//   - SQL 类
//   - Bug 修复类
//   - 报告分析类
//   - 代码实现类
func NewDefaultLibrary() *TaskProfileLibrary {
	return &TaskProfileLibrary{
		profiles: []TaskProfile{
			sqlProfile,
			bugFixProfile,
			reportProfile,
			codeProfile,
		},
	}
}

// Match 根据任务描述匹配最合适的任务画像。
//
// 简单实现：关键词匹配。
// 真实系统可以用 LLM 分类任务类型。
func (l *TaskProfileLibrary) Match(task string) TaskProfile {
	taskLower := strings.ToLower(task)

	// 按优先级匹配——越具体的越先匹配
	if strings.Contains(taskLower, "sql") ||
		strings.Contains(taskLower, "查询") ||
		strings.Contains(taskLower, "数据库") {
		return sqlProfile
	}

	if strings.Contains(taskLower, "bug") ||
		strings.Contains(taskLower, "修复") ||
		strings.Contains(taskLower, "报错") ||
		strings.Contains(taskLower, "问题") {
		return bugFixProfile
	}

	if strings.Contains(taskLower, "报告") ||
		strings.Contains(taskLower, "分析") ||
		strings.Contains(taskLower, "总结") ||
		strings.Contains(taskLower, "原因") {
		return reportProfile
	}

	if strings.Contains(taskLower, "代码") ||
		strings.Contains(taskLower, "实现") ||
		strings.Contains(taskLower, "函数") ||
		strings.Contains(taskLower, "组件") {
		return codeProfile
	}

	// 默认用通用检查
	return generalProfile
}

// ============================================================
// 四类任务的高标准检查清单
// ============================================================

var sqlProfile = TaskProfile{
	Type: "sql",
	Name: "SQL 查询类",
	Standards: []CheckStandard{
		{
			ID:          "sql-1",
			Dimension:   "正确性",
			Description: "是否包含 SELECT 语句",
			PassIf:      "回答中包含 SELECT 关键字（不区分大小写）",
			Required:    true, // 必过项：没有 SELECT 就不是 SQL
		},
		{
			ID:          "sql-2",
			Dimension:   "正确性",
			Description: "是否包含 WHERE 条件",
			PassIf:      "回答中包含 WHERE 关键字，不是全表扫描",
			Required:    true, // 必过项：没有 WHERE 就是全表扫描，绝对不行
		},
		{
			ID:          "sql-3",
			Dimension:   "正确性",
			Description: "时间范围是否符合任务要求",
			PassIf:      "回答中提到的时间范围跟任务要求一致",
			Required:    false, // 质量项：有更好，没有也能跑
		},
		{
			ID:          "sql-4",
			Dimension:   "规范性",
			Description: "是否有防全表扫描措施",
			PassIf:      "有 WHERE / LIMIT，不是裸 SELECT *",
			Required:    false, // 质量项：有更好，没有也能跑
		},
	},
}

var bugFixProfile = TaskProfile{
	Type: "bug-fix",
	Name: "Bug 修复类",
	Standards: []CheckStandard{
		{
			ID:          "bug-1",
			Dimension:   "完整性",
			Description: "有没有复现问题",
			PassIf:      "回答中提到了怎么复现这个 bug",
			Required:    true, // 必过项：不能复现就没法修
		},
		{
			ID:          "bug-2",
			Dimension:   "正确性",
			Description: "有没有定位根因",
			PassIf:      "回答中说明了问题的根本原因，不是只说现象",
			Required:    true, // 必过项：只说现象不修根因，下次还会犯
		},
		{
			ID:          "bug-3",
			Dimension:   "完整性",
			Description: "有没有修复方案",
			PassIf:      "回答中给出了具体的修复代码或方案",
			Required:    true, // 必过项：没有方案等于白说
		},
		{
			ID:          "bug-4",
			Dimension:   "正确性",
			Description: "有没有验证修复",
			PassIf:      "回答中提到了怎么验证修复是否生效",
			Required:    false, // 质量项：有更好，缺了不算致命
		},
	},
}

var reportProfile = TaskProfile{
	Type: "report",
	Name: "报告分析类",
	Standards: []CheckStandard{
		{
			ID:          "rpt-1",
			Dimension:   "正确性",
			Description: "结论有没有数据支撑",
			PassIf:      "回答中的结论后面跟着数据/证据，不是空口下结论",
			Required:    true, // 必过项：没有数据支撑的结论是瞎猜
		},
		{
			ID:          "rpt-2",
			Dimension:   "完整性",
			Description: "有没有分析原因",
			PassIf:      "回答中说明了为什么会出现这个问题，不只是描述现象",
			Required:    true, // 必过项：只说现象不说原因，报告没意义
		},
		{
			ID:          "rpt-3",
			Dimension:   "可落地性",
			Description: "有没有给出建议",
			PassIf:      "回答中给出了具体的改进建议或行动方案",
			Required:    false, // 质量项：有更好，没有也能交差
		},
		{
			ID:          "rpt-4",
			Dimension:   "完整性",
			Description: "有没有遗漏关键信息",
			PassIf:      "回答覆盖了任务要求的所有方面，没有明显遗漏",
			Required:    false, // 质量项：有更好，没有也能跑
		},
	},
}

var codeProfile = TaskProfile{
	Type: "code",
	Name: "代码实现类",
	Standards: []CheckStandard{
		{
			ID:          "code-1",
			Dimension:   "正确性",
			Description: "代码是否完整可运行",
			PassIf:      "回答中有完整的代码片段，不是伪代码",
			Required:    true, // 必过项：给一段伪代码等于没给
		},
		{
			ID:          "code-2",
			Dimension:   "正确性",
			Description: "有没有边界处理",
			PassIf:      "代码中考虑了空值/异常/边界情况",
			Required:    true, // 必过项：没边界处理的代码上线就崩
		},
		{
			ID:          "code-3",
			Dimension:   "规范性",
			Description: "有没有错误处理",
			PassIf:      "代码中有 try/catch 或错误返回，不是裸跑",
			Required:    false, // 质量项：有更好，没有也能跑
		},
		{
			ID:          "code-4",
			Dimension:   "可落地性",
			Description: "有没有使用说明",
			PassIf:      "回答中说明了怎么使用这段代码",
			Required:    false, // 质量项：有更好，没有也能跑
		},
	},
}

var generalProfile = TaskProfile{
	Type: "general",
	Name: "通用类",
	Standards: []CheckStandard{
		{
			ID:          "gen-1",
			Dimension:   "完整性",
			Description: "回答是否完整",
			PassIf:      "回答不是一两句话，有足够的内容展开",
		},
		{
			ID:          "gen-2",
			Dimension:   "正确性",
			Description: "有没有直接回答问题",
			PassIf:      "回答跟任务相关，不是跑题的废话",
		},
	},
}

// ============================================================
// RuleReflector——基于任务画像的规则版自检器
// ============================================================

// RuleReflector 是规则版自检器——按任务画像匹配检查标准，逐条检查。
//
// 比通用及格线高一级：根据任务类型匹配对应的检查清单，
// SQL 类查 SQL 的标准，Bug 修复类查修复的标准。
type RuleReflector struct {
	library *TaskProfileLibrary
}

// NewRuleReflector 创建一个规则版自检器。
func NewRuleReflector() *RuleReflector {
	return &RuleReflector{
		library: NewDefaultLibrary(),
	}
}

// Check 实现 Reflector 接口。
//
// 判定逻辑：
//  1. 必过项（Required=true）必须全部满足，缺一个就不通过
//  2. 质量项（Required=false）至少满足 50%，才算"良好"
//  3. 必过项全过 + 质量项 >= 50% → 通过
func (r *RuleReflector) Check(answer string, task string, history []Message) Result {
	// 1. 匹配任务画像
	profile := r.library.Match(task)

	// 2. 逐条检查标准
	var issues []Issue
	requiredPassed := 0
	requiredTotal := 0
	qualityPassed := 0
	qualityTotal := 0

	answerLower := strings.ToLower(answer)

	for _, std := range profile.Standards {
		passed := checkStandard(std, answerLower, task)

		if std.Required {
			requiredTotal++
			if passed {
				requiredPassed++
			} else {
				issues = append(issues, Issue{
					Severity: "high",
					Standard: std.Description,
					Message:  fmt.Sprintf("必过项未达标：%s（要求：%s）", std.Dimension, std.PassIf),
				})
			}
		} else {
			qualityTotal++
			if passed {
				qualityPassed++
			} else {
				issues = append(issues, Issue{
					Severity: "medium",
					Standard: std.Description,
					Message:  fmt.Sprintf("质量项未达标：%s（要求：%s）", std.Dimension, std.PassIf),
				})
			}
		}
	}

	// 3. 判定逻辑
	//    - 必过项必须全过
	//    - 质量项至少过 50%
	allRequiredPassed := requiredPassed == requiredTotal
	qualityRatio := 0
	if qualityTotal > 0 {
		qualityRatio = (qualityPassed * 100) / qualityTotal
	}
	passed := allRequiredPassed && qualityRatio >= 50

	// 4. 计算总分（给用户看的，不是判定标准）
	totalChecks := len(profile.Standards)
	totalPassed := requiredPassed + qualityPassed
	score := (totalPassed * 100) / totalChecks

	return Result{
		Passed:      passed,
		Score:       score,
		Issues:      issues,
		TotalChecks: totalChecks,
	}
}

// checkStandard 检查单条标准是否通过。
//
// 这是简化版——用关键词匹配做检查。
// 真实系统可以用 LLM 做更灵活的语义检查。
func checkStandard(std CheckStandard, answerLower string, task string) bool {
	switch std.ID {
	case "sql-1": // 有没有 SELECT
		return strings.Contains(answerLower, "select")

	case "sql-2": // 有没有 WHERE
		return strings.Contains(answerLower, "where")

	case "sql-3": // 时间范围对不对（简化版：检查有没有提到时间）
		// 简化：只要提到了时间相关的词就算过
		timeWords := []string{"昨天", "今天", "最近", "本周", "本月", "yesterday", "today", "last", "recent"}
		for _, w := range timeWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "sql-4": // 有没有防全表扫描
		// 有 WHERE 或 LIMIT 就算过
		return strings.Contains(answerLower, "where") || strings.Contains(answerLower, "limit")

	case "bug-1": // 有没有复现
		reproduceWords := []string{"复现", "重现", "复现步骤", "怎么触发", "reproduce"}
		for _, w := range reproduceWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "bug-2": // 有没有定位根因
		rootCauseWords := []string{"原因", "根因", "根本原因", "因为", "导致", "root cause"}
		for _, w := range rootCauseWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "bug-3": // 有没有修复方案
		fixWords := []string{"修复", "解决", "改成", "修改", "fix", "change"}
		for _, w := range fixWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "bug-4": // 有没有验证
		verifyWords := []string{"验证", "测试", "确认", "检查", "verify", "test"}
		for _, w := range verifyWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "rpt-1": // 结论有没有数据支撑
		// 简化：有没有数字/百分比
		for _, ch := range answerLower {
			if ch >= '0' && ch <= '9' {
				return true
			}
		}
		return false

	case "rpt-2": // 有没有分析原因
		causeWords := []string{"原因", "因为", "导致", "由于", "为什么", "分析"}
		for _, w := range causeWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "rpt-3": // 有没有建议
		suggestWords := []string{"建议", "应该", "可以", "改进", "方案", "建议"}
		for _, w := range suggestWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "rpt-4": // 有没有遗漏（简化版：回答长度够不够）
		return len([]rune(answerLower)) >= 50

	case "code-1": // 代码是否完整
		// 简化：有没有代码块标记
		return strings.Contains(answerLower, "```") || strings.Contains(answerLower, "func ") || strings.Contains(answerLower, "function ")

	case "code-2": // 有没有边界处理
		edgeWords := []string{"空", "nil", "null", "undefined", "边界", "异常", "error"}
		for _, w := range edgeWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "code-3": // 有没有错误处理
		errorWords := []string{"try", "catch", "throw", "err", "error", "异常"}
		for _, w := range errorWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "code-4": // 有没有使用说明
		usageWords := []string{"使用", "用法", "调用", "示例", "example", "usage"}
		for _, w := range usageWords {
			if strings.Contains(answerLower, w) {
				return true
			}
		}
		return false

	case "gen-1": // 回答是否完整
		return len([]rune(answerLower)) >= 30

	case "gen-2": // 有没有直接回答
		// 简化版：默认通过
		return true
	}

	return false
}

// ============================================================
// CheckWithRetry——带重试的完整闭环
// ============================================================

// CheckWithRetry 是带重试的自检：不通过就让模型改，最多 maxRetries 次。
func CheckWithRetry(
	answer string,
	task string,
	history []Message,
	reflector Reflector,
	llmCallback func(originalAnswer string, issues []Issue) string,
	maxRetries int,
) (finalAnswer string, checks []Result) {

	current := answer

	for i := 0; i <= maxRetries; i++ {
		result := reflector.Check(current, task, history)
		checks = append(checks, result)

		if result.Passed {
			return current, checks
		}

		// 最后一次了，不再改
		if i == maxRetries {
			break
		}

		// 让模型改
		current = llmCallback(current, result.Issues)
	}

	return current, checks
}
