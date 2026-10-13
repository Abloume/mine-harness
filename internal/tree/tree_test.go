package tree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 测试目录结构：
//
//	root/
//	├── README.md            (2 行)
//	├── docs/
//	│   └── guide.md         (3 行)
//	└── src/
//	    ├── main.go          (4 行)
//	    └── util.go          (2 行)
func makeTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"README.md":         "# Demo\nHello\n",
		"docs/guide.md":     "# Guide\nline2\nline3\n",
		"src/main.go":       "package main\nfunc main() {}\n// x\nline4\n",
		"src/util.go":       "package util\nfunc Util() {}\n",
		"src/.hidden.go":    "package hidden\n",
		".git/config":       "[core]\n",      // 应被忽略
		"node_modules/pkg":  "x\n",          // 应被忽略
		".env":              "API_KEY=secret\n", // 应被忽略（密钥防泄露）
		"junk.tmp":          "junk\n",       // 自定义忽略
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestBuildAndBrowse 构建后 Browse 根目录：忽略规则生效、子项按名排序、
// L0 摘要含行数与首行。
func TestBuildAndBrowse(t *testing.T) {
	root := makeTestTree(t)
	idx, err := NewTreeIndex(root, []string{".tmp"})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := idx.Browse(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, filepath.Base(n.Path))
	}
	// 忽略 .git / node_modules / junk.tmp / .env；src/docs 目录 + README.md 文件
	want := []string{"README.md", "docs", "src"}
	if len(names) != len(want) {
		t.Fatalf("根节点应为 %v, got %v", want, names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("排序应 %v, got %v", want, names)
		}
	}
	// README 的 L0 含行数与首行
	if !strings.Contains(nodes[0].L0, "2 行") || !strings.Contains(nodes[0].L0, "# Demo") {
		t.Fatalf("README L0 应含行数与首行, got %q", nodes[0].L0)
	}
}

// TestBrowseFileError browse 文件应报错（指引用 read）。
func TestBrowseFileError(t *testing.T) {
	root := makeTestTree(t)
	idx, _ := NewTreeIndex(root, nil)
	if _, err := idx.Browse(filepath.Join(root, "README.md")); err == nil {
		t.Fatal("browse 文件应报错")
	}
}

// TestOverviewDirAndFile 目录 L1 是子项清单；文件 L1 是头部 + 截断标记。
func TestOverviewDirAndFile(t *testing.T) {
	root := makeTestTree(t)
	idx, _ := NewTreeIndex(root, nil)

	dirOV, err := idx.Overview(filepath.Join(root, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dirOV, "main.go") || !strings.Contains(dirOV, "util.go") {
		t.Fatalf("目录 overview 应列子项, got:\n%s", dirOV)
	}

	// 多行文件：L1 只含前 40 行（此处 4 行全含）+ 行数
	fileOV, err := idx.Overview(filepath.Join(root, "src", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fileOV, "共 4 行") || !strings.Contains(fileOV, "package main") {
		t.Fatalf("文件 overview 应含行数与头部, got:\n%s", fileOV)
	}

	// 超过 40 行：L1 截断并标注"其余按需读取"
	long := filepath.Join(root, "long.go")
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("line\n")
	}
	if err := os.WriteFile(long, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// 构建是 walk 全量，需重建索引才能看到新文件
	idx2, err := NewTreeIndex(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	longOV, err := idx2.Overview(long)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(longOV, "共 50 行") || !strings.Contains(longOV, "其余 10 行按需读取") {
		t.Fatalf("长文件 L1 应截断并标注, got:\n%s", longOV)
	}
}

// TestReadWithLineNumbers L2 读取带行号（引用可追溯的载体）。
func TestReadWithLineNumbers(t *testing.T) {
	root := makeTestTree(t)
	idx, _ := NewTreeIndex(root, nil)
	content, err := idx.Read(filepath.Join(root, "src", "util.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "1: package util") || !strings.Contains(content, "2: func Util() {}") {
		t.Fatalf("read 应带行号, got:\n%s", content)
	}
	// 目录 read 报错（引导 browse/overview）
	if _, err := idx.Read(filepath.Join(root, "src")); err == nil {
		t.Fatal("read 目录应报错")
	}
	// 相对路径解析（检索工具入参常用相对路径）
	rel, err := idx.Read("src/util.go")
	if err != nil || !strings.Contains(rel, "package util") {
		t.Fatalf("相对路径读取失败: %v", err)
	}
}

// TestMissingPath 不存在的路径 browse/overview/read 都报错（不 panic）。
func TestMissingPath(t *testing.T) {
	root := makeTestTree(t)
	idx, _ := NewTreeIndex(root, nil)
	for _, op := range []func(string) error{
		func(p string) error { _, err := idx.Browse(p); return err },
		func(p string) error { _, err := idx.Overview(p); return err },
		func(p string) error { _, err := idx.Read(p); return err },
	} {
		if err := op(filepath.Join(root, "nope")); err == nil {
			t.Fatal("不存在的路径应报错")
		}
	}
}

// TestEnvSecretExcluded .env（含密钥）必须默认排除：browse 摘要/read 都
// 不能把密钥内容送进模型上下文（安全护栏回归）。
func TestEnvSecretExcluded(t *testing.T) {
	root := makeTestTree(t)
	idx, err := NewTreeIndex(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Read(".env"); err == nil {
		t.Fatal(".env 不应可读（密钥防泄露）")
	}
	if _, err := idx.Browse(root); err != nil {
		t.Fatal(err)
	}
}

// TestNewTreeIndexErrors 根路径不存在/不是目录都报错。
func TestNewTreeIndexErrors(t *testing.T) {
	if _, err := NewTreeIndex(filepath.Join(t.TempDir(), "ghost"), nil); err == nil {
		t.Fatal("不存在的根应报错")
	}
	f := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(f, []byte("x"), 0o644)
	if _, err := NewTreeIndex(f, nil); err == nil {
		t.Fatal("文件根应报错")
	}
}
