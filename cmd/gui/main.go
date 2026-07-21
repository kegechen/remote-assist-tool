package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/gui"
)

// main 启动 remote-assist GUI：
//  1. 定位同目录/相邻目录下的 remote 可执行文件
//  2. 拉起后台 HTTP server（前端通过浏览器交互）
//  3. 自动打开默认浏览器
func main() {
	binPath, err := findRemoteBin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "找不到 remote 可执行文件: %v\n", err)
		os.Exit(1)
	}

	addr := os.Getenv("GUI_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8731"
	}
	defaultServer := os.Getenv("REMOTE_RELAY_SERVER")

	srv := gui.NewServer(binPath, defaultServer)
	mux := srv.Routes()

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "监听 %s 失败: %v\n", addr, err)
		os.Exit(1)
	}

	// token 必须带在 URL 里：前端从 query 取到它，之后每次调 API 都带 X-Auth-Token。
	// 不带 token 打开只会看到首页，任何 API 都会 403（见 gui.Server.guard）。
	url := "http://" + listener.Addr().String() + "/?token=" + srv.Token()
	fmt.Println("Remote Assist GUI 已启动:", url)
	fmt.Println("在浏览器中打开上面的地址（**必须带 token**）。Ctrl+C 退出。")
	if !isLoopbackAddr(addr) {
		fmt.Fprintf(os.Stderr, "\n警告: GUI_LISTEN=%s 监听在非本机地址上。\n"+
			"这个界面能在远端机器上执行任意命令，因此后端只接受 Host 为 localhost/127.0.0.1 的请求"+
			"（防 DNS rebinding）——从别的机器直接访问会被拒。\n"+
			"要远程用，请做端口转发: ssh -L %s:localhost:%s <本机>\n\n",
			addr, portOf(addr), portOf(addr))
	}

	// 稍等 server 起来再开浏览器
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = openBrowser(url)
	}()

	httpSrv := &http.Server{Handler: mux}
	if err := httpSrv.Serve(listener); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

// isLoopbackAddr 判断监听地址是否只对本机开放。空主机（如 ":8731"）等价于 0.0.0.0，
// 即所有网卡，不算 loopback。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false // ":8731" = 所有网卡
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return addr
}

// cliNames 返回 CLI 二进制的候选文件名，按优先级排列。
//
// 首选与本进程同平台同架构的那个（webui 是 windows-amd64，就该配 windows-amd64 的 cli）；
// 再退到不带平台后缀的名字，方便有人自己改名或从别处拷一个过来。
func cliNames() []string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return []string{
		"remote-assist-cli-" + runtime.GOOS + "-" + runtime.GOARCH + ext,
		"remote-assist-cli" + ext,
	}
}

// findRemoteBin 在多个常见位置查找 CLI 可执行文件（webui 靠拉起它来干活）。
func findRemoteBin() (string, error) {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe)) // 与 webui 同目录：发布包的常态
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, filepath.Join(wd, "bin"), wd) // 源码树里直接跑
	}
	var tried []string
	for _, d := range dirs {
		for _, n := range cliNames() {
			p := filepath.Join(d, n)
			tried = append(tried, p)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("未找到 %s；已找过: %v", strings.Join(cliNames(), " / "), tried)
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch {
	case os.Getenv("windir") != "" || os.Getenv("SystemRoot") != "":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case os.Getenv("DISPLAY") != "":
		cmd = exec.Command("xdg-open", url)
	default:
		cmd = exec.Command("open", url)
	}
	return cmd.Start()
}
