package agent

import (
	"strings"
	"testing"
)

// 增量 D · 意图路由：分类器命中、多意图拆分、路由映射、组合拆分+分类。

func TestHeuristicIntentClassifier(t *testing.T) {
	c := &HeuristicIntentClassifier{}
	cases := []struct {
		task string
		want Intent
	}{
		{"帮我修改 README 并提交推送", IntentMutateData},
		{"删除这个文件", IntentMutateData},
		{"更新一下文档", IntentMutateData},
		{"这个项目有什么内容，都是谁提交的", IntentQueryKnowledge},
		{"要多少次更新", IntentQueryKnowledge},
		{"介绍一下 Go 和 JS 的区别", IntentQueryKnowledge},
		{"我要退款，订单号是 8842", IntentService},
		{"这个订单能不能优惠", IntentService},
		{"你好", IntentChitChat},
		{"随便聊聊", IntentChitChat},
		{"今天天气怎么样", IntentUnknown},
	}
	for _, tc := range cases {
		got := c.Classify(tc.task)
		if got.Intent != tc.want {
			t.Errorf("Classify(%q) = %s，期望 %s（命中: %v）", tc.task, got.Intent, tc.want, got.Matched)
		}
	}
}

func TestHeuristicIntentClassifierConfidence(t *testing.T) {
	c := &HeuristicIntentClassifier{}
	// 单意图命中：置信度应为 1（该意图命中数 = 总命中数）
	got := c.Classify("帮我修改 README")
	if got.Intent != IntentMutateData || got.Confidence != 1.0 {
		t.Errorf("单意图命中置信度应为 1，实际 intent=%s conf=%v", got.Intent, got.Confidence)
	}
	// 无命中：unknown + 置信度 0
	got = c.Classify("今天天气怎么样")
	if got.Intent != IntentUnknown || got.Confidence != 0.0 {
		t.Errorf("无命中应为 unknown + 置信度 0，实际 %s %v", got.Intent, got.Confidence)
	}
	// 混合命中：改 + 查同时命中 → 取加权最高分意图，置信度 = 加权分占比（< 1）
	got = c.Classify("修改配置并查看多少内容")
	if got.Intent != IntentQueryKnowledge {
		t.Errorf("混合命中应取加权最高分（多少×2+内容+看 > 修改×2），实际 %s", got.Intent)
	}
	if got.Confidence <= 0 || got.Confidence >= 1 {
		t.Errorf("混合命中置信度应在 (0,1)，实际 %v", got.Confidence)
	}
}

func TestHeuristicIntentSplitter(t *testing.T) {
	cases := []struct {
		task string
		want int
	}{
		{"这个项目有什么内容、要多少次更新、都是谁提交的", 3},
		{"帮我改一下 README，另外这个项目是做什么的", 2},
		{"退款，以及查一下订单状态", 2},
		{"你好", 1},          // 无分隔符 → 整句
		{"", 0},              // 空输入 → 无子句
		{"，，，，", 0},         // 全标点 → 无有效子句
	}
	for _, tc := range cases {
		got := HeuristicIntentSplitter(tc.task)
		if len(got) != tc.want {
			t.Errorf("Splitter(%q) 子句数 = %d，期望 %d（got: %v）", tc.task, len(got), tc.want, got)
		}
	}
}

func TestRouteIntent(t *testing.T) {
	cases := []struct {
		it   Intent
		want string
	}{
		{IntentQueryKnowledge, "retrieve"},
		{IntentMutateData, "approval"},
		{IntentService, "service-flow"},
		{IntentChitChat, "direct-reply"},
		{IntentUnknown, "fallback"},
	}
	for _, tc := range cases {
		if got := RouteIntent(tc.it); got != tc.want {
			t.Errorf("RouteIntent(%s) = %s，期望 %s", tc.it, got, tc.want)
		}
	}
}

func TestSplitAndClassify(t *testing.T) {
	c := &HeuristicIntentClassifier{}
	// PLAN 示例：三路并行查询（README 读取 + 提交计数 + 作者列表）
	res := SplitAndClassify("这个项目有什么内容、要多少次更新、都是谁提交的", c)
	if len(res) != 3 {
		t.Fatalf("应拆出 3 条意图，实际 %d", len(res))
	}
	for i, r := range res {
		if r.Intent != IntentQueryKnowledge {
			t.Errorf("第 %d 条应为 query-knowledge，实际 %s", i+1, r.Intent)
		}
		if r.Route != "retrieve" {
			t.Errorf("第 %d 条路由应为 retrieve，实际 %s", i+1, r.Route)
		}
	}
	// 混合意图：改数据走审批链、查知识走检索链
	res = SplitAndClassify("帮我改一下 README，另外这个项目是做什么的", c)
	if len(res) != 2 {
		t.Fatalf("应拆出 2 条意图，实际 %d", len(res))
	}
	if res[0].Intent != IntentMutateData || res[0].Route != "approval" {
		t.Errorf("第 1 条应为 mutate-data/approval，实际 %s/%s", res[0].Intent, res[0].Route)
	}
	if res[1].Intent != IntentQueryKnowledge || res[1].Route != "retrieve" {
		t.Errorf("第 2 条应为 query-knowledge/retrieve，实际 %s/%s", res[1].Intent, res[1].Route)
	}
}

func TestSplitAndClassifyEmpty(t *testing.T) {
	c := &HeuristicIntentClassifier{}
	// 空输入：拆分器返回空 → 组合回退为整句分类（unknown）
	res := SplitAndClassify("", c)
	if len(res) != 1 {
		t.Fatalf("空输入应回退为 1 条整句，实际 %d", len(res))
	}
	if res[0].Intent != IntentUnknown {
		t.Errorf("空输入分类应为 unknown，实际 %s", res[0].Intent)
	}
	_ = strings.Builder{}
}
