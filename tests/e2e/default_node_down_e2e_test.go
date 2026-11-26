package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// 默认节点连不上时的端到端验证：本机起节点模式的 relay，默认节点 A 分别是拒绝连接、接了连接不握手、运行中被杀掉、
// TCP 下行黑洞（握手和请求的应答都过得去，之后 relay 发回的数据全被丢掉）。看 share 是否换到 B、协助端只给协助码能否连上，
// 以及 share 有没有在连不通的节点上先显示一个用不了的协助码。

// downBannerRE 只认行首的「协助码: XXXX-XXXX-XXXX」横幅：换码提示「原协助码 X 无法继续使用」里的是旧码。
var downBannerRE = regexp.MustCompile(`(?m)^\s*协助码[:：]\s*([0-9A-Za-z]{4}(?:-[0-9A-Za-z]+)+)`)

// bannerCodes 是 share 输出里先后打出的协助码横幅（去掉连字符）。
func bannerCodes(out string) []string {
	var codes []string
	for _, m := range downBannerRE.FindAllStringSubmatch(out, -1) {
		codes = append(codes, strings.ReplaceAll(m[1], "-", ""))
	}
	return codes
}

// listNode 是列表里的一个节点：TCP、QUIC 地址可以和 relay 实际监听的不同（前面挡着代理）。
type listNode struct {
	n         *e2eNode
	tcp, quic []string
}

// signDownList 用测试根签一份 prefer-default 列表，第一个节点是默认节点，全部 active。
func signDownList(t *testing.T, f *dirtest.Fixture, nodes ...listNode) []byte {
	t.Helper()
	var entries []directory.NodeEntry
	for _, ln := range nodes {
		n := ln.n
		c := f.Cert(n.prefix, func(c *directory.NodeCert) {
			c.Name, c.Operator = n.name, "e2e"
			c.TCP, c.QUIC = ln.tcp, ln.quic
			c.TLSFPs = []string{n.fp}
		})
		entries = append(entries, directory.NodeEntry{Cert: f.SignCert(c), Status: directory.StatusActive, Weight: 100})
	}
	b := f.Body(entries...)
	b.Policy = directory.PolicyPreferDefault
	return f.Sign(b)
}

