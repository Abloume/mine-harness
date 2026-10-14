// Package dag 实现 Plan-and-Execute 模式的 DAG 执行器。
//
// 设计参考：卡码笔记《Plan-and-Execute怎么落地成DAG执行器》
// 核心思路：模型输出结构化的 nodes + edges（不是文本列表），
// 执行前做环检测 + 拓扑排序，按层并行调度，节点有状态机，
// 失败只影响后继分支，支持局部 Replan。
//
// 学习定位：这是内核练习，不是生产调度系统。
// 生产用 Airflow / Argo / Temporal 等成熟工作流引擎。
//
// JS/TS ↔ Go 差异：
//   - TS 常用 class + 泛型表达图结构；Go 用 struct + slice
//   - TS 的 Promise.all 对应 Go 的 goroutine + sync.WaitGroup
//   - Go 的 interface 隐式实现（鸭子类型），不需要 implements 关键字
package dag

import (
	"errors"
	"fmt"
	"sync"
)

// NodeStatus 是节点的状态机状态。
//
// 状态流转：
//
//	pending → ready → running → success
//	                  ↓
//	                failed → skipped（后继被跳过）
//
// JS/TS ↔ Go 差异：TS 常用字符串联合类型；Go 用 int 常量 + iota，
// 内存更小但可读性差一点（需要看常量定义才知道含义）。
type NodeStatus int

const (
	StatusPending NodeStatus = iota // 前置未完成，还不能跑
	StatusReady                    // 前置都完成了，可以执行
	StatusRunning                  // 正在执行
	StatusSuccess                  // 执行成功
	StatusFailed                   // 执行失败
	StatusSkipped                  // 前置失败，被跳过
)

// String 实现 fmt.Stringer，打印时可读。
func (s NodeStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusReady:
		return "ready"
	case StatusRunning:
		return "running"
	case StatusSuccess:
		return "success"
	case StatusFailed:
		return "failed"
	case StatusSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// Node 是 DAG 里的一个节点——对应一次工具调用或一个子任务。
//
// Tool + Params：节点要执行的动作。执行器拿到 Tool 名字后，
// 从注册表找到对应的执行函数跑。
//
// Deps：前置节点 id 列表。所有 Deps 都 success 后，本节点变 ready。
//
// JS/TS ↔ Go 差异：TS 里 Params 常用 any / unknown；
// Go 里用 string（JSON 序列化后的字符串），跟 agent.Tool 的 Input 保持一致。
type Node struct {
	ID     string         `json:"id"`     // 节点唯一标识
	Action string         `json:"action"` // 人类可读的动作描述
	Tool   string         `json:"tool"`   // 要调用的工具名
	Params string         `json:"params"` // 工具参数（JSON 字符串）
	Deps   []string       `json:"deps"`   // 前置节点 id 列表

	// 运行时状态（不序列化到 JSON，执行器自己维护）
	Status NodeStatus `json:"-"`
	Output string     `json:"-"` // 执行成功后的输出
	Err    error      `json:"-"` // 执行失败的原因
}

// Edge 是一条依赖边：from 完成后 to 才能跑。
//
// Optional=true 表示软依赖——from 失败不阻塞 to，
// 但 to 的输入里要标注"缺少该分支的数据"。
type Edge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Optional bool   `json:"optional,omitempty"`
}

// Graph 是完整的 DAG：节点列表 + 依赖边。
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// ExecFunc 是工具执行函数的签名：输入是 JSON 字符串，输出是结果字符串。
//
// 跟 agent.Tool.Execute 保持一致——这样 DAG 执行器可以直接复用
// agent 包注册的工具，不需要重新写一遍。
type ExecFunc func(params string) (string, error)

// Executor 是 DAG 执行器：拿到 Graph + 工具注册表，按层并行执行。
//
// 并发模型：同一层的节点用 goroutine 并行跑，WaitGroup 等全部完成。
// 信号量控制最大并发数（避免一层 20 个节点同时打爆下游 API）。
//
// JS/TS ↔ Go 差异：TS 用 Promise.all + 并发池库；
// Go 用 goroutine + channel 做信号量，原生支持，不需要第三方库。
type Executor struct {
	tools map[string]ExecFunc // 工具注册表：name → 执行函数
	sem   chan struct{}       // 信号量：控制最大并发数
}

