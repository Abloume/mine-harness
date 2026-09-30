package agent

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// 审批点（Approval Point / human-in-the-loop，HITL）：
//
// 对应生产 harness 的"人工确认"机制（微软 Agent Framework 的 middleware
// approval、LangGraph 的 interrupt）：高风险动作（删文件、发消息、花钱、
// 改配置）在执行前暂停，交给人或策略决定放行/拒绝。
//
// 关键设计决策：
//   - 决策器可插拔：Approver 接口 + ApproverFunc 函数适配器，测试用
//     Auto/Deny，真实交互用 CLIApprover；
//   - fail-closed（默认拒绝）：工具标记了 RequiresApproval 但 Agent 没配
//     approver（或拒绝）→ 不执行。安全系统宁可误杀，不可放行；
//   - 拒绝不终止：拒绝结果以 tool 角色消息回填给模型，模型看到"未授权"
//     后换路径；若模型反复请求同一动作，循环检测（LoopGuard）会兜底中止。

// Approver 决定一个高风险工具调用是否放行。返回 true = 放行，false = 拒绝。
//
// JS/TS ↔ Go 差异：TS 的单方法抽象常用接口或函数参数皆可；Go 里 interface
// 是"方法集"约定，配合函数适配器（下面的 ApproverFunc）可以把普通函数直接
// 当接口用——和标准库 http.HandlerFunc 是同一个套路。
type Approver interface {
	Approve(toolName, args string) bool
}

// ApproverFunc 是函数适配器：让普通函数满足 Approver 接口。
type ApproverFunc func(toolName, args string) bool

func (f ApproverFunc) Approve(toolName, args string) bool { return f(toolName, args) }

// AutoApprover 全放行：信任场景 / 测试放行路径用。
type AutoApprover struct{}

func (AutoApprover) Approve(string, string) bool { return true }

// DenyApprover 全拒绝：最安全 / 测试拒绝路径用。
type DenyApprover struct{}

func (DenyApprover) Approve(string, string) bool { return false }

// CLIApprover 人工审批：读 stdin，输入 y/yes 放行，其他一律拒绝。
// 这是最真实的 human-in-the-loop，但会阻塞等待输入，适合交互式 CLI，
// 不适合自动化测试（测试用 Auto/Deny/ApproverFunc）。
type CLIApprover struct {
	in *bufio.Scanner
}

// NewCLIApprover 构造读标准输入的人工审批器。
func NewCLIApprover() *CLIApprover {
	return &CLIApprover{in: bufio.NewScanner(os.Stdin)}
}

// Approve 实现接口：提示用户确认，y/yes 放行，其余（含读输入出错）拒绝。
//
// JS/TS ↔ Go 差异：这里"出错当拒绝"就是 fail-closed 的落地——错误路径
// 必须朝安全侧收敛，而不是朝可用侧收敛。
func (c *CLIApprover) Approve(toolName, args string) bool {
	fmt.Printf("[审批] 高风险工具 %s(%s) 需要确认：输入 y 放行，其他拒绝 > ", toolName, args)
	if !c.in.Scan() {
		return false // 读不到输入（EOF/错误）→ 拒绝
	}
	ans := strings.ToLower(strings.TrimSpace(c.in.Text()))
	return ans == "y" || ans == "yes"
}
