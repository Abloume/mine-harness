package agent

import (
	"fmt"
	"hash/fnv"
	"time"
)

// 协作式取消契约（缺口 D · 决策依据 reflections/2026-10-13-cooperative-cancel.md）。
//
// 核心原则（来自外部实践"Agent 执行一半被用户取消，已做过的事怎么办"）：
//  1. 取消不是强杀命令，而是受控状态转换：外部信号 → cancel_requested →
//     任务在下一个安全检查点停止，不领取新步骤；
//  2. 安全检查点放在步骤边界：调用工具前查一次、工具返回后记录结果再查一次、
//     长循环按批次查——最多多完成一个可控单元，不把状态截断在任意一行代码
//     （硬停可能停在"请求已发出、本地还没记账"的缝隙里）；
//  3. 副作用先分类（只读/可补偿/不可逆）再补偿：取消后按执行日志逆序调用
//     补偿动作，不是让模型临时猜该撤什么；补偿不是事务回滚，本身可能失败，
//     失败如实记录并交还人工（needs_review）；
//  4. 不可逆动作执行前解决：执行前二次检查取消状态；执行后立即写步骤账本
//     （请求标识/结果/完成时间/可用补偿方式）；
//  5. 状态如实：不可逆步骤已提交 → partially_completed / needs_review，
//     不把"进程已停"冒充"外部世界已恢复"。

// 取消相关最终状态（RunResult.Status 取值，与 StatusCompleted/StatusAborted 并列）。
const (
	StatusCanceled      = "canceled"             // 用户取消：无可逆残留（或已全部补偿）
	StatusPartial       = "partially_completed"  // 有不可逆步骤已执行并提交，后续步骤停止
	StatusNeedsReview   = "needs_review"         // 补偿失败/未声明，需要人工接管收尾
)

// StepRecord 是步骤账本的一条记录：一次已执行工具调用的完整信息。
// 取消收尾、审计与第 8 步轨迹导出都读它——"已发生动作有记录"是验收前提。
type StepRecord struct {
	Step          int        // 主循环步号
	Name          string     // 工具名
	InputHash     string     // 请求标识（幂等键：name+input 的哈希；真实实现由工具侧生成外部单号）
	Input         string     // 原始入参（审计用，完整保留）
	SideEffect    SideEffect // 副作用类型
	OK            bool       // 执行是否成功
	Compensated   bool       // 是否已逆序补偿
	CompensateErr string     // 补偿失败原因（非空 = 该步骤无法自动收尾）
	At            string     // 完成时间（记账时刻，RFC3339；"本地落账"证据）
}

// CompensateRecord 是一次补偿执行的记录（取消收尾产生）。
type CompensateRecord struct {
	Step      int
	Name      string
	InputHash string
	OK        bool
	Err       string
	At        string
}

// RunResult 是 Run 的完整结果。
// 缺口 D 扩展：Ledger 暴露步骤账本、Compensations 暴露补偿执行记录——
// 取消后调用方可以展示"停在哪里、留下了什么、谁负责收尾"。
// Status 新增取消语义：canceled / partially_completed / needs_review。
type RunResult struct {
	Answer        string             // 最终回答（仅在 completed 时有意义）
	Steps         int
	Status        string             // StatusCompleted / StatusAborted / 取消三态
	Reason        string             // 中止/取消原因（护栏触发 or 取消点描述）
	Ledger        []StepRecord       // 步骤账本（缺口 D；含已执行工具调用的完整记录）
	Compensations []CompensateRecord // 补偿执行记录（缺口 D；取消收尾产生）
	Trace         []TraceEvent       // 结构化轨迹（增量 A；llm/tool/approval/skill/compact/cancel 事件流）
}

// checkCancel 检测外部取消信号（安全检查点共用判定）。
// 幂等：ctx.Done() 首次关闭时置位 cancelRequested，之后恒为 true——多次
// 检查不重复消费。与 JS 的 AbortController 对照：AbortSignal 是事件式
// （监听 abort 事件），Go 的 context 是"轮询 + select 阻塞"两用；
// Agent 主循环用轮询检查点，不需要事件注册。
func (a *Agent) checkCancel() bool {
	if !a.cancelRequested {
		select {
		case <-a.ctx.Done():
			a.cancelRequested = true
		default:
		}
	}
	return a.cancelRequested
}