// NewExecutor 创建一个 DAG 执行器。
// maxConcurrent 限制同一层最多同时跑多少个节点。
func NewExecutor(maxConcurrent int) *Executor {
	if maxConcurrent <= 0 {
		maxConcurrent = 5 // 默认 5 个并发
	}
	return &Executor{
		tools: make(map[string]ExecFunc),
		sem:   make(chan struct{}, maxConcurrent),
	}
}

// Register 注册一个工具：名字 + 执行函数。
func (e *Executor) Register(name string, fn ExecFunc) {
	e.tools[name] = fn
}

// ErrCycleDetected 表示 DAG 里检测到环（不应该出现的情况）。
var ErrCycleDetected = errors.New("dag: cycle detected in graph")

// ErrNodeNotFound 表示执行时找不到某个工具。
var ErrNodeNotFound = errors.New("dag: tool not found in registry")

// Validate 校验 DAG 是否合法：
//  1. 节点 id 不重复
//  2. 边的 from/to 都存在
//  3. 没有环（DFS 三色标记法）
//
// 检测到环直接报错——不自动断边，因为断边会破坏原始意图，
// 应该让模型重新规划，而不是执行器偷偷改图。
func (g *Graph) Validate() error {
	// 1. 检查节点 id 唯一性
	idSet := make(map[string]bool)
	for _, n := range g.Nodes {
		if idSet[n.ID] {
			return fmt.Errorf("dag: duplicate node id %q", n.ID)
		}
		idSet[n.ID] = true
	}

	// 2. 检查边的端点都存在
	for _, e := range g.Edges {
		if !idSet[e.From] {
			return fmt.Errorf("dag: edge from %q not found", e.From)
		}
		if !idSet[e.To] {
			return fmt.Errorf("dag: edge to %q not found", e.To)
		}
	}

	// 3. 环检测：DFS 三色标记
	//    白色(0)=未访问，灰色(1)=在当前递归路径上，黑色(2)=已访问完
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)

	// 邻接表：from → 所有 to
	adj := make(map[string][]string)
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	var dfs func(id string) error
	dfs = func(id string) error {
		color[id] = gray
		for _, next := range adj[id] {
			switch color[next] {
			case gray:
				// 撞到灰色节点 → 成环
				return ErrCycleDetected
			case white:
				if err := dfs(next); err != nil {
					return err
				}
			}
			// black 已经访问过，直接跳过
		}
		color[id] = black
		return nil
	}

	for _, n := range g.Nodes {
		if color[n.ID] == white {
			if err := dfs(n.ID); err != nil {
				return err
			}
		}
	}

	return nil
}

// TopoSort 用 Kahn 算法做拓扑排序，返回分层结果。
//
// 返回值是一个二维数组：外层是层（第 0 层 = 第一层），
// 内层是该层所有可并行执行的节点 id。
//
// Kahn 算法步骤：
//  1. 统计每个节点的入度（多少条边指向它）
//  2. 入度为 0 的节点进队列（这是第一层）
//  3. 取出队列里的节点，删掉它的出边，邻居入度减 1
//  4. 邻居入度减到 0 就入队——这是下一层
//
// 顺带一个好处：如果最后排出来的节点数 < 总数，说明有环。
//
// JS/TS ↔ Go 差异：TS 用数组当队列（shift/push）；
// Go 用 slice + 索引指针，避免 shift 的 O(n) 开销。
func (g *Graph) TopoSort() ([][]string, error) {
	// 先校验合法性
	if err := g.Validate(); err != nil {
		return nil, err
	}

	// 入度表
	inDegree := make(map[string]int)
	for _, n := range g.Nodes {
		inDegree[n.ID] = 0
	}
	for _, e := range g.Edges {
		inDegree[e.To]++
	}

	// 邻接表：from → 所有 to
	adj := make(map[string][]string)
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	// 初始队列：所有入度为 0 的节点
	var queue []string
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	var layers [][]string
	processed := 0

	for len(queue) > 0 {
		// 当前层 = 队列里的所有节点
		currentLayer := make([]string, len(queue))
		copy(currentLayer, queue)
		layers = append(layers, currentLayer)
		processed += len(currentLayer)

		// 下一层候选
		var nextQueue []string
		for _, id := range currentLayer {
			// 删掉这个节点的所有出边，邻居入度减 1
			for _, next := range adj[id] {
				inDegree[next]--
				if inDegree[next] == 0 {
					nextQueue = append(nextQueue, next)
				}
			}
		}

		queue = nextQueue
	}

	// 处理的节点数 < 总数 → 有环（理论上 Validate 已经拦了，这里兜底）
	if processed < len(g.Nodes) {
		return nil, ErrCycleDetected
	}

	return layers, nil
}