// startNodeWith 同 e2eNode.start（节点模式），多带 extra 参数，返回进程（测试要中途杀掉它）。
func startNodeWith(t *testing.T, relayBin string, n *e2eNode, extra ...string) *exec.Cmd {
	t.Helper()
	args := []string{"--listen", n.addr,
		"--cert", filepath.Join(n.dir, "certs", "server.crt"),
		"--key", filepath.Join(n.dir, "certs", "server.key"),
		"--stun", "",
		"--state-dir", filepath.Join(n.dir, "state"),
		"--blocklist-db", filepath.Join(n.dir, "blocklist.json"),
		"--blocklist-threshold", "1",
		"--directory-url", deadDirectoryURL,
	}
	cmd := exec.Command(relayBin, append(args, extra...)...)
	cmd.Env = isolatedEnv(t, n.dir)
	n.out = &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = n.out, n.out
	if err := cmd.Start(); err != nil {
		t.Fatalf("relay %s start: %v", n.prefix, err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	want := []string{"Server starting on", fmt.Sprintf("this relay is node %s %q", n.prefix, n.name)}
	deadline := time.Now().Add(15 * time.Second)
	for {
		out := n.out.String()
		if !slices.ContainsFunc(want, func(w string) bool { return !strings.Contains(out, w) }) {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay %s 没有就绪（等 %q）:\n%s", n.prefix, want, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// launchShare 起 share（共享 home/shared，--p2p disabled），不等协助码。
func launchShare(t *testing.T, bin, home string, env []string, args ...string) *e2eShare {
	t.Helper()
	s := &e2eShare{root: filepath.Join(home, "shared"), out: &lockedBuffer{}}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, "hello.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, append([]string{"share", "--root", s.root, "--p2p", "disabled"}, args...)...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = s.out, s.out
	if err := cmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return s
}

// waitBanner 等 share 打出第 nth 个（从 1 数）协助码横幅，返回那个码。
func waitBanner(t *testing.T, s *e2eShare, nth int, wait time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		if codes := bannerCodes(s.out.String()); len(codes) >= nth {
			s.code = codes[nth-1]
			return s.code
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v 内 share 没有打出第 %d 个协助码:\n%s", wait, nth, s.out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// downEnv 是两台节点（A 为默认节点）都写好列表缓存后的公共准备：返回 share、help 的家目录。
func downEnv(t *testing.T, f *dirtest.Fixture, root string, raw []byte, nodes ...*e2eNode) (shareHome, helpHome string) {
	t.Helper()
	for _, n := range nodes {
		seedCache(t, f, n.cachePath(), raw)
	}
	shareHome, helpHome = filepath.Join(root, "share-home"), filepath.Join(root, "help-home")
	seedCache(t, f, clientCache(shareHome), raw)
	seedCache(t, f, clientCache(helpHome), raw)
	return shareHome, helpHome
}

// startHelpWait 同 startE2EHelp，initialize 最多等 initWait：带 --code 的 help 先连上、加入完才开始应答 MCP，
// TCP 加入确认不了、要改用 QUIC 重来时要十几秒。
func startHelpWait(t *testing.T, bin string, env []string, initWait time.Duration, args ...string) *mcpSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, append([]string{"help", "--mcp-stdio", "--p2p", "disabled"}, args...)...)
	cmd.Env = env
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("help start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	s := &mcpSession{t: t, stdin: stdin, stderr: stderr}
	s.cond = sync.NewCond(&s.mu)
	go s.readLoop(stdout)
	s.call(1, "initialize", map[string]any{}, initWait)
	return s
}

// helpReadsHello 只给协助码起 help，读到 share 目录里的 hello.txt 才算通；返回 help 的 stderr。
func helpReadsHello(t *testing.T, remoteBin, helpHome, defaultAddr string, sh *e2eShare) string {
	t.Helper()
	s := startHelpWait(t, remoteBin, isolatedEnv(t, helpHome, "RAT_E2E_DEFAULT_RELAY="+defaultAddr), 40*time.Second, "--code", sh.code)
	if ok, last := readHello(t, s, sh.root); !ok {
		t.Fatalf("只给协助码的 help 没连上: %s\nhelp stderr:\n%s\nshare:\n%s", last, s.stderr.String(), sh.out.String())
	}
	return s.stderr.String()
}

func wantContains(t *testing.T, what, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("%s 里应有 %q:\n%s", what, want, text)
	}
}

// TestMultiRelayDefaultRefusedFallsBack 默认节点 A 在列表里，地址上没人监听（拒绝连接）：share 换到 B，
// 只给协助码的 help 按码的首位直接连 B。
func TestMultiRelayDefaultRefusedFallsBack(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18481) // 不启动
	b := newE2ENode(t, relayBin, root, "B", 18482)
	raw := signDownList(t, f, listNode{a, []string{a.addr}, nil}, listNode{b, []string{b.addr}, nil})
	shareHome, helpHome := downEnv(t, f, root, raw, a, b)
	startNodeWith(t, relayBin, b)

	start := time.Now()
	sh := launchShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	code := waitBanner(t, sh, 1, 60*time.Second)
	t.Logf("share %v 后拿到协助码，首位 %s", time.Since(start).Round(time.Millisecond), code[:1])
	if !strings.HasPrefix(code, "B") {
		t.Fatalf("默认节点 A 连不上，share 应换到 B，码却是 %s:\n%s", code, sh.out.String())
	}
	wantContains(t, "share 输出", sh.out.String(), "默认节点 e2e-a（A） 连接失败，已改用节点 e2e-b（B）")

	stderr := helpReadsHello(t, remoteBin, helpHome, a.addr, sh)
	wantContains(t, "help stderr", stderr, "协助码属于节点 e2e-b（B）")
	b.assertNotBanned(t)
}

// TestMultiRelayDefaultHangsFallsBack 默认节点 A 的地址接受 TCP 连接，但一个字节都不回（进程假死、握手被中间设备卡住）：
// 等拨号超时后换到 B。
func TestMultiRelayDefaultHangsFallsBack(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18483)
	b := newE2ENode(t, relayBin, root, "B", 18484)
	raw := signDownList(t, f, listNode{a, []string{a.addr}, nil}, listNode{b, []string{b.addr}, nil})
	shareHome, helpHome := downEnv(t, f, root, raw, a, b)
	startNodeWith(t, relayBin, b)

	ln, err := net.Listen("tcp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	})

	start := time.Now()
	sh := launchShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	code := waitBanner(t, sh, 1, 90*time.Second)
	t.Logf("share %v 后拿到协助码，首位 %s", time.Since(start).Round(time.Millisecond), code[:1])
	if !strings.HasPrefix(code, "B") {
		t.Fatalf("默认节点 A 不回数据，share 应换到 B，码却是 %s:\n%s", code, sh.out.String())
	}
	wantContains(t, "share 输出", sh.out.String(), "默认节点 e2e-a（A） 连接失败，已改用节点 e2e-b（B）")

	helpReadsHello(t, remoteBin, helpHome, a.addr, sh)
	b.assertNotBanned(t)
}

// TestMultiRelayDefaultDiesWhileSharing share 已经在默认节点 A 上，A 被杀掉：先回 A 重连，连续 3 次失败后换到 B，
// 换了码；协助端用新码连上。
func TestMultiRelayDefaultDiesWhileSharing(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18485)
	b := newE2ENode(t, relayBin, root, "B", 18486)
	raw := signDownList(t, f, listNode{a, []string{a.addr}, nil}, listNode{b, []string{b.addr}, nil})
	shareHome, helpHome := downEnv(t, f, root, raw, a, b)
	aCmd := startNodeWith(t, relayBin, a)
	startNodeWith(t, relayBin, b)

	sh := launchShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	if code := waitBanner(t, sh, 1, 30*time.Second); !strings.HasPrefix(code, "A") {
		t.Fatalf("A、B 都正常时 share 应在默认节点 A 上，码却是 %s:\n%s", code, sh.out.String())
	}
	a.assertNotBanned(t)

	if err := aCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	code := waitBanner(t, sh, 2, 90*time.Second)
	t.Logf("A 被杀后 %v，share 打出新协助码，首位 %s", time.Since(killed).Round(time.Millisecond), code[:1])
	if !strings.HasPrefix(code, "B") {
		t.Fatalf("A 挂了，share 应换到 B，新码却是 %s:\n%s", code, sh.out.String())
	}
	wantContains(t, "share 输出", sh.out.String(), "节点 e2e-a（A） 连续 3 次连接或注册失败，重新选择节点")

	helpReadsHello(t, remoteBin, helpHome, a.addr, sh)
	b.assertNotBanned(t)
}

// downlinkBlackhole 是挡在 relay 前面的 TCP 代理，模拟线上遇到的下行黑洞：客户端发的全部转给 relay；relay 发回的
// 只转到客户端发出第 3 条 TLS 应用数据记录（TLS 1.3 下依次是握手的 Finished、注册或加入请求、之后的第一条消息）为止，
// 或者请求发出 1 秒后为止，之后全部丢掉。握手、请求的应答都过得去，后面的数据收不到。
type downlinkBlackhole struct {
	ln       net.Listener
	upstream string
	dropped  atomic.Int64 // 丢掉的下行字节数
}

func startBlackhole(t *testing.T, listen, upstream string) *downlinkBlackhole {
	t.Helper()
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	b := &downlinkBlackhole{ln: ln, upstream: upstream}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	track := func(c net.Conn) {
		mu.Lock()
		conns = append(conns, c)
		mu.Unlock()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			track(c)
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.serve(c, track)
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return b
}

// tlsRecords 数一条 TLS 字节流里的记录（只看 5 字节的记录头，不解密）。
type tlsRecords struct {
	hdr     [5]byte
	filled  int
	skip    int
	appData int // 类型 23（application_data）的记录数
}

func (r *tlsRecords) feed(p []byte) {
	for len(p) > 0 {
		if r.skip > 0 {
			k := min(r.skip, len(p))
			r.skip -= k
			p = p[k:]
			continue
		}
		k := copy(r.hdr[r.filled:], p)
		r.filled += k
		p = p[k:]
		if r.filled == len(r.hdr) {
			if r.hdr[0] == 23 {
				r.appData++
			}
			r.skip = int(r.hdr[3])<<8 | int(r.hdr[4])
			r.filled = 0
		}
	}
}

func (b *downlinkBlackhole) serve(c net.Conn, track func(net.Conn)) {
	up, err := net.Dial("tcp", b.upstream)
	if err != nil {
		c.Close()
		return
	}
	track(up)
	var appData atomic.Int32
	var requestAt atomic.Int64 // 第 2 条应用数据记录（请求）经过的时间，UnixNano
	go func() {
		var rec tlsRecords
		buf := make([]byte, 32<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				rec.feed(buf[:n])
				appData.Store(int32(rec.appData))
				if rec.appData >= 2 {
					requestAt.CompareAndSwap(0, time.Now().UnixNano())
				}
				if _, werr := up.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		up.Close()
		c.Close()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := up.Read(buf)
		if n > 0 {
			at := requestAt.Load()
			if appData.Load() >= 3 || (at != 0 && time.Since(time.Unix(0, at)) > time.Second) {
				b.dropped.Add(int64(n))
			} else if _, werr := c.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	up.Close()
	c.Close()
}

// TestMultiRelayDefaultTCPBlackholeNoQUIC 默认节点 A 只有 TCP 地址，前面是下行黑洞：share 不能在 A 上显示协助码
// （那个码协助端用不了），连续 3 次失败后换到 B；协助端只给码连上 B。
func TestMultiRelayDefaultTCPBlackholeNoQUIC(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18487) // relay 实际监听的地址，列表里写的是代理
	b := newE2ENode(t, relayBin, root, "B", 18489)
	proxyAddr := "127.0.0.1:18488"
	raw := signDownList(t, f, listNode{a, []string{proxyAddr}, nil}, listNode{b, []string{b.addr}, nil})
	shareHome, helpHome := downEnv(t, f, root, raw, a, b)
	startNodeWith(t, relayBin, a)
	startNodeWith(t, relayBin, b)
	hole := startBlackhole(t, proxyAddr, a.addr)

	start := time.Now()
	sh := launchShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+proxyAddr))
	code := waitBanner(t, sh, 1, 120*time.Second)
	t.Logf("share %v 后打出第一个协助码，首位 %s；代理丢掉下行 %d 字节", time.Since(start).Round(time.Millisecond), code[:1], hole.dropped.Load())
	if !strings.HasPrefix(code, "B") {
		t.Fatalf("A 的 TCP 下行不通、又没有 QUIC，share 不该在 A 上显示协助码，第一个码却是 %s:\n%s", code, sh.out.String())
	}
	if hole.dropped.Load() == 0 {
		t.Errorf("代理没有丢过下行数据，没模拟出黑洞")
	}
	// 节点只有 TCP 地址，没有 QUIC 可改，所以只报收不到数据，不说「之后先走 QUIC」。
	wantContains(t, "share 输出", sh.out.String(), "TCP 连接已经建立，但收不到它发回的数据")
	wantContains(t, "share 输出", sh.out.String(), "节点 e2e-a（A） 连续 3 次连接或注册失败，重新选择节点")

	helpReadsHello(t, remoteBin, helpHome, proxyAddr, sh)
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}

// TestMultiRelayDefaultTCPBlackholeWithQUIC 默认节点 A 的 TCP 前面是下行黑洞，同一端口的 QUIC（UDP）是通的（线上那次的情况）：
// share 改用 QUIC 留在 A 上，不显示 TCP 上那个用不了的码，也不换节点；help 只给码，TCP 加入确认不了后改用 QUIC 连上。
func TestMultiRelayDefaultTCPBlackholeWithQUIC(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18490) // relay 的 TCP 监听在这里；QUIC 和代理共用 18491
	b := newE2ENode(t, relayBin, root, "B", 18492)
	pub := "127.0.0.1:18491"
	raw := signDownList(t, f, listNode{a, []string{pub}, []string{pub}}, listNode{b, []string{b.addr}, nil})
	shareHome, helpHome := downEnv(t, f, root, raw, a, b)
	startNodeWith(t, relayBin, a, "--quic-listen", pub)
	startNodeWith(t, relayBin, b)
	hole := startBlackhole(t, pub, a.addr)

	start := time.Now()
	sh := launchShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+pub))
	code := waitBanner(t, sh, 1, 90*time.Second)
	t.Logf("share %v 后打出第一个协助码，首位 %s；代理丢掉下行 %d 字节", time.Since(start).Round(time.Millisecond), code[:1], hole.dropped.Load())
	if !strings.HasPrefix(code, "A") {
		t.Fatalf("A 的 QUIC 是通的，share 应改用 QUIC 留在 A 上，码却是 %s:\n%s", code, sh.out.String())
	}
	if hole.dropped.Load() == 0 {
		t.Errorf("代理没有丢过下行数据，没模拟出黑洞")
	}
	out := sh.out.String()
	wantContains(t, "share 输出", out, "TCP 连接收不到它发回的数据，之后连它先走 QUIC（UDP）")
	if strings.Contains(out, "已改用节点") {
		t.Errorf("QUIC 通时不该换节点:\n%s", out)
	}

	helpStart := time.Now()
	stderr := helpReadsHello(t, remoteBin, helpHome, pub, sh)
	t.Logf("help %v 后读到文件", time.Since(helpStart).Round(time.Millisecond))
	wantContains(t, "help stderr", stderr, "QUIC")
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}
