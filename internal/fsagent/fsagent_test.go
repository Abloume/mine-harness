package fsagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setup 在临时目录造一个迷你"项目"：
//
//	app.go        （含 "TODO: fix bug"）
//	sub/note.md   （含 "fix me later"）
//	README.md
func setup(t *testing.T) (*Tools, string) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, root, "app.go", "package main\n\n// TODO: fix bug\nfunc main() {}\n")
	writeTestFile(t, root, "sub/note.md", "# note\n\nfix me later\n")
	writeTestFile(t, root, "README.md", "# demo\n")
	return NewTools(root), root
}

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadFile(t *testing.T) {
	fs, _ := setup(t)
	out, err := fs.readFile(`{"path":"app.go"}`)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(out, "TODO: fix bug") {
		t.Errorf("应读到文件内容，实际 %q", out)
	}
}

func TestReadFileTruncatesWithNotice(t *testing.T) {
	fs, _ := setup(t)
	out, err := fs.readFile(`{"path":"app.go","max_len":10}`)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if strings.Contains(out, "TODO") {
		t.Errorf("max_len=10 时不应包含完整内容（首 10 字符是 \"package ma\"）: %q", out)
	}
	if !strings.Contains(out, "已截断") {
		t.Errorf("截断必须显式告知模型，实际 %q", out)
	}
}

// TestPathTraversalRejected 是 PLAN 第 1 步验证项：越权路径必须被拒。
// 覆盖三类：绝对路径、.. 穿越、混合穿越。
func TestPathTraversalRejected(t *testing.T) {
	fs, _ := setup(t)
	bad := []string{"/etc/passwd", "../secret.txt", "a/../../x", "sub/../../app.go"}
	for _, p := range bad {
		if _, err := fs.readFile(`{"path":"` + p + `"}`); err == nil {
			t.Errorf("路径 %q 应被拒（越界）", p)
		}
	}
	// 写、编辑同样受 resolve 保护
	if _, err := fs.writeFile(`{"path":"../../evil.txt","content":"x"}`); err == nil {
		t.Error("write_file 的越界路径应被拒")
	}
	if _, err := fs.editFile(`{"path":"/tmp/x","old":"a","new":"b"}`); err == nil {
		t.Error("edit_file 的绝对路径应被拒")
	}
}

func TestWriteFileBacksUpAndReplaces(t *testing.T) {
	fs, root := setup(t)
	out, err := fs.writeFile(`{"path":"app.go","content":"package main\n// v2\n"}`)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !strings.Contains(out, "已备份") {
		t.Errorf("覆盖已有文件必须返回备份信息，实际 %q", out)
	}
	// 内容已更新
	data, _ := os.ReadFile(filepath.Join(root, "app.go"))
	if !strings.Contains(string(data), "v2") {
		t.Errorf("文件内容应为 v2，实际 %q", string(data))
	}
	// 备份文件存在且保留旧内容（可回滚 = PLAN 验证项）
	baks, _ := filepath.Glob(filepath.Join(root, "backup", "*"))
	if len(baks) == 0 {
		t.Fatal("backup/ 下应有备份文件")
	}
	old, _ := os.ReadFile(baks[0])
	if !strings.Contains(string(old), "TODO: fix bug") {
		t.Errorf("备份应保留旧内容（可回滚），实际 %q", string(old))
	}
}