// Run 执行整个 DAG：拓扑排序 → 按层并行 → 状态追踪。
//
// 执行流程：
//  1. 拓扑排序，分出层
//  2. 逐层执行：同一层的节点并行跑（信号量限流）
//  3. 某节点失败 → 后继标记 skipped（软依赖除外）
//  4. 全部层跑完，返回每个节点的最终状态
//
// 返回值：每个节点的最终状态快照（节点 id → 状态 + 输出 + 错误）。
func (e *Executor) Run(g *Graph) (map[string]*Node, error) {
	// 拓扑排序
	layers, err := g.TopoSort()
	if err != nil {
		return nil, fmt.Errorf("拓扑排序失败: %w", err)
	}

	// 节点表：id → 节点指针（方便按 id 查找和修改状态）
	nodeMap := make(map[string]*Node)
	for i := range g.Nodes {
		nodeMap[g.Nodes[i].ID] = &g.Nodes[i]
		// 初始状态都是 pending
		g.Nodes[i].Status = StatusPending
	}

	// 按层执行
	for layerIdx, layer := range layers {
		fmt.Printf("--- 第 %d 层（%d 个节点并行）---\n", layerIdx+1, len(layer))

		var wg sync.WaitGroup
		for _, id := range layer {
			wg.Add(1)
			go func(nodeID string) {
				defer wg.Done()

				// 信号量：拿一个"名额"才能跑
				e.sem <- struct{}{}
				defer func() { <-e.sem }()

				node := nodeMap[nodeID]

				// 检查前置状态：前置失败或被跳过，且不是软依赖，本节点直接 skipped
				skipped := false
				for _, dep := range nodeDeps(g, nodeID) {
					depStatus := nodeMap[dep].Status
					// 前置 failed 或 skipped 都算"不可用"——skipped 会级联传播
					if depStatus == StatusFailed || depStatus == StatusSkipped {
						// 检查这条边是不是软依赖
						if !isOptionalEdge(g, dep, nodeID) {
							skipped = true
							break
						}
						// 软依赖失败：不阻塞，但要标注
						fmt.Printf("  [软依赖警告] %s 的前置 %s 状态异常（%s），继续执行但数据不完整\n",
							nodeID, dep, depStatus)
					}
				}
				if skipped {
					node.Status = StatusSkipped
					fmt.Printf("  ⏭️  %s: skipped（前置失败）\n", nodeID)
					return
				}

				// 执行
				node.Status = StatusRunning
				fmt.Printf("  ▶️  %s: running（%s）\n", nodeID, node.Action)

				fn, ok := e.tools[node.Tool]
				if !ok {
					node.Status = StatusFailed
					node.Err = fmt.Errorf("%w: %s", ErrNodeNotFound, node.Tool)
					fmt.Printf("  ❌ %s: failed（找不到工具 %s）\n", nodeID, node.Tool)
					return
				}

				output, err := fn(node.Params)
				if err != nil {
					node.Status = StatusFailed
					node.Err = err
					fmt.Printf("  ❌ %s: failed（%v）\n", nodeID, err)
					return
				}

				node.Status = StatusSuccess
				node.Output = output
				fmt.Printf("  ✅ %s: success\n", nodeID)
			}(id)
		}
		wg.Wait()
	}

	return nodeMap, nil
}

// nodeDeps 查某个节点的所有前置依赖。
func nodeDeps(g *Graph, nodeID string) []string {
	var deps []string
	for _, e := range g.Edges {
		if e.To == nodeID {
			deps = append(deps, e.From)
		}
	}
	return deps
}

// isOptionalEdge 检查 from→to 这条边是不是软依赖。
func isOptionalEdge(g *Graph, from, to string) bool {
	for _, e := range g.Edges {
		if e.From == from && e.To == to {
			return e.Optional
		}
	}
	return false
}
