package e2e

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// 多 relay 节点列表的端到端验证（spec 第 11 节 e2e）：本机起两台节点模式的 relay，列表用 dirtest 的测试根签，
// 验证 share 自动选节点、help 只给协助码也能按码的首位找到节点；再拿已发布的客户端、线上版本的 relay 交叉验证兼容性。
//
// 正式构建的内置根是真实的根公钥，测试签不出它认的列表，所以这里用 -tags remoteassist_e2e 自己编 remote 和 relay：
// 这个 tag 把内置根换成 dirtest 的测试根（internal/directory/roots_e2e.go），把内置默认 relay 换成 RAT_E2E_DEFAULT_RELAY
// （cmd/remote/default_relay_e2e.go，没设时是连不上的本机地址），测试绝不会连到线上 relay。CF 一律关掉
// （REMOTE_RELAY_DIRECTORY=off）：列表事先写进各自的缓存，或者由 relay 随注册、加入的响应转发。
//
// 每个进程有自己的家目录和临时目录（列表缓存、指纹表、日志文件互不相干）。relay 的黑名单阈值设成 1：
// 选节点时的竞速、正常的注册和加入只要被计一分就会封掉 127.0.0.1，日志里出现 auto-banned 即失败（spec 6.9）。
//
// 兼容性那几条要旧二进制，没有就跳过：
//
//	RAT_RELEASED_CLI    最近一次发布的 remote（例如 v1.2.3）
//	RAT_DEPLOYED_RELAY  线上 relay 正在跑的版本（带黑名单、没有节点模式）
//
// 本地准备（源码导出到仓库外，不动 worktree）：
//
//	mkdir /tmp/rat-v123 && git archive v1.2.3 | tar -x -C /tmp/rat-v123
//	go -C /tmp/rat-v123 build -o /tmp/cli-v123.exe ./cmd/remote
//	RAT_RELEASED_CLI=/tmp/cli-v123.exe RAT_DEPLOYED_RELAY=/tmp/relay-deployed.exe go test ./tests/e2e/ -run MultiRelay -v
//
// CI（环境变量 CI 非空）上默认跳过：CI 的主步骤是 go test ./... -timeout 120s，各包并行，这里还要另编两个二进制、
// 起多个进程，既可能超时也容易受负载影响。设 RAT_E2E_MULTI_RELAY=1 才跑——.github/workflows/test.yml 在 ubuntu 上
// 单独一步这样跑，放宽了 -timeout，RAT_RELEASED_CLI 用 v1.2.3 源码编的 remote。

const e2eBuildTag = "remoteassist_e2e"

// deadDirectoryURL 是节点模式 relay 的拉取地址：连不上，relay 只用事先写进状态目录的那份列表。
const deadDirectoryURL = "https://127.0.0.1:9/v1/directory"

var (
	taggedOnce sync.Once
	taggedDir  string
	taggedErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if taggedDir != "" {
		os.RemoveAll(taggedDir)
	}
	os.Exit(code)
}

// taggedBins 用 -tags remoteassist_e2e 编 remote 和 relay（每次 go test 只编一次，结束时删掉）。
func taggedBins(t *testing.T) (remoteBin, relayBin string) {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	if os.Getenv("CI") != "" && os.Getenv("RAT_E2E_MULTI_RELAY") != "1" {
		t.Skip("CI 上默认跳过多 relay e2e（设 RAT_E2E_MULTI_RELAY=1 强制运行）")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("PATH 里没有 go，编不了 e2e 测试构建")
	}
	taggedOnce.Do(func() {
		wd, _ := os.Getwd()
		repo := filepath.Clean(filepath.Join(wd, "..", ".."))
		if taggedDir, taggedErr = os.MkdirTemp("", "rat-e2e-tagged-"); taggedErr != nil {
			return
		}
		for _, pkg := range []string{"remote", "relay"} {
			cmd := exec.Command(goBin, "build", "-tags", e2eBuildTag, "-o", filepath.Join(taggedDir, pkg+exeExt()), "./cmd/"+pkg)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				taggedErr = fmt.Errorf("go build -tags %s ./cmd/%s: %v\n%s", e2eBuildTag, pkg, err, out)
				return
			}
		}
	})
	if taggedErr != nil {
		t.Fatal(taggedErr)
	}
	return filepath.Join(taggedDir, "remote"+exeExt()), filepath.Join(taggedDir, "relay"+exeExt())
}

