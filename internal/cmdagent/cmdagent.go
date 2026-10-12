// Package cmdagent 提供命令执行工具（PLAN 第 2 步：命令执行）。
//
// 与 fsagent 同模式：独立包、只依赖 internal/agent 的 Tool 类型，
// 机制归内核、工具归应用层。第 2 步只做一个 run_cmd 工具——
// 对应 Claude Code 的 Bash / Codex 的 shell：一个命令工具 + 参数感知风险
// 判定（同工具不同命令风险天差地别：go test 无害，rm -rf 致命）。
//
// JS/TS ↔ Go 差异总览（详见各函数注释）：
//   - JS child_process.exec 是异步回调/Promise；Go 的 os/exec 同步等待 + 显式 error
//   - JS 超时靠 AbortController 手写；Go 有 context.WithTimeout 语言级取消
//   - 命令退出码（exit code）≠ Go error：命令失败是"业务结果"（模型要看到
//     失败输出继续处理），只有执行层面失败（不存在/超时/无法启动）才是 error
package cmdagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"mine-harness/internal/agent"
)

// 默认值：命令超时 60s（防挂死命令卡住主循环）；输出截断 8000 字符
// （保留头尾，防长输出撑爆上下文——与内核压缩机制是同一动机）。
const (
	defaultTimeout    = 60 * time.Second
	defaultMaxOutput  = 8000
	defaultTailLength = 4000
)

// Tools 是命令执行工具的宿主，绑定项目根目录（命令在此目录下运行）。
type Tools struct {
	root string
}

// NewTools 构造绑定指定根目录的命令工具。
func NewTools(root string) *Tools { return &Tools{root: root} }

// All 返回全部工具定义。
//
// 风险：run_cmd 声明 RiskMedium（执行命令有副作用），再由宿主配
// FuncRiskEvaluator(cmdagent.RiskOf) 做参数感知修正——白名单命令（构建/
// 测试/只读 git）降 RiskLow 自动放行，危险命令（rm/git push/curl 等）
// 升 RiskHigh 强制审批，其余保持 Medium。
func (t *Tools) All() []agent.Tool {
	return []agent.Tool{
		{
			Name: "run_cmd",
			Description: "在项目根目录执行命令，返回退出码 + 输出（超时默认60s、输出自动截断头尾）。" +
				"参数是 JSON：{\"cmd\":\"go test ./...\",\"timeout\":可选秒数,\"max_output\":可选截断长度}。" +
				"只读/构建/测试类命令（go build/test/vet、git status/diff/log、ls、cat 等）自动放行；" +
				"危险命令（rm、git push/commit/reset、curl、sudo 等）需审批。命令按空格拆分，不支持引号/管道/重定向。",
			BaseRisk: agent.RiskMedium,
			Execute:  t.runCmd,
		},
	}
}

// runReq 是 run_cmd 的输入。
type runReq struct {
	Cmd       string `json:"cmd"`
	Timeout   int    `json:"timeout,omitempty"`    // 秒，0 = 默认 60s
	MaxOutput int    `json:"max_output,omitempty"` // 字符，0 = 默认 8000
}

func (t *Tools) runCmd(input string) (string, error) {
	var req runReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"cmd\":\"...\"}）", err)
	}
	if strings.TrimSpace(req.Cmd) == "" {
		return "", fmt.Errorf("cmd 不能为空")
	}

	timeout := defaultTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	maxOut := defaultMaxOutput
	if req.MaxOutput > 0 {
		maxOut = req.MaxOutput
	}

	// 命令拆分：strings.Fields 按空白拆（简单实现）。
	// 局限：不带引号/管道/重定向语义（"echo 'a b'" 会拆成 echo、'a、b'）。
	// 教学点：真实 shell 的解析远比空格拆分复杂；生产要么让模型传命令数组，
	// 要么用完整的 shell 解析库。拆数组后 exec 直接执行（无 shell 介入）——
	// 这同时免疫了 shell 注入（参数里的 ;、|、$() 都只是普通字符，不会被执行）。
	parts := strings.Fields(req.Cmd)
	if len(parts) == 0 {
		return "", fmt.Errorf("cmd 不能为空")
	}

	// 超时取消（vs JS 手写 AbortController + Promise.race）：
	// context.WithTimeout 到点后触发 cmd.Cancel。这里有两个 Go 特有的坑，都解决掉：
	//
	// 坑 1（实测踩坑）：直接 exec.CommandContext 默认 Cancel = 只杀"直接子进程"。
	// 若命令是 sh 脚本（bash 会 fork 孙进程 sleep），bash 被 SIGKILL 立即死，但
	// sleep 还攥着 stdout 管道写端——exec.Cmd 的 Wait() 要等管道 EOF 才返回，
	// 于是空等到 sleep 自己结束。表现为"超时了却多等了整个命令时长"。
	// 解法：Setpgid 让命令自成进程组，Cancel 时 kill(-pid) 连孙进程一起杀。
	//
	// 坑 2：kill 与 Wait 之间的竞态。解法：WaitDelay 给 Wait 一个宽限，
	// 超时后强制收尾（Go 1.20+）。这是 JS child_process 没有的字段——
	// 对应 JS 里"kill 后进程还在输出"的手动处理。
	//
	// 注意 Setpgid 是 POSIX 专用（macOS/Linux）；Windows 用 taskkill /T 杀进程树。
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = t.root // 命令在项目根运行
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// 默认 Cancel 只杀直接子进程；这里杀整个进程组（负 pid = 组）
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	// 输出收集：先 bytes.Buffer 整体收集（简单），生产对海量输出用
	// StdoutPipe + 流式扫描（内存 O(1)）；下面的截断是兜底防爆上下文。
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()

	// 超时判定优先：ctx 到点 → 明确报"已终止"，不让模型以为命令还在跑
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("命令超时（%v）已终止：%s", timeout, req.Cmd)
	}

	stdout := truncateKeepEdges(outBuf.String(), maxOut)
	stderr := truncateKeepEdges(errBuf.String(), maxOut/4) // stderr 是辅助信息，给更小配额

	// 关键教学点：命令的非零退出码不是 Go error。
	// JS/TS 里 child_process 的 exit code 和 throw 是两回事，Go 同理——
	// exit code 是"业务结果"（测试失败、编译报错都要回填给模型继续处理）；
	// 只有 runErr 且非"退出码错误"（如命令不存在、无法启动）才走 error 分支。
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); ok {
			// 命令跑了但退出码非 0：正常回填，模型需要看到失败输出
			return fmt.Sprintf("exit: %d\n--- stdout ---\n%s\n--- stderr ---\n%s",
				cmd.ProcessState.ExitCode(), stdout, stderr), nil
		}
		return "", fmt.Errorf("命令执行失败: %v（命令不存在或无权限？）", runErr)
	}
	return fmt.Sprintf("exit: 0\n--- stdout ---\n%s\n--- stderr ---\n%s", stdout, stderr), nil
}