func TestWriteNewFileNoBackup(t *testing.T) {
	fs, root := setup(t)
	out, err := fs.writeFile(`{"path":"new.txt","content":"hi"}`)
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !strings.Contains(out, "新文件") {
		t.Errorf("新文件不应备份，实际 %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Errorf("新文件应已创建: %v", err)
	}
}

// TestWriteBackupFailureStopsWrite 验证 file-ops-policy："备份失败即停止"。
// 把 backup 位置占成一个普通文件 → MkdirAll 失败 → 写入必须中止。
func TestWriteBackupFailureStopsWrite(t *testing.T) {
	fs, root := setup(t)
	writeTestFile(t, root, "app.go", "old content")
	// 用同名普通文件挡住 backup/ 目录的创建
	writeTestFile(t, root, "backup", "not a directory")
	_, err := fs.writeFile(`{"path":"app.go","content":"new content"}`)
	if err == nil {
		t.Fatal("备份失败时应停止写入")
	}
	data, _ := os.ReadFile(filepath.Join(root, "app.go"))
	if string(data) != "old content" {
		t.Errorf("备份失败后文件不应被修改，实际 %q", string(data))
	}
}

func TestEditFileReplacesExactlyOnce(t *testing.T) {
	fs, root := setup(t)
	out, err := fs.editFile(`{"path":"app.go","old":"// TODO: fix bug","new":"// fixed"}`)
	if err != nil {
		t.Fatalf("编辑失败: %v", err)
	}
	if !strings.Contains(out, "已备份") {
		t.Errorf("编辑必须带备份，实际 %q", out)
	}
	data, _ := os.ReadFile(filepath.Join(root, "app.go"))
	if !strings.Contains(string(data), "// fixed") || strings.Contains(string(data), "TODO") {
		t.Errorf("应精确替换 TODO 为 fixed，实际 %q", string(data))
	}
}

func TestEditFileOldMissingFails(t *testing.T) {
	fs, _ := setup(t)
	// old 不存在：fail-fast，绝不静默"无事发生"
	_, err := fs.editFile(`{"path":"app.go","old":"不存在的文本","new":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Errorf("old 不存在应报错提示，实际 %v", err)
	}
}

func TestEditFileOldAmbiguousFails(t *testing.T) {
	fs, root := setup(t)
	writeTestFile(t, root, "dup.txt", "aaa bbb aaa\n")
	// old 出现 2 次：定位不精确，报错让模型先 read_file 再给唯一片段
	_, err := fs.editFile(`{"path":"dup.txt","old":"aaa","new":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "出现 2 次") {
		t.Errorf("old 歧义应报错，实际 %v", err)
	}
}

func TestGlob(t *testing.T) {
	fs, _ := setup(t)
	// **/*.go：任意深度的 .go 文件（简化实现：basename 匹配）
	out, err := fs.glob(`{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatalf("glob 失败: %v", err)
	}
	if !strings.Contains(out, "app.go") {
		t.Errorf("应命中 app.go，实际 %q", out)
	}
	// *.md 只匹配根层（filepath.Match 的 * 不跨目录）
	out, err = fs.glob(`{"pattern":"*.md"}`)
	if err != nil {
		t.Fatalf("glob 失败: %v", err)
	}
	if strings.Contains(out, "sub/") {
		t.Errorf("*.md 不应匹配子目录（sub/note.md），实际 %q", out)
	}
	if !strings.Contains(out, "README.md") {
		t.Errorf("应命中 README.md，实际 %q", out)
	}
}

func TestGlobSkipsBackup(t *testing.T) {
	fs, root := setup(t)
	if _, err := fs.writeFile(`{"path":"app.go","content":"v2"}`); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	out, err := fs.glob(`{"pattern":"**/*"}`)
	if err != nil {
		t.Fatalf("glob 失败: %v", err)
	}
	if strings.Contains(out, "backup") {
		t.Errorf("glob 不应列出 backup/ 目录，实际 %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "backup")); err != nil {
		t.Fatal("backup 目录应存在（被跳过 ≠ 不存在）")
	}
}

func TestGrepFindsMatchesWithLineNumbers(t *testing.T) {
	fs, _ := setup(t)
	out, err := fs.grep(`{"pattern":"fix"}`)
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if !strings.Contains(out, "app.go:3: // TODO: fix bug") {
		t.Errorf("应命中 app.go 第 3 行，实际 %q", out)
	}
	if !strings.Contains(out, "sub/note.md:3: fix me later") {
		t.Errorf("应命中 sub/note.md 第 3 行，实际 %q", out)
	}
}

func TestGrepSingleFile(t *testing.T) {
	fs, _ := setup(t)
	out, err := fs.grep(`{"pattern":"fix","path":"app.go"}`)
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if !strings.Contains(out, "app.go:3") || strings.Contains(out, "note.md") {
		t.Errorf("单文件搜索只应命中 app.go，实际 %q", out)
	}
}

// TestGrepSkipsBackup 验证：写文件产生的备份（含旧内容）不会被 grep 到——
// 否则模型会从备份里读到"已删除/已修改"的旧内容，造成幻觉。
func TestGrepSkipsBackup(t *testing.T) {
	fs, _ := setup(t)
	if _, err := fs.writeFile(`{"path":"app.go","content":"package main\n// v2\n"}`); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	out, err := fs.grep(`{"pattern":"TODO"}`)
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if strings.Contains(out, "backup") || strings.Contains(out, "TODO") {
		t.Errorf("grep 不应命中备份里的旧内容，实际 %q", out)
	}
}

func TestGrepMaxResults(t *testing.T) {
	fs, root := setup(t)
	// 造一个每行都匹配的文件
	writeTestFile(t, root, "many.txt", strings.Repeat("hit\n", 100))
	out, err := fs.grep(`{"pattern":"hit","path":"many.txt","max_results":10}`)
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if n := strings.Count(out, "\n"); n != 9 { // 10 行 = 9 个换行
		t.Errorf("max_results=10 应只返回 10 行，实际 %d 行", n+1)
	}
}

func TestUnknownToolNotInAll(t *testing.T) {
	// 工具名与注册表约定一致（宿主用 All() 注册，名字即模型可见 API）
	fs, _ := setup(t)
	for _, tool := range fs.All() {
		switch tool.Name {
		case "read_file", "write_file", "edit_file", "glob", "grep":
		default:
			t.Errorf("意外的工具名: %s", tool.Name)
		}
	}
}
