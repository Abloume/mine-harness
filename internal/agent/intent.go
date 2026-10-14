package agent

import (
	"regexp"
	"strings"
)

// 增量 D · 意图路由先于多 Agent（第 7 步前置步骤）。
//
// 定位：拆多 Agent 之前先回答"用户到底要什么"。单 Agent 频繁在多种职责间
// 打架时，先做意图分类/路由，让不同意图走不同链路（查知识→检索链、
// 改数据→审批链、闲聊→直答），一句话含多个意图时拆成多条并行查询。
//
// 与 Summarizer / MemoryExtractor 同模式：内核定义窄接口 + 默认启发式实现
// （标准库优先、零成本、确定性强），宿主可换成调 LLM 的版本（真实分类）。
//
// JS/TS ↔ Go 差异：TS 常用正则 + includes 链式判断写意图路由；
// Go 用包级编译好的 *regexp.Regexp + 关键词计数打分，
// 编译一次复用（regexp.MustCompile 包级变量），避免每次调用重复编译。

// Intent 是意图分类取值。
const (
	IntentQueryKnowledge Intent = "query-knowledge" // 查知识：检索/读取/了解
	IntentMutateData     Intent = "mutate-data"     // 改数据：写类动作（审批链）
	IntentService        Intent = "service"         // 售后/事务：退款/订单/客服
	IntentChitChat       Intent = "chit-chat"       // 闲聊：寒暄/无实质任务
	IntentUnknown        Intent = "unknown"         // 无法判定
)

// Intent 类型定义（可读性）。
type Intent string

// IntentResult 是单意图分类 + 路由结果。
type IntentResult struct {
	Intent     Intent   // 分类结果
	Confidence float64  // 置信度（0-1：该意图命中数 / 总命中数；无命中为 0）
	Route      string   // 路由链路（RouteIntent 决定）
	Matched    []string // 命中的关键词（可观测：解释"为什么这么分"）
}

// IntentClassifier 意图分类器窄接口（宿主可换 LLM 版本）。
type IntentClassifier interface {
	Classify(task string) IntentResult
}

// IntentSplitter 多意图拆分器：一句话含多个意图 → 子句列表（并行查询前提）。
type IntentSplitter func(task string) []string

// 启发式分类器的关键词表：强词（权重 ×2）+ 普通词（权重 ×1）。
//
// 分级依据（两个刻意的设计决定）：
//  1. 动作强度——"改/删/写"是强动作（触发审批链），"更新/提交"是双义词
//     （既可能是动作"提交代码"，也可能是查询对象"谁提交的/多少次更新"），
//     双义词权重降到与查询词同档，靠语义指示词（多少/谁/什么）拉回查询侧；
//  2. 安全侧不对称——模糊句宁可按改写进审批，也不按查询绕过审批
//     （tie-break 优先级 mutate > service > query > chit）。
var intentStrongKeywords = map[Intent][]string{
	IntentMutateData: {
		"改", "写", "删", "修", "删除", "修复", "替换", "合并", "推送",
		"发布", "保存", "新建", "编辑", "移动", "增",
	},
	IntentQueryKnowledge: {
		"多少", "谁", "什么", "为什么", "有哪些", "如何",
		"怎么做", "怎么实现", "怎么解决", "怎么写",
	},
	IntentService: {
		"退款", "退货", "取消订阅",
	},
	IntentChitChat: {},
}

var intentNormalKeywords = map[Intent][]string{
	IntentMutateData: {
		"更新", "提交", "创建", "添加",
	},
	IntentQueryKnowledge: {
		"查", "看", "读", "找", "了解", "介绍", "说明", "解释",
		"统计", "数量", "内容", "区别",
	},
	IntentService: {
		"售后", "客服", "投诉", "订单", "优惠", "发票", "支付", "理赔", "物流",
	},
	IntentChitChat: {
		"你好", "嗨", "谢谢", "再见", "拜拜", "在吗", "随便聊聊", "辛苦了",
		"加油", "哈哈",
	},
}

// 编译一次、包级复用（正则代价高，勿每次调用都编译）。
var (
	intentStrongRegexps = compileIntentRegexps(intentStrongKeywords)
	intentNormalRegexps = compileIntentRegexps(intentNormalKeywords)
)

