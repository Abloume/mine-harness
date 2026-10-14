package agent

import (
	"fmt"
	"strings"
	"sync"
)

// 增量 H · 任务系统 + 依赖图（Task Graph）。
//
// 定位：单次 Run 只覆盖"一次任务"，任务系统让目标持久化到单次对话之外——
// 任务可分解（父任务拆子任务）、有依赖图（先 A 后 B）、可并行追踪。
//
// 与 Run 的分工：
//   - Run 是"一次执行"（一次 agent loop 的生命周期，task 参数即本次目标）；
//   - Task 是"一个目标"（可跨多次 Run 存在，父子分解 + 依赖门控 + 完成态传播）。
//   - 机制归内核（注册 / 依赖 / 状态机 / 环路防护 / 传播），宿主决定任务来源与领取顺序。
//
// JS/TS ↔ Go 差异：TS 常用 class 或 Map<string, Node> 手写图结构；
// Go 用 struct + map 邻接表（Depends 正向边、Children 反向索引），
// 并配合 sync.Mutex 应对并行领取（JS 单线程没有数据竞争问题）。
//
// 状态机：pending → running → done / blocked
//   - blocked 是"依赖未就绪"的门控结果（查询派生：pending 且依赖未全 done）；
//   - done 触发完成态传播：父任务的所有子任务全 done → 父任务自动 done（递归向上）。

// Task 状态取值。
const (
	TaskPending = "pending" // 待执行：依赖未就绪或未领取
	TaskRunning = "running" // 执行中（已被某次 Run 领取）
	TaskDone    = "done"    // 完成
	TaskBlocked = "blocked" // 依赖未就绪，被门控（查询派生，也可显式标记）
)

// Task 是一个可分解、带依赖的任务节点。
type Task struct {
	ID       string   // 任务 ID（图中唯一）
	Goal     string   // 任务目标（人类可读）
	Status   string   // 状态机：pending → running → done / blocked
	Parent   string   // 父任务 ID（"" = 根任务；完成态向上传播）
	Depends  []string // 前置依赖：本任务依赖它们全 done 才可执行
	Children []string // 反向索引：父任务的直接子任务列表（注册时自动构建）
}

// TaskGraph 是任务注册表 + 依赖图。
// 线程安全：Schedulable / MarkDone 会被并行领取方同时调用，全部方法加锁。
type TaskGraph struct {
	mu       sync.Mutex
	tasks    map[string]*Task
	order    []string        // 注册顺序（Dump / 观测稳定输出用；Go map 遍历无序）
	sink     func(TraceEvent) // 轨迹回调（nil = 未接入；Agent 接入时注入 appendTrace）
	buffered []TraceEvent    // sink 接入前的事件暂存：注册事件不因接入时序丢失
}

// NewTaskGraph 创建空任务图。
func NewTaskGraph() *TaskGraph {
	return &TaskGraph{tasks: make(map[string]*Task)}
}

// SetTraceSink 注入轨迹回调：状态变更时发出 TraceTask 事件（Type=TraceTask）。
// 接入前产生的事件（如注册）会缓存并在接入时回放——宿主"先注册后接入"也完整。
// 注意：回调内不得调用 TaskGraph 方法（锁不可重入）。
func (g *TaskGraph) SetTraceSink(sink func(TraceEvent)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sink = sink
	if sink != nil && len(g.buffered) > 0 {
		for _, ev := range g.buffered {
			sink(ev)
		}
		g.buffered = nil
	}
}

func (g *TaskGraph) emit(ev TraceEvent) {
	if g.sink != nil {
		g.sink(ev)
	} else {
		g.buffered = append(g.buffered, ev)
	}
}

// dependsReach 判断从 start 出发（沿 Depends + Parent），能否到达 target。
// 用于环路检测：注册新任务 / 更新依赖时，若某前置能绕回目标自身 → 环。
func (g *TaskGraph) dependsReach(start, target string) bool {
	// JS/TS ↔ Go 差异：递归闭包需先声明 var 再赋值（Go 要求显式类型）；
	// TS 里 const visit = ... 直接自引用即可。
	var visit func(cur string, seen map[string]bool) bool
	visit = func(cur string, seen map[string]bool) bool {
		if cur == target {
			return true
		}
		if seen[cur] {
			return false
		}
		seen[cur] = true
		t := g.tasks[cur]
		if t == nil {
			return false
		}
		for _, d := range t.Depends {
			if visit(d, seen) {
				return true
			}
		}
		if t.Parent != "" && visit(t.Parent, seen) {
			return true
		}
		return false
	}
	return visit(start, map[string]bool{})
}

// Register 注册一个任务并做三类校验：
//   - ID 唯一；
//   - 父任务、每个依赖任务必须已注册（注册顺序 = 先父后子、先依赖后依赖者）；
//   - 环路检测：沿"前置链"（Depends + Parent）DFS，若回到本任务自身则拒绝
//     （依赖须先注册的约束已挡住"依赖自身"，此处是纵深防御——
//     真正可达的环由 UpdateDeps 动态改依赖时触发）。
//
// 返回错误时不写入任何状态（注册是原子的）。
func (g *TaskGraph) Register(id, goal, parent string, depends ...string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if id == "" {
		return fmt.Errorf("taskgraph: 任务 ID 不能为空")
	}
	if _, dup := g.tasks[id]; dup {
		return fmt.Errorf("taskgraph: 任务 %q 已注册", id)
	}
	if parent != "" {
		if _, ok := g.tasks[parent]; !ok {
			return fmt.Errorf("taskgraph: 父任务 %q 不存在（需先注册）", parent)
		}
	}
	for _, d := range depends {
		if _, ok := g.tasks[d]; !ok {
			return fmt.Errorf("taskgraph: 依赖任务 %q 不存在（需先注册）", d)
		}
		if g.dependsReach(d, id) {
			return fmt.Errorf("taskgraph: 任务 %q 与前置 %q 构成依赖环", id, d)
		}
	}
	if parent != "" && g.dependsReach(parent, id) {
		return fmt.Errorf("taskgraph: 任务 %q 与父任务 %q 构成依赖环", id, parent)
	}

	t := &Task{
		ID:      id,
		Goal:    goal,
		Status:  TaskPending,
		Parent:  parent,
		Depends: append([]string{}, depends...),
	}
	g.tasks[id] = t
	g.order = append(g.order, id)
	if parent != "" {
		g.tasks[parent].Children = append(g.tasks[parent].Children, id)
	}

	g.emit(TraceEvent{Type: TraceTask, Name: id, Outcome: "registered", Detail: "goal=" + goal})
	return nil
}

