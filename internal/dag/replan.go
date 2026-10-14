package dag

import (
	"errors"
	"fmt"
)

// ReplanReason 记录为什么触发了重规划。
type ReplanReason struct {
	FailedNodeID string // 哪个节点失败了
	Err          error  // 失败原因
	Message      string // 人类可读的说明
}

// ReplanTrigger 判断一个失败节点是否需要触发 Replan。
//
// 这是一个接口——调用方可以自己定义"什么算永久失败"。
// 默认实现是：找不到工具 / 参数错误 / 权限不足 算永久失败，
// 网络超时 / 临时错误 不算。
//
// JS/TS ↔ Go 差异：TS 常用回调函数（(node, err) => boolean）；
// Go 用接口，更正式但代码量多一点。
type ReplanTrigger interface {
	ShouldReplan(node *Node, err error) bool
}

// DefaultReplanTrigger 是默认的重规划触发器。
//
// 判断逻辑：
//   - 找不到工具（ErrNodeNotFound）→ 永久失败，需要 Replan
//   - 其他错误（网络超时等）→ 临时失败，重试就行，不需要 Replan
type DefaultReplanTrigger struct{}

// ShouldReplan 实现 ReplanTrigger 接口。
func (DefaultReplanTrigger) ShouldReplan(node *Node, err error) bool {
	// 找不到工具 = 永久失败，计划本身有问题
	// 用 errors.Is 是因为错误可能被 fmt.Errorf("%w: ...") 包装过
	if errors.Is(err, ErrNodeNotFound) {
		return true
	}
	// 其他错误先假设是临时的，不触发 Replan
	return false
}

// SubgraphResult 是局部重规划后产出的新子图。
//
// LockedNodes：已成功的节点 id 列表——这些不许动。
// NewNodes：重新规划出来的新节点列表（替换失败节点 + 它的后继）。
// NewEdges：新的依赖边。
type SubgraphResult struct {
	LockedNodes []string // 已成功、不许动的节点
	NewNodes    []Node   // 新的子图节点
	NewEdges    []Edge   // 新的子图边
}

// Replanner 负责局部重规划。
//
// 它的职责：
//  1. 摘出失败节点 + 它的所有后继（受影响的子图）
//  2. 带上原始任务 + 已完成结果 + 失败原因
//  3. 让 Planner 重新规划这一段
//  4. 返回新的子图（替换掉旧的失败子图）
//
// 注意：这里没有真的调 LLM——它只是把"需要重规划什么"整理出来。
// 真正调 LLM 的是调用方（Planner.Plan）。
type Replanner struct {
	trigger ReplanTrigger
	maxRetries int // Replan 次数上限
}

// NewReplanner 创建一个 Replanner。
func NewReplanner(trigger ReplanTrigger, maxRetries int) *Replanner {
	if maxRetries <= 0 {
		maxRetries = 3 // 默认上限 3 次
	}
	return &Replanner{
		trigger:    trigger,
		maxRetries: maxRetries,
	}
}

// ExtractSubgraph 摘出失败节点 + 它的所有后继节点。
//
// 这是 Replan 的第一步：确定哪些节点是"受影响的"，
// 也就是需要被替换的。
//
// 算法：BFS 从失败节点出发，沿着 edges 往下走，收集所有后继。
func (g *Graph) ExtractSubgraph(failedNodeID string) ([]string, error) {
	// 先确认节点存在
	found := false
	for _, n := range g.Nodes {
		if n.ID == failedNodeID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("节点 %q 不存在", failedNodeID)
	}

	// 邻接表：from → 所有 to
	adj := make(map[string][]string)
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	// BFS 收集所有后继
	visited := make(map[string]bool)
	queue := []string{failedNodeID}
	visited[failedNodeID] = true

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, next := range adj[current] {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}

	// 转成 slice
	var result []string
	for id := range visited {
		result = append(result, id)
	}
	return result, nil
}

// LockedNodes 返回所有状态为 success 的节点 id——这些不许动。
func (g *Graph) LockedNodes() []string {
	var locked []string
	for _, n := range g.Nodes {
		if n.Status == StatusSuccess {
			locked = append(locked, n.ID)
		}
	}
	return locked
}

// MergeSubgraph 把新的子图合并回原 DAG。
//
// 步骤：
//  1. 删除旧的失败子图节点（失败节点 + 它的后继）
//  2. 添加新的子图节点
//  3. 补边：新子图的入口节点要跟 locked 节点连上
//
// 注意：这是简化版——真实系统需要更复杂的边重连逻辑。
// 这里只展示核心思路：删旧的，加新的。
func (g *Graph) MergeSubgraph(removeIDs []string, newNodes []Node, newEdges []Edge) {
	// 1. 删掉要移除的节点
	removeSet := make(map[string]bool)
	for _, id := range removeIDs {
		removeSet[id] = true
	}

	var keptNodes []Node
	for _, n := range g.Nodes {
		if !removeSet[n.ID] {
			keptNodes = append(keptNodes, n)
		}
	}
	g.Nodes = append(keptNodes, newNodes...)

	// 2. 删掉涉及被移除节点的边
	var keptEdges []Edge
	for _, e := range g.Edges {
		if !removeSet[e.From] && !removeSet[e.To] {
			keptEdges = append(keptEdges, e)
		}
	}
	g.Edges = append(keptEdges, newEdges...)
}

// CheckReplan 检查是否需要触发 Replan。
//
// 返回值：
//   - needReplan：是否需要重规划
//   - reason：为什么需要（或不需要）
func (r *Replanner) CheckReplan(node *Node, err error) (needReplan bool, reason string) {
	if r.trigger.ShouldReplan(node, err) {
		return true, fmt.Sprintf("节点 %s 失败（%v），触发重规划", node.ID, err)
	}
	return false, fmt.Sprintf("节点 %s 失败（%v），判定为临时错误，不触发重规划", node.ID, err)
}

// MaxRetries 返回 Replan 次数上限。
func (r *Replanner) MaxRetries() int {
	return r.maxRetries
}
