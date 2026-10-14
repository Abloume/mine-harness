package evolve

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// SkillStore 负责把进化后的 Skill 版本持久化到磁盘。
//
// 目录结构：
//
//	skills/<name>/
//	  SKILL.md          ← 当前生效版本（宿主 SkillProvider 读这个）
//	  history/
//	    v0.md           ← 初始版本
//	    v1.md           ← 第 1 轮进化后
//	    v2.md           ← 第 2 轮进化后
//
// 为什么要版本化：自进化是一个迭代过程，需要能回滚到任意历史版本。
// 论文里验证过"大改会引入回归"，版本历史就是后悔药。
//
// JS/TS ↔ Go 差异：TS 里文件操作用 fs 模块（callback/promise）；
// Go 用 os 包，错误处理用 if err != nil 显式判断（没有异常栈）。
type SkillStore struct {
	root string // skills/ 根目录
}

// NewSkillStore 构造存储。root 是 skills/ 目录的路径。
func NewSkillStore(root string) *SkillStore {
	return &SkillStore{root: root}
}

// SaveVersion 把某个版本的 Skill 正文存到 history 目录。
//
// 参数：
//   - skillName：技能名（对应 skills/<name>/ 目录）
//   - version：版本号（0 = 初始，1 = 第 1 轮进化后...）
//   - content：SKILL.md 正文
//
// 返回写入的文件路径。
func (s *SkillStore) SaveVersion(skillName string, version int, content string) (string, error) {
	dir := filepath.Join(s.root, skillName, "history")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("创建 history 目录失败: %w", err)
	}

	path := filepath.Join(dir, fmt.Sprintf("v%d.md", version))
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("写入版本文件失败: %w", err)
	}
	return path, nil
}

// LoadVersion 读回某个版本的 Skill 正文。
func (s *SkillStore) LoadVersion(skillName string, version int) (string, error) {
	path := filepath.Join(s.root, skillName, "history", fmt.Sprintf("v%d.md", version))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取版本文件失败: %w", err)
	}
	return string(data), nil
}

// ListVersions 列出某个 Skill 的所有历史版本号（升序）。
func (s *SkillStore) ListVersions(skillName string) ([]int, error) {
	dir := filepath.Join(s.root, skillName, "history")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取 history 目录失败: %w", err)
	}

	var versions []int
	for _, e := range entries {
		var v int
		_, err := fmt.Sscanf(e.Name(), "v%d.md", &v)
		if err == nil {
			versions = append(versions, v)
		}
	}
	sort.Ints(versions)
	return versions, nil
}

// PromoteToCurrent 把某个版本提升为"当前生效版本"——
// 即把 history/v<n>.md 复制到 skills/<name>/SKILL.md。
//
// 这是"切换 Skill 版本"的物理动作：宿主 SkillProvider 下次扫描时就读到新版本了。
func (s *SkillStore) PromoteToCurrent(skillName string, version int) error {
	content, err := s.LoadVersion(skillName, version)
	if err != nil {
		return err
	}

	currentPath := filepath.Join(s.root, skillName, "SKILL.md")
	if err := os.WriteFile(currentPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("提升当前版本失败: %w", err)
	}
	return nil
}
