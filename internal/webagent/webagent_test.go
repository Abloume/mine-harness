package webagent

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher"
)

// setupRoot 造一个含 index.html 的迷你项目目录，返回 Tools。
func setupRoot(t *testing.T) (*Tools, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "webdemo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "webdemo", "index.html"),
		[]byte("<html><body><h1>hello webagent</h1></body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewTools(root), root
}

func TestServeDirStartsAndServes(t *testing.T) {
	tools, _ := setupRoot(t)
	out, err := tools.serveDir(`{"dir":"webdemo","port":0}`)
	if err != nil {
		t.Fatalf("起服务失败: %v", err)
	}
	if !strings.Contains(out, "http://127.0.0.1:") {
		t.Errorf("应返回预览 URL，实际 %q", out)
	}

	// 从返回文本里提取 URL（正则太繁，直接请求该前缀）
	url := "http://127.0.0.1:" + portOf(t, out) + "/"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("请求预览页失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hello webagent") {
		t.Errorf("应返回 index.html 内容，实际 %d %s", resp.StatusCode, body)
	}

	// stop 释放端口：再请求应连接失败
	if _, err := tools.serveDir(fmt.Sprintf(`{"action":"stop","port":%s}`, portOf(t, out))); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if _, err := http.Get(url); err == nil {
		t.Error("停止后端口应释放（请求应失败）")
	}
}

func TestServeDirReuseSamePort(t *testing.T) {
	tools, _ := setupRoot(t)
	out1, err := tools.serveDir(`{"dir":"webdemo","port":0}`)
	if err != nil {
		t.Fatalf("起服务失败: %v", err)
	}
	port := portOf(t, out1)
	// 第二次指定同一端口 → 复用不重复起
	out2, err := tools.serveDir(fmt.Sprintf(`{"dir":"webdemo","port":%s}`, port))
	if err != nil {
		t.Fatalf("复用失败: %v", err)
	}
	if !strings.Contains(out2, "已复用") {
		t.Errorf("同端口应复用，实际 %q", out2)
	}
	_, _ = tools.serveDir(`{"action":"stop"}`)
}

// TestServeDirPortInUse 验证端口占用被检测并给出换端口提示。
func TestServeDirPortInUse(t *testing.T) {
	tools, _ := setupRoot(t)
	// 先占一个端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port

	_, err = tools.serveDir(fmt.Sprintf(`{"dir":"webdemo","port":%d}`, busy))
	if err == nil || !strings.Contains(err.Error(), "占用") {
		t.Fatalf("端口占用应报错提示，实际 %v", err)
	}
}

func TestServeDirBadInput(t *testing.T) {
	tools, _ := setupRoot(t)
	if _, err := tools.serveDir(`{"dir":"no-such-dir"}`); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("目录不存在应报错，实际 %v", err)
	}
	if _, err := tools.serveDir(`{"dir":"../escape"}`); err == nil || !strings.Contains(err.Error(), "越出") {
		t.Errorf("越界目录应被拒，实际 %v", err)
	}
	if _, err := tools.serveDir(`{"dir":""}`); err == nil {
		t.Error("空 dir 应报错")
	}
}

func TestServeDirStopAll(t *testing.T) {
	tools, _ := setupRoot(t)
	if _, err := tools.serveDir(`{"dir":"webdemo","port":0}`); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.serveDir(`{"dir":"webdemo","port":0}`); err != nil {
		t.Fatal(err)
	}
	// 端口 0 起两个服务，实际端口不同（都活着时 servers 应有 2 个）
	tools.mu.Lock()
	n := len(tools.servers)
	tools.mu.Unlock()
	if n != 2 {
		t.Fatalf("应起 2 个服务，实际 %d", n)
	}
	out, err := tools.serveDir(`{"action":"stop"}`)
	if err != nil || !strings.Contains(out, "已停止 2 个") {
		t.Fatalf("stop 全部应停 2 个，实际 %v %q", err, out)
	}
}

// ---- screenshot ----

func TestScreenshotBadInput(t *testing.T) {
	tools, _ := setupRoot(t)
	if _, err := tools.screenshot(`{"url":""}`); err == nil {
		t.Error("空 url 应报错")
	}
	if _, err := tools.screenshot(`{"url":"http://x","out":""}`); err == nil {
		t.Error("空 out 应报错")
	}
	if _, err := tools.screenshot(`{"url":"http://x","out":"../escape.png"}`); err == nil || !strings.Contains(err.Error(), "越出") {
		t.Errorf("越界 out 应被拒，实际 %v", err)
	}
}

// TestScreenshotPNG 真实起无头 Chrome 截图（本机有 Chrome 才跑，否则 Skip）。
func TestScreenshotPNG(t *testing.T) {
	if _, has := launcher.LookPath(); !has {
		t.Skip("本机无 Chrome，跳过截图测试")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body style="margin:0"><h1 style="background:#1e6fff;color:#fff;padding:20px">shot me</h1></body></html>`)
	}))
	defer srv.Close()

	root := t.TempDir()
	tools := NewTools(root)
	out, err := tools.screenshot(fmt.Sprintf(`{"url":%q,"out":"shots/page.png","width":375}`, srv.URL))
	if err != nil {
		t.Fatalf("截图失败: %v", err)
	}
	if !strings.Contains(out, "PNG") {
		t.Errorf("应返回 PNG 结果说明，实际 %q", out)
	}
	data, err := os.ReadFile(filepath.Join(root, "shots", "page.png"))
	if err != nil {
		t.Fatalf("截图文件不存在: %v", err)
	}
	// PNG 魔数 \x89PNG\r\n\x1a\n
	if len(data) < 8 || data[0] != 0x89 || string(data[1:4]) != "PNG" {
		t.Errorf("产物应为 PNG，前 8 字节 %v", data[:min(8, len(data))])
	}
}

// portOf 从 serve 返回文本提取端口（形如 http://127.0.0.1:PORT/）。
func portOf(t *testing.T, out string) string {
	t.Helper()
	// 提取 ":PORT/" 之间的数字
	start := strings.Index(out, "127.0.0.1:") + len("127.0.0.1:")
	end := strings.Index(out[start:], "/")
	if start < 0 || end < 0 {
		t.Fatalf("无法从 %q 提取端口", out)
	}
	return out[start : start+end]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
