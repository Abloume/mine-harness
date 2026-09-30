package agent

import "fmt"

// Tool 描述一个可被模型调用的工具。
type Tool struct {
	Name        string
	Description string
	Execute     func(input string) (string, error) // 简化：JSON 字符串进、字符串出
}

// Registry 是工具注册表：模型只能调用已注册的工具——这本身就是一层护栏
// （对应生产 harness 中"工具白名单 + 权限控制"的最小雏形）。
//
// JS/TS ↔ Go 差异：TS 常用 Map<string, Tool>；Go 用 map[string]Tool 语法近似。
// 但 Go 的 map 读不存在的 key 返回零值（不报错），必须查 ok 才知道是否存在，
// 这一点和 TS 的 Map.has() 语义对应。
type Registry struct {
	tools map[string]Tool
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册一个工具，同名工具后注册的覆盖先注册的。
func (r *Registry) Register(t Tool) {
	r.tools[t.Name] = t
}

// List 返回全部已注册工具（供 LLM 接口感知可用工具）。
func (r *Registry) List() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// Call 执行指定工具并返回结果。
//
// JS/TS ↔ Go 差异：JS/TS 习惯 throw 异常；Go 把错误作为返回值显式传递，
// 调用方必须处理 err——这就是 Go 著名的"错误显式"哲学。
func (r *Registry) Call(name, input string) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("未知工具: %s（护栏拦截，模型不能调用未注册工具）", name)
	}
	return t.Execute(input)
}
