// Package memory 实现 Agent 的"长期记忆地基"（PLAN 第 5 步阶段一）：
// 会话持久化 + 断点续跑。
//
// 定位：单次 Run 的上下文（history）从"进程内存"落盘为 JSON 文件，
// 下次 Run 恢复为初始上下文——模型不用重述已经做过的探索与结论。
// 这也是阶段二（树索引 + 推理检索）对历史做召回的前提。
//
// 依赖边界：本包 import internal/agent 的消息类型（学习项目允许；
// 生产框架会把消息模型放在独立 schema 包避免循环依赖）。
package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mine-harness/internal/agent"
)

// SessionStore 是会话的本地文件存储：每个会话一个 JSON 文件。
//
// 文件格式：JSON 数组（[]agent.Message），人类可读、可用 jq 检查。
//
// JS/TS ↔ Go 差异：JS 常直接 fs.writeFile 覆盖；Go 这里刻意用"同目录
// temp + rename"原子写——写一半崩溃不会留下半截文件（JSON 解析不了的
// 坏会话会让后续 Load 直接失败，这比静默损坏更安全）。
type SessionStore struct {
	dir string
}

// NewSessionStore 构造会话存储；目录不存在则创建。
func NewSessionStore(dir string) (*SessionStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("会话目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建会话目录失败: %w", err)
	}
	return &SessionStore{dir: dir}, nil
}

// Save 持久化会话历史（原子写，覆盖旧文件）。
// sessionID 只允许字母数字与 -_：会话 ID 会拼进文件路径，必须防路径穿越。
func (s *SessionStore) Save(sessionID string, msgs []agent.Message) error {
	if !validID(sessionID) {
		return fmt.Errorf("非法会话 ID: %q（只允许字母、数字、-、_）", sessionID)
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化会话失败: %w", err)
	}
	return atomicWrite(s.path(sessionID), data)
}

// Load 恢复会话历史。ok=false 表示会话不存在（这是正常情况，不是错误——
// 首次续跑某会话前它本来就没有落盘文件）。
func (s *SessionStore) Load(sessionID string) ([]agent.Message, bool, error) {
	if !validID(sessionID) {
		return nil, false, fmt.Errorf("非法会话 ID: %q", sessionID)
	}
	data, err := os.ReadFile(s.path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读取会话失败: %w", err)
	}
	var msgs []agent.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, false, fmt.Errorf("解析会话失败（文件损坏?）: %w", err)
	}
	return msgs, true, nil
}

// List 返回全部已落盘会话 ID（排序，确定性输出）。
func (s *SessionStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *SessionStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// validID 只允许字母数字与 -_（会话 ID 拼进路径，防穿越/防注入）。
func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// atomicWrite 同目录 temp + rename（与 fsagent 同一模式）：
// POSIX 上同文件系统 rename 是原子操作——要么旧文件、要么新文件，
// 绝无"写到一半"的半截文件。temp 用 defer Remove 兜底清理。
//
// JS/TS ↔ Go 差异：JS writeFile 直接覆盖（写一半断电就坏）；Go 的
// os.Rename 原子替换是"落盘类功能"的通用护栏，会话/备份/配置都用它。
func atomicWrite(full string, data []byte) error {
	dir := filepath.Dir(full)
	tmp, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}