// truncateKeepEdges 截断长文本：保留头尾、中间省略。
// 防爆上下文的关键：模型需要"命令开头 + 结尾"（报错通常在结尾），
// 中间细节可丢；同时显式标注截断，模型知道自己没看到全文。
func truncateKeepEdges(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max / 2
	tail := max - head
	return s[:head] + fmt.Sprintf("\n...[已截断 %d 字符，保留头 %d / 尾 %d]...\n", len(s)-max, head, tail) + s[len(s)-tail:]
}

// RiskOfArgs 是宿主接入 FuncRiskEvaluator 的入口：参数是工具输入 JSON
// （{"cmd":"..."}），先解包取命令再判定。
// 教学点：工具参数永远是 JSON 字符串（内核不感知工具内部结构），
// 风险判定必须自己先 Unmarshal——否则会把 `{"cmd":"go` 当成命令名。
// 解析失败 fail-closed 返回 RiskMedium（未知即不信任）。
func RiskOfArgs(args string) agent.RiskLevel {
	var req runReq
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return agent.RiskMedium
	}
	return RiskOf(req.Cmd)
}

// RiskOf 是 run_cmd 的参数感知风险判定（宿主用 FuncRiskEvaluator 接入）。
// 入参是命令字符串本身；工具参数 JSON 场景用 RiskOfArgs。
//
// 判定规则（简单启发式，教学够用；生产可用分类器/模型）：
//   - 命令名命中白名单（构建/测试/只读）→ RiskLow：自动放行不打扰
//   - 命令文本命中危险模式 → RiskHigh：强制审批
//   - 其余 → RiskMedium：默认走审批
//
// 注意：数组执行模式已免疫 shell 注入，这里的危险词检测是第二道防线
// （防"模型把危险命令写成低风险样貌"的误判，也防未来切换 shell 模式）。
func RiskOf(cmd string) agent.RiskLevel {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return agent.RiskMedium
	}
	name := parts[0]

	// 白名单：无副作用或副作用只在项目内的命令
	readOnly := map[string]bool{
		"go":  true, // go build/test/vet/fmt/list/version 全部放行
		"git": true, // 注意：git 不全是只读！push/commit/reset 等危险词会升 High
		"ls":  true, "cat": true, "pwd": true, "echo": true,
		"grep": true, "find": true, "head": true, "tail": true,
		"wc": true, "which": true, "date": true, "true": true,
	}
	if readOnly[name] {
		// 白名单内仍有危险子命令（git push / go get 写环境）——先查危险模式
		if dangerousPattern(cmd) {
			return agent.RiskHigh
		}
		return agent.RiskLow
	}

	if dangerousPattern(cmd) {
		return agent.RiskHigh
	}
	return agent.RiskMedium
}

// dangerousPattern 检测不可逆/越权/外联类危险模式。
// 启发式子串匹配（大小写不敏感）：生产可换成白名单 allowlist 更严格。
func dangerousPattern(cmd string) bool {
	low := strings.ToLower(cmd)
	danger := []string{
		"rm ", "rm -rf", "mv ", "dd ", "mkfs", "shutdown", "reboot",
		"sudo ", "chmod", "chown", "kill ", "pkill",
		"curl ", "wget ", "nc ", "telnet ",
		"git push", "git commit", "git reset", "git clean", "git checkout -f",
		"git rebase", "git merge", "git cherry-pick", "git stash drop",
		"go get", "go install",
		">", ">>", "| sh", "| bash", "eval ", ":()",
	}
	for _, d := range danger {
		if strings.Contains(low, d) {
			return true
		}
	}
	return false
}