// finishCancel 是取消收尾：逆序补偿可补偿步骤 → 状态如实分级 → 返回终态。
// 不再继续主循环；调用方拿到账本/补偿记录，可展示"停在哪、留下什么"。
func (a *Agent) finishCancel(step int) RunResult {
	a.compensateReverse()

	hasIrreversible := false // 有不可逆步骤已执行（外部世界已变化）
	hasCompFail := false     // 有补偿失败/无法自动收尾
	for _, r := range a.ledger {
		if r.SideEffect == SideIrreversible && r.OK {
			hasIrreversible = true
		}
		if r.SideEffect == SideCompensatable && r.OK && !r.Compensated {
			hasCompFail = true
		}
	}
	for _, c := range a.compensations {
		if !c.OK {
			hasCompFail = true
		}
	}

	status := StatusCanceled
	if hasCompFail {
		status = StatusNeedsReview // 补偿收不干净：交还人工，优先级高于 partial
	} else if hasIrreversible {
		status = StatusPartial // 不可逆已提交：如实告知"后续已停，但外部动作已发生"
	}

	// 增量 A：取消收尾进轨迹（Detail 含终态与收尾统计）——
	// 监测清单"取消与补偿"闭环：canceled/partially_completed/needs_review
	// 是调用方展示与人工接管的依据。
	a.appendTrace(TraceEvent{Type: TraceCancel, Step: step, Outcome: status,
		Detail: fmt.Sprintf("用户取消：已执行 %d 个工具调用，%d 个已补偿（终态 %s）",
			len(a.ledger), len(a.compensations), status)})

	return RunResult{
		Steps:         step,
		Status:        status,
		Reason:        fmt.Sprintf("用户取消：任务在第 %d 步安全检查点停止（已执行 %d 个工具调用，其中 %d 个已补偿）",
			step, len(a.ledger), len(a.compensations)),
		Ledger:        a.ledger,
		Compensations: a.compensations,
		Trace:         a.trace,
	}
}

// compensateReverse 按执行日志逆序调用可补偿步骤的补偿动作。
// 逆序理由：后发生的副作用通常依赖先发生的（先占库存后创建草稿，
// 撤销时应先删草稿再放库存）——补偿是"撤销栈"，不是模型临时猜。
// 补偿执行 1 次尝试：失败如实记录（尝试次数/错误），状态 needs_review。
func (a *Agent) compensateReverse() {
	for i := len(a.ledger) - 1; i >= 0; i-- {
		rec := &a.ledger[i]
		if rec.SideEffect != SideCompensatable || !rec.OK || rec.Compensated {
			continue
		}
		comp, ok := a.registry.CompensateOf(rec.Name)
		if !ok || comp.Exec == nil {
			// 声明了可补偿却没给补偿动作 → 无法自动收尾，如实记录
			rec.CompensateErr = "可补偿步骤未声明补偿动作，无法自动补偿，需要人工接管"
			a.compensations = append(a.compensations, CompensateRecord{
				Step: rec.Step, Name: rec.Name, InputHash: rec.InputHash,
				OK: false, Err: rec.CompensateErr, At: time.Now().Format(time.RFC3339),
			})
			continue
		}
		_, err := comp.Exec(rec.Input)
		if err != nil {
			rec.CompensateErr = fmt.Sprintf("补偿失败（已尝试 1 次）: %v", err)
			a.compensations = append(a.compensations, CompensateRecord{
				Step: rec.Step, Name: rec.Name, InputHash: rec.InputHash,
				OK: false, Err: rec.CompensateErr, At: time.Now().Format(time.RFC3339),
			})
			continue
		}
		rec.Compensated = true
		a.compensations = append(a.compensations, CompensateRecord{
			Step: rec.Step, Name: rec.Name, InputHash: rec.InputHash,
			OK: true, At: time.Now().Format(time.RFC3339),
		})
	}
}

// recordStep 在工具执行后立即记账（闭合"外部成功后本地落账前"的缝隙：
// 取消信号即使刚到达，这一步的结果也已经落账，收尾时不会遗漏）。
// InputHash = 请求标识（幂等键雏形）：同工具同参数的调用可识别、可去重；
// 真实实现由工具侧生成外部单号（超时后用同一键查结果而非重复创建）。
func (a *Agent) recordStep(step int, name, input string, ok bool) StepRecord {
	rec := StepRecord{
		Step:       step,
		Name:       name,
		InputHash:  shortHash(name + "|" + input),
		Input:      input,
		SideEffect: a.registry.SideEffectOf(name),
		OK:         ok,
		At:         time.Now().Format(time.RFC3339),
	}
	a.ledger = append(a.ledger, rec)
	return rec
}

// shortHash 生成请求标识（FNV-1a 32 位短哈希，hex 8 位）。
// 用途是账本去重/幂等键，不是安全哈希——无需抗碰撞。
func shortHash(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
