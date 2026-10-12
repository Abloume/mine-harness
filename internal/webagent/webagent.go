// Package webagent 提供前端场景工具（PLAN 第 3 步剩余两块：本地预览 + 浏览器截屏）。
//
// 与 fsagent/cmdagent/figmaagent 同模式：独立包、只依赖 internal/agent 的 Tool 类型。
// 两个工具（都只影响本地/项目内 → RiskLow 自动放行）：
//   - serve_dir:  net/http 静态文件服务，起本地页面预览（端口占用检测 + 生命周期管理）
//   - screenshot: 无头 Chrome（CDP，rod 封装）截取页面 → PNG 落盘
//
// 与前三个包的关键差异：fsagent/cmdagent/figmaagent 的工具是"调用即返回"的无状态
// 工具；webagent 的 serve_dir 有"生命周期"——起的服务要能停、端口要能释放、进程要
// 能清理。这对应前端日常的"起 dev server → 用完关掉"。
//
// JS/TS ↔ Go 差异总览（详见各函数注释）：
//   - JS 起服务（http.createServer().listen）端口占用是异步 error 事件；
//     Go 的 net.Listen 失败同步返回——错误在 Serve 之前就暴露，天然好处理
//   - JS 没有 http.Server.Shutdown 的标准物；Go 有 context 超时版优雅关闭
//   - 浏览器自动化：JS/TS 用 puppeteer/playwright（Node 进程）；Go 用 rod
//     （封装 Chrome DevTools Protocol，WebSocket 通信，结构相似）
package webagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"

	"mine-harness/internal/agent"
)

// 默认值：服务优雅关闭宽限、截图超时、页面加载后的稳定等待。
const (
	shutdownGrace = 2 * time.Second
	shotTimeout   = 30 * time.Second
	loadSettle    = 400 * time.Millisecond // 等字体/懒加载稳定（教学简化：固定等待）
)

// Tools 是前端场景工具的宿主，绑定项目根目录（dir/out 都是相对 root 的路径）。
// 与 fsagent 同边界哲学：工具能碰的最大范围 = root。
type Tools struct {
	root string

	mu      sync.Mutex
	servers map[int]*http.Server // 当前由本工具起的服务：port → server
}

// NewTools 构造工具集。root 是项目根目录（相对路径都基于它解析）。
func NewTools(root string) *Tools {
	return &Tools{
		root:    root,
		servers: make(map[int]*http.Server),
	}
}

// All 返回全部工具定义。
func (t *Tools) All() []agent.Tool {
	return []agent.Tool{
		{
			Name: "serve_dir",
			Description: "用 net/http 起本地静态文件服务预览目录，返回 http://127.0.0.1:PORT/。" +
				"参数 JSON：{\"dir\":\"webdemo\",\"port\":可选端口(0=自动选空闲),\"action\":可选\"stop\"停止}" +
				"。端口被占用会报错提示换端口；同端口已起则复用返回；stop 停止指定端口服务（省略 port 停全部）。",
			BaseRisk: agent.RiskLow,
			Execute:  t.serveDir,
		},
		{
			Name: "screenshot",
			Description: "用无头 Chrome（rod/CDP）截取页面保存为 PNG，返回图片路径。" +
				"参数 JSON：{\"url\":\"http://127.0.0.1:8080/\",\"out\":\"shots/page.png\",\"width\":可选375,\"height\":可选812}" +
				"。out 是相对项目根的路径；截图 PNG 可交给视觉模型验证（多模态工具结果扩展点）。",
			BaseRisk: agent.RiskLow,
			Execute:  t.screenshot,
		},
	}
}

// ---- serve_dir ----

type serveReq struct {
	Dir    string `json:"dir"`
	Port   int    `json:"port"`             // 0 = 自动选空闲端口
	Action string `json:"action,omitempty"` // "stop" 停止服务
}