// UpdateDeps 动态更新任务的依赖列表（宿主可在运行中调整编排）。
// 环检测在此真正可达：a 依赖 b 后，把 b 的依赖改为 a → a→b→a 环 → 拒绝。
// 返回错误时不写入任何状态。
func (g *TaskGraph) UpdateDeps(id string, depends ...string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	t, ok := g.tasks[id]
	if !ok {
		return fmt.Errorf("taskgraph: 任务 %q 未注册", id)
	}
	for _, d := range depends {
		if _, ok := g.tasks[d]; !ok {
			return fmt.Errorf("taskgraph: 依赖任务 %q 不存在", d)
		}
		if d == id {
			return fmt.Errorf("taskgraph: 任务 %q 不能依赖自身", id)
		}
		if g.dependsReach(d, id) {
			return fmt.Errorf("taskgraph: 更新后任务 %q 与前置 %q 构成依赖环", id, d)
		}
	}
	t.Depends = append([]string{}, depends...)
	g.emit(TraceEvent{Type: TraceTask, Name: id, Outcome: "deps-updated", Detail: "depends=" + strings.Join(depends, ",")})
	return nil
}

// Status 查询任务状态（不存在返回 "", false）。
func (g *TaskGraph) Status(id string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tasks[id]
	if !ok {
		return "", false
	}
	return t.Status, true
}

// IsBlocked 判定任务是否被依赖门控：pending 且存在未 done 的依赖。
// blocked 是派生状态而非独立字段——依赖状态变化后，本方法结果自动更新。
func (g *TaskGraph) IsBlocked(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.isBlockedLocked(id)
}

func (g *TaskGraph) isBlockedLocked(id string) bool {
	t, ok := g.tasks[id]
	if !ok || t.Status != TaskPending {
		return false
	}
	for _, d := range t.Depends {
		if dep, ok := g.tasks[d]; !ok || dep.Status != TaskDone {
			return true
		}
	}
	return false
}

// Schedulable 返回当前可领取的任务（pending、无子任务、依赖全 done）——
// 宿主据此领取并并行执行；无依赖根任务注册即就绪。
// 排除有 Children 的任务：父任务是聚合节点（靠完成态传播完成），
// 不是可执行的工作单元，不应被宿主领取执行。
func (g *TaskGraph) Schedulable() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for id, t := range g.tasks {
		if t.Status == TaskPending && len(t.Children) == 0 && !g.isBlockedLocked(id) {
			out = append(out, id)
		}
	}
	return out
}

// MarkRunning 领取任务（pending → running）。
// 未注册或已 done/blocked 的任务忽略（幂等，宿主可无脑调用）。
func (g *TaskGraph) MarkRunning(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tasks[id]
	if !ok || t.Status != TaskPending {
		return
	}
	t.Status = TaskRunning
	g.emit(TraceEvent{Type: TraceTask, Name: id, Outcome: "running"})
}

// MarkDone 完成任务并向上传播：父任务的所有子任务全 done → 父任务自动 done（递归）。
func (g *TaskGraph) MarkDone(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.markDoneLocked(id)
}

func (g *TaskGraph) markDoneLocked(id string) {
	t, ok := g.tasks[id]
	if !ok || t.Status == TaskDone {
		return
	}
	t.Status = TaskDone
	g.emit(TraceEvent{Type: TraceTask, Name: id, Outcome: "done"})
	if t.Parent == "" {
		return
	}
	parent := g.tasks[t.Parent]
	for _, c := range parent.Children {
		if child, ok := g.tasks[c]; !ok || child.Status != TaskDone {
			return // 还有未完成的子任务，父任务保持原状态
		}
	}
	g.markDoneLocked(t.Parent) // 全部子任务完成 → 父任务完成（递归向上）
}

// MarkBlocked 显式标记依赖门控（供宿主汇报"领取被拒"时使用）。
// 一般不需要显式调用——IsBlocked / Schedulable 已按依赖派生；
// 保留该入口是为了让宿主能把 blocked 原因写进自己的记录。
func (g *TaskGraph) MarkBlocked(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tasks[id]
	if !ok || t.Status != TaskPending {
		return
	}
	t.Status = TaskBlocked
	g.emit(TraceEvent{Type: TraceTask, Name: id, Outcome: "blocked"})
}

// Dump 输出整图状态（demo / 观测用），按注册顺序稳定打印
// （Go map 遍历无序，必须用 order 切片保证输出可复现）。
func (g *TaskGraph) Dump() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b strings.Builder
	for _, id := range g.order {
		t := g.tasks[id]
		dep := strings.Join(t.Depends, ",")
		if dep == "" {
			dep = "-"
		}
		fmt.Fprintf(&b, "  %s [%s] %s（依赖: %s）\n", id, t.Status, t.Goal, dep)
	}
	return b.String()
}