func compileIntentRegexps(kws map[Intent][]string) map[Intent][]*regexp.Regexp {
	out := make(map[Intent][]*regexp.Regexp)
	for it, words := range kws {
		for _, w := range words {
			out[it] = append(out[it], regexp.MustCompile(w))
		}
	}
	return out
}

// intentPriority 平局优先级（权重并列第一时使用）：
// mutate > service > query > chit（按"误判成本高者优先"）。
var intentPriority = []Intent{IntentMutateData, IntentService, IntentQueryKnowledge, IntentChitChat}

// HeuristicIntentClassifier 是默认启发式分类器：
// 逐意图对任务文本跑强词/普通词两组正则，加权计分（强词 ×2、普通词 ×1），
// 最高分意图胜出（平局按 intentPriority）；无任何命中 → IntentUnknown。
type HeuristicIntentClassifier struct{}

// Classify 实现 IntentClassifier。
func (c *HeuristicIntentClassifier) Classify(task string) IntentResult {
	scores := make(map[Intent]int)
	var matched []string
	total := 0
	for it, res := range intentStrongRegexps {
		n := 0
		for _, re := range res {
			if re.MatchString(task) {
				n++
				matched = append(matched, re.String())
			}
		}
		if n > 0 {
			scores[it] += 2 * n
			total += 2 * n
		}
	}
	for it, res := range intentNormalRegexps {
		n := 0
		for _, re := range res {
			if re.MatchString(task) {
				n++
				matched = append(matched, re.String())
			}
		}
		if n > 0 {
			scores[it] += n
			total += n
		}
	}
	if total == 0 {
		return IntentResult{Intent: IntentUnknown, Confidence: 0, Route: RouteIntent(IntentUnknown)}
	}
	// 取加权最高分；平局按 intentPriority 顺序取第一个（确定性优先）
	best, bestScore := IntentUnknown, 0
	for _, it := range intentPriority {
		if scores[it] > bestScore {
			best, bestScore = it, scores[it]
		}
	}
	return IntentResult{
		Intent:     best,
		Confidence: float64(bestScore) / float64(total),
		Route:      RouteIntent(best),
		Matched:    matched,
	}
}

// RouteIntent 把意图映射到链路（路由决策）。
// 路由结果由宿主接走：检索链查知识、审批链改数据、服务流处理售后、直答回闲聊。
func RouteIntent(it Intent) string {
	switch it {
	case IntentQueryKnowledge:
		return "retrieve" // 检索/读取链
	case IntentMutateData:
		return "approval" // 审批链（写类动作必须过闸门）
	case IntentService:
		return "service-flow" // 售后/事务流
	case IntentChitChat:
		return "direct-reply" // 直答，不走工具
	default:
		return "fallback" // 兜底：继续走常规 loop
	}
}

// HeuristicIntentSplitter 是默认多意图拆分器：按标点与连接词切分。
// 标点：顿号/逗号/分号/句号；连接词："还有/以及/另外/同时/并且"。
//
// JS/TS ↔ Go 差异：JS 的 split(/[，、；。]|还有|以及/ ) 即可；
// Go 的 regexp 支持 UTF-8，但 split 语义需 strings.FieldsFunc 或
// regexp.Split——这里用 strings 包先替换连接词再按标点切（避免转义问题）。
func HeuristicIntentSplitter(task string) []string {
	// 连接词替换成顿号，再统一按标点切分
	repl := strings.NewReplacer(
		"还有", "、", "以及", "、", "另外", "、", "同时", "、", "并且", "、",
	)
	norm := repl.Replace(task)
	var out []string
	for _, seg := range strings.FieldsFunc(norm, func(r rune) bool {
		switch r {
		case '、', '，', ',', '；', ';', '。', '?', '？', '！', '!':
			return true
		}
		return false
	}) {
		if s := strings.TrimSpace(seg); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SplitAndClassify 是"拆分 + 逐句分类 + 路由"的便捷组合：
// 一句话 → 子句列表 → 每句一个 IntentResult。
// 宿主可直接用它做多意图并行查询的前置（每路按 Route 接不同链路）。
func SplitAndClassify(task string, c IntentClassifier) []IntentResult {
	parts := HeuristicIntentSplitter(task)
	if len(parts) == 0 {
		parts = []string{task}
	}
	out := make([]IntentResult, 0, len(parts))
	for _, p := range parts {
		out = append(out, c.Classify(p))
	}
	return out
}