func (t *Tools) serveDir(input string) (string, error) {
	var req serveReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"dir\":\"webdemo\",\"port\":0}）", err)
	}

	if req.Action == "stop" {
		return t.stopServer(req.Port)
	}
	if strings.TrimSpace(req.Dir) == "" {
		return "", fmt.Errorf("dir 不能为空")
	}

	// 相对 root 解析 + 越界守卫（同 fsagent：filepath.Join 后再确认在 root 内）
	dir := filepath.Join(t.root, req.Dir)
	rel, err := filepath.Rel(t.root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("dir 越出项目根: %q", req.Dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("目录不存在或不是目录: %s（先确认页面文件已生成）", req.Dir)
	}

	port := req.Port

	// 复用已起服务：同端口（或同目录）已在服务中，直接返回 URL 不重复起
	t.mu.Lock()
	if _, ok := t.servers[port]; ok && port != 0 {
		t.mu.Unlock()
		return fmt.Sprintf("已复用服务: http://127.0.0.1:%d/（dir=%s）", port, req.Dir), nil
	}
	t.mu.Unlock()

	// 端口占用检测（JS/TS 对比）：Node 的 listen 撞端口是异步 'error' 事件，
	// 要监听回调才能发现；Go 的 net.Listen 同步失败直接返回 error——起服务前
	// 就确定端口可用，"准备"与"运行"分离，错误处理简单得多。
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", fmt.Errorf("端口 %d 被占用（%v）——换一个端口，或用 port=0 自动选空闲", port, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port // port=0 时拿到真实端口

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(dir)))
	srv := &http.Server{Handler: mux}

	// 服务在后台 goroutine 跑；错误通过 channel 回传（Serve 返回 ErrServerClosed
	// 是正常关闭，其余才值得报）
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	t.mu.Lock()
	t.servers[actualPort] = srv
	t.mu.Unlock()

	return fmt.Sprintf("已启动预览服务: http://127.0.0.1:%d/（dir=%s，可用 serve_dir action=stop 停止）", actualPort, req.Dir), nil
}

// stopServer 停止指定端口（0 = 全部）的服务并释放端口。
func (t *Tools) stopServer(port int) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.servers) == 0 {
		return "当前没有运行中的预览服务。", nil
	}
	stopped := 0
	for p, srv := range t.servers {
		if port != 0 && p != port {
			continue
		}
		// 优雅关闭（JS/TS 对比）：Node 是 server.close(cb) 手动等；Go 的
		// Shutdown 带 context 超时——到点强制断，不会卡死调用方
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		if err := srv.Shutdown(ctx); err != nil {
			cancel()
			return "", fmt.Errorf("停止端口 %d 失败: %v", p, err)
		}
		cancel()
		delete(t.servers, p)
		stopped++
	}
	return fmt.Sprintf("已停止 %d 个预览服务，端口已释放。", stopped), nil
}

// ---- screenshot ----

type shotReq struct {
	URL    string `json:"url"`
	Out    string `json:"out"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// screenshot 用无头 Chrome 截取页面。
// rod 封装了 Chrome DevTools Protocol（WebSocket）；这里显式用 launcher 找系统
// Chrome（Mac 上是 /Applications/Google Chrome.app），避免 rod 默认行为里"找不到
// 就下载 Chromium"（几百 MB，不该发生在用户机器上）。
// JS/TS 对比：puppeteer 的 launch() + page.goto + page.screenshot 一一对应
// launcher + browser.MustPage + MustScreenshot，概念同构。
func (t *Tools) screenshot(input string) (string, error) {
	var req shotReq
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("参数解析失败: %v（需要 JSON {\"url\":\"http://...\",\"out\":\"shots/a.png\"}）", err)
	}
	if req.URL == "" {
		return "", fmt.Errorf("url 不能为空")
	}
	if req.Out == "" {
		return "", fmt.Errorf("out 不能为空（PNG 保存路径，相对项目根）")
	}
	out := filepath.Join(t.root, req.Out)
	rel, err := filepath.Rel(t.root, out)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("out 越出项目根: %q", req.Out)
	}

	// 显式找系统 Chrome；找不到就报错（不自动下载）
	path, has := launcher.LookPath()
	if !has {
		return "", fmt.Errorf("未找到系统 Chrome——screenshot 依赖 Chrome，请安装或检查路径")
	}
	u := launcher.New().Bin(path).Headless(true).MustLaunch()
	browser := rod.New().ControlURL(u).Timeout(shotTimeout).MustConnect()
	defer browser.MustClose() // 关闭即清理浏览器进程（防僵尸，rod 内置 leakless）

	page := browser.MustPage(req.URL)
	page.MustWaitLoad()

	// 移动端尺寸模拟：width/height 可选（前端页面常用 375x812 验证）
	if req.Width > 0 {
		h := req.Height
		if h <= 0 {
			h = 0 // 0 = 保持页面自身高度
		}
		page.MustSetViewport(req.Width, h, 1, false)
		time.Sleep(loadSettle) // 等布局/字体稳定（教学简化：固定等待，生产可用网络空闲信号）
	}

	// 整页截图：fullPage=true（取页面完整高度，验证长页面）
	data, err := page.Screenshot(true, nil)
	if err != nil {
		return "", fmt.Errorf("截图失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", fmt.Errorf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return "", fmt.Errorf("写 PNG 失败: %v", err)
	}
	return fmt.Sprintf("截图已保存: %s（PNG %d 字节，可交给视觉模型比对设计稿）", out, len(data)), nil
}