// envBin 取旧二进制的路径；没设或文件不存在时跳过。
func envBin(t *testing.T, env string) string {
	t.Helper()
	p := os.Getenv(env)
	if p == "" {
		t.Skipf("%s 未设置，跳过", env)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("%s 指向的文件不存在: %v", env, err)
	}
	return p
}

// isolatedEnv 是子进程的环境：家目录、临时目录都指到 home 下，去掉会改变 relay 选择的变量，关掉 CF。
func isolatedEnv(t *testing.T, home string, extra ...string) []string {
	t.Helper()
	tmp := filepath.Join(home, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	drop := []string{"HOME", "USERPROFILE", "TMP", "TEMP", "TMPDIR",
		"REMOTE_RELAY_SERVER", "REMOTE_RELAY_DIRECTORY", "REMOTE_ASSIST_LOG", "RAT_E2E_DEFAULT_RELAY"}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(drop, func(d string) bool { return strings.EqualFold(d, k) }) {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+home, "USERPROFILE="+home, "TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp, "REMOTE_RELAY_DIRECTORY=off")
	return append(env, extra...)
}

// e2eNode 是本机的一台 relay。
type e2eNode struct {
	prefix, name, addr string
	dir                string // 证书、状态目录、黑名单文件、家目录都在这里
	fp                 string // TLS 证书指纹
	out                *lockedBuffer
}

// newE2ENode 给节点生成自签证书、算出指纹，还不启动。
func newE2ENode(t *testing.T, relayBin, root, prefix string, port int) *e2eNode {
	t.Helper()
	n := &e2eNode{
		prefix: prefix,
		name:   "e2e-" + strings.ToLower(prefix),
		addr:   fmt.Sprintf("127.0.0.1:%d", port),
		dir:    filepath.Join(root, "relay-"+prefix),
	}
	if err := os.MkdirAll(filepath.Join(n.dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	certs := filepath.Join(n.dir, "certs")
	if out, err := exec.Command(relayBin, "--gen-certs", "--certs-dir", certs).CombinedOutput(); err != nil {
		t.Fatalf("gen-certs: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(certs, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("%s 不是 PEM", filepath.Join(certs, "server.crt"))
	}
	n.fp = directory.CertFingerprint(block.Bytes)
	return n
}

func (n *e2eNode) cachePath() string { return filepath.Join(n.dir, "state", "directory.json") }

// start 启动 relay，等它开始监听才返回。nodeMode 时开节点模式（拉取地址连不上，只用事先写进状态目录的列表），
// 还要等它在列表里认出自己——认不出说明列表没按 relay 的内置根验过。
func (n *e2eNode) start(t *testing.T, relayBin string, nodeMode bool) {
	t.Helper()
	args := []string{"--listen", n.addr,
		"--cert", filepath.Join(n.dir, "certs", "server.crt"),
		"--key", filepath.Join(n.dir, "certs", "server.key"),
		"--stun", "",
		"--state-dir", filepath.Join(n.dir, "state"),
		"--blocklist-db", filepath.Join(n.dir, "blocklist.json"),
		"--blocklist-threshold", "1",
	}
	if nodeMode {
		args = append(args, "--directory-url", deadDirectoryURL)
	}
	cmd := exec.Command(relayBin, args...)
	cmd.Env = isolatedEnv(t, n.dir)
	n.out = &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = n.out, n.out
	if err := cmd.Start(); err != nil {
		t.Fatalf("relay %s start: %v", n.prefix, err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	want := []string{"Server starting on"}
	if nodeMode {
		want = append(want, fmt.Sprintf("this relay is node %s %q", n.prefix, n.name))
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		out := n.out.String()
		if !slices.ContainsFunc(want, func(w string) bool { return !strings.Contains(out, w) }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay %s 没有就绪（等 %q）:\n%s", n.prefix, want, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertNotBanned 黑名单阈值是 1：正常的选节点、竞速、注册、加入只要被计一分，127.0.0.1 就会被封。
func (n *e2eNode) assertNotBanned(t *testing.T) {
	t.Helper()
	if out := n.out.String(); strings.Contains(out, "auto-banned") {
		t.Errorf("relay %s 封了本机地址（正常使用不该被计分）:\n%s", n.prefix, out)
	}
}

// newE2EFixture 返回以现在为准的测试根夹具（dirtest 默认的「现在」是固定日期，真二进制按真实时钟验）。
func newE2EFixture(t *testing.T) *dirtest.Fixture {
	t.Helper()
	f := dirtest.New(t)
	f.Now = time.Now()
	f.StatusCert = f.IssueStatus(1, f.Status, f.Primary, directory.SignerPrimary)
	return f
}

// signE2EList 用测试根签一份列表：nodes 里第一个是默认节点，地址只有 TCP；status 里没写的节点是 active。
func signE2EList(t *testing.T, f *dirtest.Fixture, policy string, nodes []*e2eNode, status map[string]string) []byte {
	t.Helper()
	t.Helper()
	var entries []directory.NodeEntry
	for _, n := range nodes {
		c := f.Cert(n.prefix, func(c *directory.NodeCert) {
			c.Name, c.Operator = n.name, "e2e"
			c.TCP, c.QUIC = []string{n.addr}, nil
			c.TLSFPs = []string{n.fp}
		})
		st := status[n.prefix]
		if st == "" {
			st = directory.StatusActive
		}
		entries = append(entries, directory.NodeEntry{Cert: f.SignCert(c), Status: st, Weight: 100})
	}
	b := f.Body(entries...)
	b.Policy = policy
	return f.Sign(b)
}

// clientCache 是家目录 home 下客户端的列表缓存。
func clientCache(home string) string { return filepath.Join(home, ".remote_assist_relays.json") }

// seedCache 把列表写进缓存文件（relay 的 <state>/directory.json 或客户端的 ~/.remote_assist_relays.json）。
func seedCache(t *testing.T, f *dirtest.Fixture, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	err := directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
		_, changed, err := c.Offer(raw, f.Roots, time.Now())
		return changed, err
	})
	if err != nil {
		t.Fatalf("写列表缓存 %s: %v", path, err)
	}
}

// cachedList 读缓存里能用的列表；没有时返回 nil。
func cachedList(t *testing.T, f *dirtest.Fixture, path string) *directory.Verified {
	t.Helper()
	c, err := directory.LoadCache(path)
	if err != nil {
		return nil
	}
	v, err := c.Current(f.Roots, time.Now())
	if err != nil {
		return nil
	}
	return v
}

// assistCodeRE 匹配协助信息里的「协助码: XXXX-XXXX-XXXX」，分组数不限（旧版 relay 的码可能更短）。
var assistCodeRE = regexp.MustCompile(`协助码[:：]?\s*([0-9A-Za-z]{4}(?:-[0-9A-Za-z]{1,4})*)`)

// relayLineRE 匹配协助信息里的「中转服务: <host:port> (<说明>)」。
var relayLineRE = regexp.MustCompile(`中转服务: (\S+) \(([^)]*)\)`)

// e2eShare 是一个跑着的 share。
type e2eShare struct {
	root string // 共享出去的目录，里面有 hello.txt
	out  *lockedBuffer
	code string // 去掉分隔符的协助码
}

// relayLine 返回协助信息里「中转服务」一行的地址与说明。
func (s *e2eShare) relayLine(t *testing.T) (addr, desc string) {
	t.Helper()
	m := relayLineRE.FindStringSubmatch(s.out.String())
	if m == nil {
		t.Fatalf("share 输出里没有「中转服务」一行:\n%s", s.out.String())
	}
	return m[1], m[2]
}

// startE2EShare 在家目录 home 下起 share（共享 home/shared，--p2p disabled），等它打出协助码。
func startE2EShare(t *testing.T, bin, home string, env []string, args ...string) *e2eShare {
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
	deadline := time.Now().Add(20 * time.Second)
	for {
		if m := assistCodeRE.FindStringSubmatch(s.out.String()); m != nil {
			s.code = strings.ReplaceAll(m[1], "-", "")
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("share 没有打出协助码:\n%s", s.out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startE2EHelp 起 help --mcp-stdio（--p2p disabled）并完成 initialize。
func startE2EHelp(t *testing.T, bin string, env []string, args ...string) *mcpSession {
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
	s.call(1, "initialize", map[string]any{}, 10*time.Second)
	return s
}

// readHello 反复调 read_file 读 share 目录里的 hello.txt，直到读到内容或超时（带 --code 的 help 在后台连，刚起来时可能还没连上）。
func readHello(t *testing.T, s *mcpSession, root string) (ok bool, last string) {
	t.Helper()
	args := map[string]any{"name": "read_file", "arguments": map[string]any{"path": filepath.Join(root, "hello.txt")}}
	deadline := time.Now().Add(25 * time.Second)
	for id := 100; ; id++ {
		res, errMsg := s.call(id, "tools/call", args, 20*time.Second)
		last = res + errMsg
		if strings.Contains(res, "world") || strings.Contains(res, "d29ybGQ=") {
			return true, last
		}
		if time.Now().After(deadline) {
			return false, last
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// shareNoCandidates 起 share 并等它明确报告「没有可用节点」：用于断言排空节点不能接新 share。
func shareNoCandidates(t *testing.T, bin, home string, env []string, args ...string) (bool, string) {
	t.Helper()
	out := &lockedBuffer{}
	cmd := exec.Command(bin, append([]string{"share", "--root", filepath.Join(home, "shared"), "--p2p", "disabled"}, args...)...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("share start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	deadline := time.Now().Add(15 * time.Second)
	for {
		text := out.String()
		if strings.Contains(text, "没有可用的节点") {
			return true, text
		}
		if time.Now().After(deadline) {
			return false, text
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertLogRedacted 进程在自己临时目录下写的日志文件（spec 6.8）里不能有协助码的任何 4 字符片段（spec 第 11 节）。
// 全是数字的片段不查，时间戳里难免撞上。printed：进程打印过协助码（share 的协助信息），文件里应能看到脱敏后的 ********。
func assertLogRedacted(t *testing.T, home, mode, code string, printed bool) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(home, "tmp", "remote-assist-*", mode+"-*.log"))
	if len(files) == 0 {
		t.Fatalf("%s 的临时目录里没有 %s 的日志文件", home, mode)
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if printed && !strings.Contains(text, "********") {
			t.Errorf("%s 里没有脱敏后的协助码:\n%s", p, text)
		}
		for i := 0; i+4 <= len(code); i++ {
			g := code[i : i+4]
			if strings.Trim(g, "0123456789") != "" && strings.Contains(text, g) {
				t.Errorf("%s 里出现了协助码的片段 %q:\n%s", p, g, text)
			}
		}
	}
}

// TestMultiRelayShareAutoAndHelpByCode 默认节点 A 在排空：自动模式的 share 改选 B，码以 B 开头，「中转服务」写 B 的列表地址、
// 说明证书已按节点列表校验；只给协助码的 help 按码的首位找到 B 并跑通 read_file（连默认节点 A 是加入不了的）。
// share 的日志文件里没有协助码。
func TestMultiRelayShareAutoAndHelpByCode(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18471)
	b := newE2ENode(t, relayBin, root, "B", 18472)
	raw := signE2EList(t, f, directory.PolicyPreferDefault, []*e2eNode{a, b}, map[string]string{"A": directory.StatusDraining})
	for _, n := range []*e2eNode{a, b} {
		seedCache(t, f, n.cachePath(), raw)
		n.start(t, relayBin, true)
	}
	shareHome, helpHome := filepath.Join(root, "share-home"), filepath.Join(root, "help-home")
	seedCache(t, f, clientCache(shareHome), raw)
	seedCache(t, f, clientCache(helpHome), raw)

	sh := startE2EShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	if !strings.HasPrefix(sh.code, "B") {
		t.Fatalf("默认节点 A 在排空，share 应改选 B，码却是 %s:\n%s", sh.code, sh.out.String())
	}
	if addr, desc := sh.relayLine(t); addr != b.addr || desc != "TLS，证书已按节点列表校验" {
		t.Fatalf("「中转服务」应是 %s（证书已按节点列表校验），得到 %s（%s）", b.addr, addr, desc)
	}

	s := startE2EHelp(t, remoteBin, isolatedEnv(t, helpHome, "RAT_E2E_DEFAULT_RELAY="+a.addr), "--code", sh.code)
	if ok, last := readHello(t, s, sh.root); !ok {
		t.Fatalf("只给协助码的 help 没连上 B: %s\nhelp stderr:\n%s\nshare:\n%s", last, s.stderr.String(), sh.out.String())
	}
	if want := "协助码属于节点 " + b.name + "（B）"; !strings.Contains(s.stderr.String(), want) {
		t.Errorf("help 应提示 %q，stderr:\n%s", want, s.stderr.String())
	}

	// A 在排空：正在排空的节点不能接新 share（nodepick.eligibleForShare 只认 active）。
	// 把 B 也改成排空后另起一个实例，两个节点都不能用；若实现误把 draining 当可选，这里会注册成功。
	allDraining := signE2EList(t, f, directory.PolicyPreferDefault, []*e2eNode{a, b},
		map[string]string{"A": directory.StatusDraining, "B": directory.StatusDraining})
	drainHome := filepath.Join(root, "drain-home")
	if err := os.MkdirAll(drainHome, 0o700); err != nil {
		t.Fatal(err)
	}
	seedCache(t, f, clientCache(drainHome), allDraining)
	if ok, last := shareNoCandidates(t, remoteBin, drainHome, isolatedEnv(t, drainHome, "RAT_E2E_DEFAULT_RELAY="+a.addr), "--new-instance"); !ok {
		t.Errorf("A、B 都在排空时 share 不应注册成功: %s", last)
	}

	assertLogRedacted(t, shareHome, "share", sh.code, true)
	assertLogRedacted(t, helpHome, "help", sh.code, false)
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}

// TestMultiRelayCertMismatchSkipped 默认节点 A 的证书与列表里的指纹对不上（换了证书没更新列表，或者地址被人顶替）：
// share 拒绝 A、改用 B，提示证书不符；help 只给码照样连上 B。A 那边只是握手被中止，不计分。
func TestMultiRelayCertMismatchSkipped(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18485)
	b := newE2ENode(t, relayBin, root, "B", 18486)
	a.fp = dirtest.Fingerprint("not-the-cert-of-A")
	raw := signE2EList(t, f, directory.PolicyPreferDefault, []*e2eNode{a, b}, nil)
	a.start(t, relayBin, false) // 列表里没有它的证书，开节点模式也认不出自己
	seedCache(t, f, b.cachePath(), raw)
	b.start(t, relayBin, true)
	shareHome, helpHome := filepath.Join(root, "share-home"), filepath.Join(root, "help-home")
	seedCache(t, f, clientCache(shareHome), raw)
	seedCache(t, f, clientCache(helpHome), raw)

	sh := startE2EShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	if addr, desc := sh.relayLine(t); addr != b.addr || desc != "TLS，证书已按节点列表校验" || !strings.HasPrefix(sh.code, "B") {
		t.Fatalf("A 的证书与列表不符，share 应在 B 上，中转服务却是 %s（%s）、码 %s:\n%s", addr, desc, sh.code, sh.out.String())
	}
	if want := "节点 " + a.name + "（A） 的地址 " + a.addr + " 出示的证书与节点列表不符"; !strings.Contains(sh.out.String(), want) {
		t.Errorf("share 应提示 %q:\n%s", want, sh.out.String())
	}

	s := startE2EHelp(t, remoteBin, isolatedEnv(t, helpHome, "RAT_E2E_DEFAULT_RELAY="+a.addr), "--code", sh.code)
	if ok, last := readHello(t, s, sh.root); !ok {
		t.Fatalf("只给协助码的 help 没连上 B: %s\nhelp stderr:\n%s", last, s.stderr.String())
	}
	if !waitFor(a.out, "New connection from", 5*time.Second) {
		t.Errorf("share 应先试默认节点 A，A 却没收到连接:\n%s", a.out.String())
	}
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}

// TestMultiRelayBootstrapFromDefaultNode 新装的机器：没有缓存、CF 不通。share、help 都按内置默认地址连默认节点 A（TOFU），
// A 随注册、加入的响应把列表转发过来、写进各自的缓存；码以 A 开头，help 只给码照样连上。
func TestMultiRelayBootstrapFromDefaultNode(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18473)
	b := newE2ENode(t, relayBin, root, "B", 18474)
	raw := signE2EList(t, f, directory.PolicyPreferDefault, []*e2eNode{a, b}, nil)
	for _, n := range []*e2eNode{a, b} {
		seedCache(t, f, n.cachePath(), raw)
		n.start(t, relayBin, true)
	}
	shareHome, helpHome := filepath.Join(root, "share-home"), filepath.Join(root, "help-home")

	sh := startE2EShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	if !strings.HasPrefix(sh.code, "A") {
		t.Fatalf("没有列表时 share 应在默认节点 A 上，码却是 %s:\n%s", sh.code, sh.out.String())
	}
	if !waitFor(sh.out, "已获取节点列表（来自 relay "+a.addr+"）", 10*time.Second) {
		t.Errorf("share 没有提示从 A 拿到列表:\n%s", sh.out.String())
	}
	if cachedList(t, f, clientCache(shareHome)) == nil {
		t.Errorf("share 的缓存里没有 A 转发的列表")
	}

	s := startE2EHelp(t, remoteBin, isolatedEnv(t, helpHome, "RAT_E2E_DEFAULT_RELAY="+a.addr), "--code", sh.code)
	if ok, last := readHello(t, s, sh.root); !ok {
		t.Fatalf("没有列表的 help 没连上默认节点 A: %s\nhelp stderr:\n%s", last, s.stderr.String())
	}
	if cachedList(t, f, clientCache(helpHome)) == nil {
		t.Errorf("help 的缓存里没有 A 转发的列表，stderr:\n%s", s.stderr.String())
	}
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}

// TestMultiRelayFastestRaceNotScored fastest 策略：share 同时拨 A、B，先握手完的胜出，另一路关掉。落选那一路不能被计分
// （握手中止、握手后零消息断开都是 0 分，spec 6.9），黑名单阈值是 1，计了分就会封掉 127.0.0.1。help 只给码照样连上。
func TestMultiRelayFastestRaceNotScored(t *testing.T) {
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18475)
	b := newE2ENode(t, relayBin, root, "B", 18476)
	nodes := []*e2eNode{a, b}
	raw := signE2EList(t, f, directory.PolicyFastest, nodes, nil)
	for _, n := range nodes {
		seedCache(t, f, n.cachePath(), raw)
		n.start(t, relayBin, true)
	}
	shareHome, helpHome := filepath.Join(root, "share-home"), filepath.Join(root, "help-home")
	seedCache(t, f, clientCache(shareHome), raw)
	seedCache(t, f, clientCache(helpHome), raw)

	sh := startE2EShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	addr, _ := sh.relayLine(t)
	i := slices.IndexFunc(nodes, func(n *e2eNode) bool { return n.addr == addr })
	if i < 0 || !strings.HasPrefix(sh.code, nodes[i].prefix) {
		t.Fatalf("share 应在 A 或 B 上、码以所在节点的前缀开头，中转服务 %s、码 %s:\n%s", addr, sh.code, sh.out.String())
	}

	s := startE2EHelp(t, remoteBin, isolatedEnv(t, helpHome, "RAT_E2E_DEFAULT_RELAY="+a.addr), "--code", sh.code)
	if ok, last := readHello(t, s, sh.root); !ok {
		t.Fatalf("只给协助码的 help 没连上 %s: %s\nhelp stderr:\n%s", nodes[i].prefix, last, s.stderr.String())
	}
	// 两台都要真的被拨过，「落选那一路不计分」才有意义。
	for _, n := range nodes {
		if !waitFor(n.out, "New connection from", 5*time.Second) {
			t.Errorf("fastest 应同时拨 A、B，%s 却没收到连接:\n%s", n.prefix, n.out.String())
		}
	}
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}

// TestMultiRelayReleasedShareOnNode 已发布的 share（不认识节点列表）用 --server 连非默认节点 B：节点模式的 relay 照样发以 B
// 开头的码，新版 help 只给码就能按首位找到 B（spec 第 10 节「旧 share + 新 help」）。
func TestMultiRelayReleasedShareOnNode(t *testing.T) {
	oldCLI := envBin(t, "RAT_RELEASED_CLI")
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18477)
	b := newE2ENode(t, relayBin, root, "B", 18478)
	raw := signE2EList(t, f, directory.PolicyPreferDefault, []*e2eNode{a, b}, nil)
	for _, n := range []*e2eNode{a, b} {
		seedCache(t, f, n.cachePath(), raw)
		n.start(t, relayBin, true)
	}
	shareHome, helpHome := filepath.Join(root, "share-home"), filepath.Join(root, "help-home")
	seedCache(t, f, clientCache(helpHome), raw)

	sh := startE2EShare(t, oldCLI, shareHome, isolatedEnv(t, shareHome), "--server", b.addr, "--insecure")
	if !strings.HasPrefix(sh.code, "B") {
		t.Fatalf("节点 B 发给旧 share 的码应以 B 开头，得到 %s:\n%s", sh.code, sh.out.String())
	}

	s := startE2EHelp(t, remoteBin, isolatedEnv(t, helpHome, "RAT_E2E_DEFAULT_RELAY="+a.addr), "--code", sh.code)
	if ok, last := readHello(t, s, sh.root); !ok {
		t.Fatalf("新版 help 只给码没连上旧 share 所在的 B: %s\nhelp stderr:\n%s\nshare:\n%s", last, s.stderr.String(), sh.out.String())
	}
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}

// TestMultiRelayReleasedHelpFollowsAutoShare 新版 share 自动选到 B（默认节点 A 在排空），已发布的 help 照协助信息里的「中转服务」
// 用 --server 连过去（spec 第 10 节「新 share（自动）+ 旧 help，走 MCP」）。
func TestMultiRelayReleasedHelpFollowsAutoShare(t *testing.T) {
	oldCLI := envBin(t, "RAT_RELEASED_CLI")
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18479)
	b := newE2ENode(t, relayBin, root, "B", 18480)
	raw := signE2EList(t, f, directory.PolicyPreferDefault, []*e2eNode{a, b}, map[string]string{"A": directory.StatusDraining})
	for _, n := range []*e2eNode{a, b} {
		seedCache(t, f, n.cachePath(), raw)
		n.start(t, relayBin, true)
	}
	shareHome, helpHome := filepath.Join(root, "share-home"), filepath.Join(root, "help-home")
	seedCache(t, f, clientCache(shareHome), raw)

	sh := startE2EShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	addr, _ := sh.relayLine(t)
	if addr != b.addr {
		t.Fatalf("默认节点 A 在排空，share 应在 B 上，中转服务却是 %s:\n%s", addr, sh.out.String())
	}

	s := startE2EHelp(t, oldCLI, isolatedEnv(t, helpHome), "--server", addr, "--insecure", "--code", sh.code)
	if ok, last := readHello(t, s, sh.root); !ok {
		t.Fatalf("旧版 help 照「中转服务」没连上: %s\nhelp stderr:\n%s\nshare:\n%s", last, s.stderr.String(), sh.out.String())
	}
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}

// TestMultiRelayNewClientsOnDeployedRelay 节点还跑着线上版本的 relay（有黑名单、没有节点模式）时，列表照样能把它们列成节点：
// 新版 share 自动模式（fastest）同时拨两台、按列表严格校验证书，注册在胜出的那台；新版 help 按「中转服务」显式连过去。
// 两台都没封本机（阈值 1）：新客户端不向旧 relay 发任何新的连接前消息，竞速落选的连接也不计分（spec 6.9）。
// 旧 relay 不发前缀码，所以 help 写地址：只给码的话首位是随机的，找不到对应节点时只能连默认节点。
func TestMultiRelayNewClientsOnDeployedRelay(t *testing.T) {
	deployed := envBin(t, "RAT_DEPLOYED_RELAY")
	remoteBin, relayBin := taggedBins(t)
	root := t.TempDir()
	f := newE2EFixture(t)
	a := newE2ENode(t, relayBin, root, "A", 18481)
	b := newE2ENode(t, relayBin, root, "B", 18482)
	nodes := []*e2eNode{a, b}
	raw := signE2EList(t, f, directory.PolicyFastest, nodes, nil)
	for _, n := range nodes {
		n.start(t, deployed, false)
	}
	shareHome, helpHome := filepath.Join(root, "share-home"), filepath.Join(root, "help-home")
	seedCache(t, f, clientCache(shareHome), raw)
	seedCache(t, f, clientCache(helpHome), raw)

	sh := startE2EShare(t, remoteBin, shareHome, isolatedEnv(t, shareHome, "RAT_E2E_DEFAULT_RELAY="+a.addr))
	addr, desc := sh.relayLine(t)
	if !slices.ContainsFunc(nodes, func(n *e2eNode) bool { return n.addr == addr }) || desc != "TLS，证书已按节点列表校验" {
		t.Fatalf("share 应在列表节点 A 或 B 上（证书已按节点列表校验），中转服务却是 %s（%s）:\n%s", addr, desc, sh.out.String())
	}

	s := startE2EHelp(t, remoteBin, isolatedEnv(t, helpHome, "RAT_E2E_DEFAULT_RELAY="+a.addr), "--server", addr, "--code", sh.code)
	if ok, last := readHello(t, s, sh.root); !ok {
		t.Fatalf("新版 help 显式连旧 relay 没跑通: %s\nhelp stderr:\n%s\nshare:\n%s", last, s.stderr.String(), sh.out.String())
	}
	for _, n := range nodes {
		if !waitFor(n.out, "New connection from", 5*time.Second) {
			t.Errorf("fastest 应同时拨 A、B，%s 却没收到连接:\n%s", n.prefix, n.out.String())
		}
	}
	a.assertNotBanned(t)
	b.assertNotBanned(t)
}

// TestMultiRelayBanCheckIsLive 对照组：上面那些「没封本机」的断言是有效的。没有成功注册过的新 relay 上，
// 一条未知的连接前消息就够把 127.0.0.1 封掉（新版计 1 分，线上版本按「注册前的消息」计 2 分），日志里出现 auto-banned。
// 新客户端要是向旧 relay 发了新的连接前消息，TestMultiRelayNewClientsOnDeployedRelay 就是这样失败的。
func TestMultiRelayBanCheckIsLive(t *testing.T) {
	_, relayBin := taggedBins(t)
	bins := map[string]string{"tagged": relayBin}
	if p := os.Getenv("RAT_DEPLOYED_RELAY"); p != "" {
		bins["deployed"] = p
	}
	port := 18483
	for name, bin := range bins {
		n := newE2ENode(t, bin, t.TempDir(), "A", port)
		port++
		t.Run(name, func(t *testing.T) {
			n.start(t, bin, false)
			conn, err := tls.Dial("tcp", n.addr, &tls.Config{InsecureSkipVerify: true})
			if err != nil {
				t.Fatalf("连 relay: %v", err)
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, `{"type":"e2e_unknown_probe"}`+"\n"); err != nil {
				t.Fatalf("发未知消息: %v", err)
			}
			if !waitFor(n.out, "auto-banned", 10*time.Second) {
				t.Fatalf("未知的连接前消息没有让 relay 封掉本机，「没封本机」的断言就测不出问题:\n%s", n.out.String())
			}
		})
	}
}
