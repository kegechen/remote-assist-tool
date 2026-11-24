package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// dirEnv 是节点列表管理器的测试环境：夹具签的列表、一个假的 CF（TLS 测试服务）、可调的时钟、收集到的提示。
type dirEnv struct {
	t    *testing.T
	f    *dirtest.Fixture
	ts   *httptest.Server
	path string

	mu   sync.Mutex
	list []byte        // CF 当前发布的列表；nil 时返回 503
	hits int           // CF 收到的请求数
	gate chan struct{} // 非 nil 时请求挂住，直到它被关闭
	now  time.Time
	logs []string
}

func newDirEnv(t *testing.T) *dirEnv {
	t.Helper()
	e := &dirEnv{t: t, f: dirtest.New(t), path: filepath.Join(t.TempDir(), directoryCacheFile), now: dirtest.Epoch}
	e.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hits++
		list, gate := e.list, e.gate
		e.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if r.URL.Path != "/v1/directory" || list == nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(list)
	}))
	t.Cleanup(e.ts.Close)
	return e
}

// manager 返回指向这个环境的管理器；每调一次相当于一个新进程（共用同一个缓存文件）。
func (e *dirEnv) manager() *dirManager {
	return &dirManager{
		path:  e.path,
		url:   e.ts.URL + "/v1/directory",
		roots: e.f.Roots,
		http:  e.ts.Client(),
		now:   e.clock,
		logf:  e.logf,
		every: directoryRefreshEvery,
	}
}

func (e *dirEnv) host() string {
	u, err := url.Parse(e.ts.URL)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.Host
}

func (e *dirEnv) serve(list []byte) {
	e.mu.Lock()
	e.list = list
	e.mu.Unlock()
}

// hold 让 CF 收到的请求挂住，直到调用返回的 release。测试中途失败时由 Cleanup 放行（排在 ts.Close 之前），
// 否则 ts.Close 会一直等挂住的请求，失败的测试变成卡到超时。
func (e *dirEnv) hold() (release func()) {
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	e.mu.Lock()
	e.gate = gate
	e.mu.Unlock()
	e.t.Cleanup(release)
	return release
}

func (e *dirEnv) hitCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hits
}

func (e *dirEnv) setNow(now time.Time) {
	e.mu.Lock()
	e.now = now
	e.mu.Unlock()
}

func (e *dirEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *dirEnv) logf(format string, args ...any) {
	e.mu.Lock()
	e.logs = append(e.logs, fmt.Sprintf(format, args...))
	e.mu.Unlock()
}

// logsWith 返回包含全部 parts 的提示行。
func (e *dirEnv) logsWith(parts ...string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
next:
	for _, l := range e.logs {
		for _, p := range parts {
			if !strings.Contains(l, p) {
				continue next
			}
		}
		out = append(out, l)
	}
	return out
}

func (e *dirEnv) logCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.logs)
}

// wantLogs 断言恰好有 n 行提示包含全部 parts。
func (e *dirEnv) wantLogs(n int, parts ...string) {
	e.t.Helper()
	if got := e.logsWith(parts...); len(got) != n {
		e.mu.Lock()
		all := strings.Join(e.logs, "\n  ")
		e.mu.Unlock()
		e.t.Fatalf("%d log lines contain %q, want %d; all lines:\n  %s", len(got), parts, n, all)
	}
}

func (e *dirEnv) entry(prefix, status string) directory.NodeEntry {
	en := e.f.Entry(prefix)
	en.Status = status
	return en
}

// sign 签一份序号为 serial 的列表，第一项是默认节点。
func (e *dirEnv) sign(serial uint64, nodes ...directory.NodeEntry) []byte {
	b := e.f.Body(nodes...)
	b.Serial = serial
	return e.f.Sign(b)
}

func (e *dirEnv) readCache() []byte {
	e.t.Helper()
	data, err := os.ReadFile(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	return data
}

// within5s 在另一个 goroutine 里调 call，最多等 5 秒：它若不守自己的 ctx 而一直等着挂住的请求，测试要失败，而不是卡到超时。
func within5s(t *testing.T, what string, call func() error) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- call() }()
	return recv5s(t, what, ch)
}

// recv5s 从 ch 收一个值，最多等 5 秒。失败时测试结束，Cleanup 放行挂住的请求，不会卡到 go test 超时。
func recv5s[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return within 5s", what)
		var zero T
		return zero
	}
}

// ctx5s 返回 5 秒后到期的 ctx。Refresh 守调用方的 ctx：回归成请求永远不结束时，测试拿到 DeadlineExceeded 失败，
// 而不是卡到 go test 超时。
func ctx5s(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDirManagerRefreshStoresList(t *testing.T) {
	e := newDirEnv(t)
	e.serve(e.sign(42, e.entry("A", directory.StatusActive), e.entry("B", directory.StatusActive)))
	m := e.manager()
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	v, c := m.Current()
	if v == nil || v.Serial != 42 || len(v.Nodes) != 2 || c == nil {
		t.Fatalf("Current = %+v, %v; want list 42 with 2 nodes and its cache", v, c)
	}
	if s, k := m.Position(); s != 42 || k != 1 {
		t.Fatalf("Position = (%d, %d), want (42, 1)", s, k)
	}
	// 来源写主机名，不写完整地址。
	e.wantLogs(1, "[relay]", "已获取节点列表（来自 "+e.host()+"）", "序号 42", "启用 2 个节点")

	// 写进了缓存文件：另一个进程读得到。
	if v2, _ := e.manager().Current(); v2 == nil || v2.Serial != 42 {
		t.Fatalf("another process sees %+v, want list 42", v2)
	}
}

func TestDirManagerRefreshLogsUpdateOnlyWhenChanged(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	e.serve(e.sign(42, e.entry("A", directory.StatusActive), e.entry("B", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.serve(e.sign(43, e.entry("A", directory.StatusActive), e.entry("B", directory.StatusDraining)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.wantLogs(1, "[relay]", "节点列表已更新", e.host(), "序号 42 → 43", "启用 1 个节点")
	n := e.logCount()
	if err := m.Refresh(ctx5s(t)); err != nil { // 同一份列表
		t.Fatal(err)
	}
	if e.logCount() != n {
		t.Fatalf("refreshing the same list printed %q", e.logsWith(""))
	}
	if v, _ := m.Current(); v == nil || v.Serial != 43 {
		t.Fatalf("Current = %+v, want list 43", v)
	}
}

// 换了状态密钥：列表序号从头算，提示里要看得出是换了密钥，而不是序号倒退。
func TestDirManagerLogsStatusKeyChange(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	// 新密钥签的第一份列表碰巧与旧列表同序号：位置照样变了，也要提示。
	next := dirtest.SeedKey("status-2")
	e.f.StatusCert = e.f.IssueStatus(2, next, e.f.Primary, directory.SignerPrimary)
	e.f.Status = next
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.wantLogs(1, "节点列表已更新", "状态密钥授权 1 → 2", "序号 42 → 42")

	next = dirtest.SeedKey("status-3")
	e.f.StatusCert = e.f.IssueStatus(3, next, e.f.Primary, directory.SignerPrimary)
	e.f.Status = next
	e.serve(e.sign(1, e.entry("A", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.wantLogs(1, "节点列表已更新", "状态密钥授权 2 → 3", "序号 42 → 1")
	if s, k := m.Position(); s != 1 || k != 3 {
		t.Fatalf("Position = (%d, %d), want (1, 3)", s, k)
	}
}

func TestDirManagerLogsSkippedNodes(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	expired := e.f.Cert("B", func(c *directory.NodeCert) {
		c.NotBefore = dirtest.Epoch.Add(-30 * 24 * time.Hour).Unix()
		c.NotAfter = dirtest.Epoch.Add(-48 * time.Hour).Unix()
	})
	bad := directory.NodeEntry{Cert: e.f.SignCert(expired), Status: directory.StatusActive, Weight: 100}
	e.serve(e.sign(42, e.entry("A", directory.StatusActive), bad))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.wantLogs(1, "[relay]", `"B"`, "已丢弃", "expired")
	e.wantLogs(1, "已获取节点列表", "启用 1 个节点")

	// 列表更新了，B 还是同样被丢弃：不再提示（Worker 重签一次就打一行，长期留在列表里的坏节点会刷屏）。
	e.serve(e.sign(43, e.entry("A", directory.StatusActive), bad))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.wantLogs(1, "已丢弃")
	// B 修好了，之后又坏了：再提示。
	e.serve(e.sign(44, e.entry("A", directory.StatusActive), e.entry("B", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.serve(e.sign(45, e.entry("A", directory.StatusActive), bad))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.wantLogs(2, "已丢弃")
	// 同一张证书（前缀、序号都没变），丢弃的原因变了：再提示。
	forged := directory.NodeEntry{Cert: e.f.SignCertWith(expired, dirtest.SeedKey("rogue"), directory.SignerPrimary), Status: directory.StatusActive, Weight: 100}
	e.serve(e.sign(46, e.entry("A", directory.StatusActive), forged))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.wantLogs(3, "已丢弃")
	e.wantLogs(1, "已丢弃", directory.ErrBadSignature.Error())
	e.wantLogs(4, "节点列表已更新") // 列表更新本身照常每次提示
}

// 同一张证书在列表里出现两次、丢弃的原因不同（Worker 出错时会这样）：去重按（节点, 原因）算，
// 两种原因不能互相顶掉、每次列表更新都再提示一遍。读不出前缀的坏项都算同一个节点：多出一项就提示多出来的那项。
func TestDirManagerSkippedNodesDedupPerReason(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	expired := e.f.Cert("B", func(c *directory.NodeCert) {
		c.NotBefore = dirtest.Epoch.Add(-30 * 24 * time.Hour).Unix()
		c.NotAfter = dirtest.Epoch.Add(-48 * time.Hour).Unix()
	})
	stale := directory.NodeEntry{Cert: e.f.SignCert(expired), Status: directory.StatusActive, Weight: 100}
	forged := directory.NodeEntry{Cert: e.f.SignCertWith(expired, dirtest.SeedKey("rogue"), directory.SignerPrimary), Status: directory.StatusActive, Weight: 100}
	refresh := func(serial uint64, nodes ...directory.NodeEntry) {
		t.Helper()
		e.serve(e.sign(serial, append([]directory.NodeEntry{e.entry("A", directory.StatusActive)}, nodes...)...))
		if err := m.Refresh(ctx5s(t)); err != nil {
			t.Fatal(err)
		}
	}
	for serial := uint64(42); serial < 46; serial++ {
		refresh(serial, forged, stale)
	}
	e.wantLogs(2, "已丢弃")
	e.wantLogs(1, "已丢弃", directory.ErrBadSignature.Error())
	e.wantLogs(1, "已丢弃", directory.ErrExpired.Error())

	unreadable := directory.NodeEntry{Cert: directory.Envelope{Kind: directory.KindNodeCert, Payload: []byte("[]")}, Status: directory.StatusActive, Weight: 100}
	refresh(46, unreadable)
	e.wantLogs(3, "已丢弃")
	refresh(47, unreadable, unreadable)
	e.wantLogs(4, "已丢弃")
	refresh(48, unreadable, unreadable)
	e.wantLogs(4, "已丢弃")
}

func TestDirManagerRejectsBadListKeepsCache(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	before := e.readCache()
	b := e.f.Body(e.entry("A", directory.StatusActive))
	b.Serial = 43
	rogue, err := directory.SignDirectory(dirtest.SeedKey("rogue"), b) // 没有授权的状态密钥
	if err != nil {
		t.Fatal(err)
	}
	e.serve(rogue)
	if err := m.Refresh(ctx5s(t)); !errors.Is(err, directory.ErrBadSignature) {
		t.Fatalf("Refresh = %v, want ErrBadSignature", err)
	}
	e.wantLogs(1, "[relay]", e.host(), "验签失败", "已丢弃", directory.LayerDirectory)
	if v, _ := m.Current(); v == nil || v.Serial != 42 {
		t.Fatalf("Current = %+v, want the cached list 42", v)
	}
	if !bytes.Equal(e.readCache(), before) {
		t.Fatal("a rejected list changed the cache file")
	}
}

// CF 连不上只在开始失败时提示一次，恢复之后再失败才再提示：share 每 6 小时、MCP 每次连接都可能取一次，
// CF 长期不通时不能每次都刷一行。
func TestDirManagerFetchFailureLoggedOnTransition(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	for range 2 {
		if err := m.Refresh(ctx5s(t)); err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("Refresh = %v, want an HTTP 503 error", err)
		}
	}
	e.wantLogs(1, "[relay]", "获取节点列表失败", e.host())
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	e.serve(nil)
	if err := m.Refresh(ctx5s(t)); err == nil {
		t.Fatal("Refresh succeeded against a 503")
	}
	e.wantLogs(2, "获取节点列表失败")
}

func TestDirManagerOfferFromRelay(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	const src = "relay 192.0.2.10:8443"
	if v := m.Offer(nil, src); v != nil {
		t.Fatalf("Offer(nil) = %+v, want nil", v)
	}
	if _, err := os.Stat(e.path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Offer(nil) touched the cache file: %v", err)
	}
	if e.logCount() != 0 {
		t.Fatalf("Offer(nil) printed %q", e.logsWith(""))
	}

	raw := e.sign(42, e.entry("A", directory.StatusActive))
	if v := m.Offer(raw, src); v == nil || v.Serial != 42 {
		t.Fatalf("Offer = %+v, want list 42", v)
	}
	e.wantLogs(1, "[relay]", "已获取节点列表", src, "序号 42")
	n := e.logCount()
	if v := m.Offer(raw, src); v == nil || v.Serial != 42 { // 同一份：照样返回验过的列表，不再提示
		t.Fatalf("Offer of the same list = %+v, want list 42", v)
	}
	if e.logCount() != n {
		t.Fatalf("offering the same list printed %q", e.logsWith(""))
	}

	if v := m.Offer(e.sign(41, e.entry("A", directory.StatusActive)), src); v != nil {
		t.Fatalf("Offer of an older list = %+v, want nil", v)
	}
	e.wantLogs(1, "[relay]", src, "不比本机缓存的新")
	if v := m.Offer([]byte("not a directory"), src); v != nil {
		t.Fatalf("Offer of garbage = %+v, want nil", v)
	}
	m.Offer([]byte("not a directory"), src) // 同一个问题不再提示
	e.wantLogs(1, "[relay]", src, "验签失败")
	m.Offer(raw, src) // 恢复正常之后再出问题，再提示
	m.Offer([]byte("not a directory"), src)
	e.wantLogs(2, "验签失败")
	if v, _ := m.Current(); v == nil || v.Serial != 42 {
		t.Fatalf("Current = %+v, want list 42", v)
	}
	if e.hitCount() != 0 {
		t.Fatalf("Offer fetched from CF %d times", e.hitCount())
	}
}

// 缓存损坏：读取退回内置默认节点，拒绝更新、不覆盖，提示带路径、让人删掉它；同一个问题只提示一次。
func TestDirManagerCorruptCache(t *testing.T) {
	e := newDirEnv(t)
	garbage := []byte("{not json")
	if err := os.WriteFile(e.path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	m := e.manager()
	for range 2 {
		if v, c := m.Current(); v != nil || c == nil {
			t.Fatalf("Current = %+v, %v; want no list and an empty cache", v, c)
		}
	}
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); !errors.Is(err, directory.ErrCacheCorrupt) {
		t.Fatalf("Refresh = %v, want ErrCacheCorrupt", err)
	}
	if v := m.Offer(e.sign(43, e.entry("A", directory.StatusActive)), "relay 192.0.2.10:8443"); v != nil {
		t.Fatalf("Offer = %+v, want nil", v)
	}
	if s, k := m.Position(); s != 0 || k != 0 {
		t.Fatalf("Position = (%d, %d), want (0, 0)", s, k)
	}
	if !bytes.Equal(e.readCache(), garbage) {
		t.Fatal("the corrupt cache file was overwritten")
	}
	e.wantLogs(1, "[relay]", "已损坏", e.path, "删除")
	e.wantLogs(0, "获取节点列表失败")
	e.wantLogs(0, "验签失败")

	// 删掉之后正常；再坏一次，再提示。
	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	m.Current()
	if err := os.WriteFile(e.path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	m.Current()
	e.wantLogs(2, "已损坏")
}

// 先遇到损坏的是 relay 转发（还没读过缓存）：写缓存的路径自己也要提示；删掉之后能写进去，再坏再提示。
func TestDirManagerCorruptCacheOnOffer(t *testing.T) {
	e := newDirEnv(t)
	garbage := []byte("{not json")
	if err := os.WriteFile(e.path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	m := e.manager()
	const src = "relay 192.0.2.10:8443"
	raw := e.sign(42, e.entry("A", directory.StatusActive))
	for range 2 {
		if v := m.Offer(raw, src); v != nil {
			t.Fatalf("Offer into a corrupt cache = %+v, want nil", v)
		}
	}
	e.wantLogs(1, "[relay]", "已损坏", e.path, "删除")
	if !bytes.Equal(e.readCache(), garbage) {
		t.Fatal("the corrupt cache file was overwritten")
	}

	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	if v := m.Offer(raw, src); v == nil {
		t.Fatal("Offer after deleting the corrupt cache = nil, want the list")
	}
	if err := os.WriteFile(e.path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	m.Offer(raw, src)
	e.wantLogs(2, "已损坏")
}

// 更新的版本写的缓存：同样不用、不覆盖，但提示升级，不能让人删掉它。
func TestDirManagerNewerCache(t *testing.T) {
	e := newDirEnv(t)
	newer := []byte(`{"version":99,"future":true}`)
	if err := os.WriteFile(e.path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	m := e.manager()
	if v, _ := m.Current(); v != nil {
		t.Fatalf("Current = %+v, want nil", v)
	}
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); !errors.Is(err, directory.ErrCacheNewer) {
		t.Fatalf("Refresh = %v, want ErrCacheNewer", err)
	}
	if !bytes.Equal(e.readCache(), newer) {
		t.Fatal("a cache written by a newer version was overwritten")
	}
	e.wantLogs(1, "[relay]", "更新版本", "升级", e.path, "不要删掉这个文件")
	e.wantLogs(0, "重置") // 损坏时的提示是「删除这个文件即可重置」，这里不能出现

	// 换成另一种问题（损坏）：状态变了，要再提示。
	if err := os.WriteFile(e.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Current()
	e.wantLogs(1, "[relay]", "已损坏", "删除")
}

// CF 发的列表比本机缓存的旧（另一个进程刚经 relay 收下了更新的那份，CF 那边还没同步过来）：不采用、提示，
// 但这也算从 CF 确认过——缓存不比 CF 旧，RefreshIfStale 不必再取。
func TestDirManagerRefreshOlderList(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	if v := m.Offer(e.sign(43, e.entry("A", directory.StatusActive)), "relay 192.0.2.10:8443"); v == nil {
		t.Fatal("Offer = nil, want list 43")
	}
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	at := dirtest.Epoch.Add(5 * time.Hour) // 列表签发于 Epoch-1h
	e.setNow(at)
	if err := m.Refresh(ctx5s(t)); !errors.Is(err, directory.ErrRollback) {
		t.Fatalf("Refresh = %v, want ErrRollback", err)
	}
	e.wantLogs(1, "[relay]", "来自 "+e.host()+" ", "不比本机缓存的新")
	if v, _ := m.Current(); v == nil || v.Serial != 43 {
		t.Fatalf("Current = %+v, want the cached list 43", v)
	}
	// 两分钟后（过了重试间隔），缓存列表按签发时间已满 6 小时，但刚从 CF 确认过：不取。
	e.setNow(at.Add(2 * time.Minute))
	if err := m.RefreshIfStale(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	if n := e.hitCount(); n != 1 {
		t.Fatalf("CF got %d requests, want 1", n)
	}
}

func TestDirManagerExpiredListLoggedOnTransition(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	late := dirtest.Epoch.Add(32 * 24 * time.Hour) // 有效期 30 天，再过时钟误差 1 天
	for i := range 2 {
		e.setNow(late.Add(time.Duration(i) * time.Minute)) // 错误原文里带着当前时间，每次都不同
		if v, c := m.Current(); v != nil || c == nil {
			t.Fatalf("Current = %+v, %v after expiry; want no list and the cache", v, c)
		}
	}
	e.wantLogs(1, "[relay]", "已过期", "内置默认节点")
	e.wantLogs(0, "已失效")
	e.setNow(dirtest.Epoch)
	if v, _ := m.Current(); v == nil {
		t.Fatal("Current = nil, want the list back")
	}
	e.setNow(late)
	m.Current()
	e.wantLogs(2, "已过期")
}

// 过期以外的失效（这里是本机时钟比签发时间早了一天多）：提示「已失效」，同样只在出现时提示一次。
func TestDirManagerInvalidListLoggedOnTransition(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	e.serve(e.sign(42, e.entry("A", directory.StatusActive))) // 签发于 Epoch-1h，允许 1 天时钟误差
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	early := dirtest.Epoch.Add(-26 * time.Hour)
	for i := range 2 {
		e.setNow(early.Add(time.Duration(i) * time.Minute))
		if v, c := m.Current(); v != nil || c == nil {
			t.Fatalf("Current = %+v, %v before the list is valid; want no list and the cache", v, c)
		}
	}
	e.wantLogs(1, "[relay]", "已失效", directory.ErrNotYetValid.Error(), "内置默认节点")
	e.wantLogs(0, "已过期")
}

// 同时有多个调用方要取列表（help 启动时的后台刷新，加上找不到前缀时的同步取一次）：只发一个请求，大家等同一个结果。
// 调用方自己的 ctx 只决定它等多久，不取消这次请求：放弃等待的调用方不影响别人，取到的列表照样写进缓存。
func TestDirManagerRefreshCoalesces(t *testing.T) {
	e := newDirEnv(t)
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	release := e.hold()
	m := e.manager()

	first := make(chan error, 1)
	go func() { first <- m.Refresh(ctx5s(t)) }()
	waitFor(t, "the first request", func() bool { return e.hitCount() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := within5s(t, "the second Refresh", func() error { return m.Refresh(ctx) }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Refresh = %v, want its own deadline", err)
	}
	if n := e.hitCount(); n != 1 {
		t.Fatalf("CF got %d requests while one was in flight, want 1", n)
	}
	release()
	if err := recv5s(t, "the first Refresh", first); err != nil {
		t.Fatalf("first Refresh = %v", err)
	}
	if v, _ := m.Current(); v == nil || v.Serial != 42 {
		t.Fatalf("Current = %+v, want list 42", v)
	}
	// 请求结束后，下一次 Refresh 重新发请求。
	if err := within5s(t, "the next Refresh", func() error { return m.Refresh(ctx5s(t)) }); err != nil {
		t.Fatal(err)
	}
	if n := e.hitCount(); n != 2 {
		t.Fatalf("CF got %d requests, want 2", n)
	}
}

// 发起请求的调用方先放弃（它的 ctx 到期）：请求不跟着取消，照样做完，后来等着的调用方拿到结果，列表写进缓存。
func TestDirManagerRefreshOutlivesInitiator(t *testing.T) {
	e := newDirEnv(t)
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	release := e.hold()
	m := e.manager()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	initiator := make(chan error, 1)
	go func() { initiator <- m.Refresh(ctx) }()
	waitFor(t, "the request", func() bool { return e.hitCount() == 1 })
	other := make(chan error, 1)
	go func() { other <- m.Refresh(ctx5s(t)) }()

	if err := recv5s(t, "the initiating Refresh", initiator); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initiating Refresh = %v, want its own deadline", err)
	}
	release()
	if err := recv5s(t, "the waiting Refresh", other); err != nil {
		t.Fatalf("waiting Refresh = %v, want the request to finish after the initiator gave up", err)
	}
	if v, _ := m.Current(); v == nil || v.Serial != 42 {
		t.Fatalf("Current = %+v, want list 42", v)
	}
}

// RefreshIfStale：缓存超过 6 小时才取。「缓存的年龄」取本进程上次从 CF 确认过的时间与列表签发时间中较晚的那个。
func TestDirManagerRefreshIfStale(t *testing.T) {
	e := newDirEnv(t)
	e.serve(e.sign(42, e.entry("A", directory.StatusActive))) // 签发于 Epoch-1h
	ctx := ctx5s(t)
	step := func(m *dirManager, now time.Time, wantHits int, what string) {
		t.Helper()
		e.setNow(now)
		if err := m.RefreshIfStale(ctx); err != nil {
			t.Fatalf("%s: RefreshIfStale = %v", what, err)
		}
		if n := e.hitCount(); n != wantHits {
			t.Fatalf("%s: CF got %d requests, want %d", what, n, wantHits)
		}
	}
	m := e.manager()
	step(m, dirtest.Epoch.Add(5*time.Hour), 1, "no cache")
	step(m, dirtest.Epoch.Add(5*time.Hour), 1, "just confirmed")
	step(m, dirtest.Epoch.Add(11*time.Hour-time.Second), 1, "confirmed less than 6h ago")
	step(m, dirtest.Epoch.Add(11*time.Hour), 2, "confirmed 6h ago")

	// 新进程没确认过，看列表签发时间。
	step(e.manager(), dirtest.Epoch.Add(5*time.Hour-time.Second), 2, "new process, list issued less than 6h ago")
	step(e.manager(), dirtest.Epoch.Add(5*time.Hour), 3, "new process, list issued 6h ago")
	// 签发时间在本机时钟的「未来」（时钟慢了，仍在允许的误差内）：算不出年龄，当作过期。
	step(e.manager(), dirtest.Epoch.Add(-2*time.Hour), 4, "new process, list issued in the future")
}

// 已经有请求在进行（help 启动时的后台刷新）时，RefreshIfStale（找不到前缀时的同步调用）等它的结果，
// 不能因为「刚试过」就直接返回、拿着旧缓存去路由。
func TestDirManagerRefreshIfStaleWaitsForInflight(t *testing.T) {
	e := newDirEnv(t)
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	release := e.hold()
	m := e.manager()
	first := make(chan error, 1)
	go func() { first <- m.RefreshIfStale(ctx5s(t)) }()
	waitFor(t, "the background request", func() bool { return e.hitCount() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := within5s(t, "RefreshIfStale", func() error { return m.RefreshIfStale(ctx) }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RefreshIfStale while a request is in flight = %v, want it to wait (its own deadline)", err)
	}
	done := make(chan error, 1)
	go func() { done <- m.RefreshIfStale(ctx5s(t)) }()
	release()
	for _, ch := range []chan error{first, done} {
		if err := recv5s(t, "RefreshIfStale", ch); err != nil {
			t.Fatalf("RefreshIfStale = %v", err)
		}
	}
	if n := e.hitCount(); n != 1 {
		t.Fatalf("CF got %d requests, want 1", n)
	}
	if v, _ := m.Current(); v == nil || v.Serial != 42 {
		t.Fatalf("Current = %+v, want list 42", v)
	}
}

// CF 取不到时，一分钟内不再试：MCP 每次连接都会问一次，CF 不通时不能每次都多等 3 秒、多一个请求。
func TestDirManagerRefreshIfStaleThrottlesFailures(t *testing.T) {
	e := newDirEnv(t) // 503
	m := e.manager()
	ctx := ctx5s(t)
	if err := m.RefreshIfStale(ctx); err == nil {
		t.Fatal("RefreshIfStale against a 503 succeeded")
	}
	for _, tc := range []struct {
		what     string
		now      time.Time
		wantHits int
	}{
		{"right after the failure", dirtest.Epoch, 1},
		{"just under a minute later", dirtest.Epoch.Add(directoryRetryDelay - time.Second), 1},
		{"a minute later", dirtest.Epoch.Add(directoryRetryDelay), 2},
		{"clock set back", dirtest.Epoch.Add(directoryRetryDelay - time.Hour), 3}, // 不按「未来」的上次尝试时间卡住
	} {
		e.setNow(tc.now)
		_ = m.RefreshIfStale(ctx)
		if n := e.hitCount(); n != tc.wantHits {
			t.Fatalf("%s: CF got %d requests, want %d", tc.what, n, tc.wantHits)
		}
	}
}

// serveExpiring 让 CF 发一份序号 42、在 Epoch+4h 失效的列表（已过期 20 小时，还在 1 天的时钟误差之内），
// 管理器在 Epoch 收下它，然后 CF 换成正常的列表 43。
func (e *dirEnv) serveExpiring(m *dirManager) {
	e.t.Helper()
	b := e.f.Body(e.entry("A", directory.StatusActive))
	b.Serial = 42
	b.IssuedAt = dirtest.Epoch.Add(-48 * time.Hour).Unix()
	b.ExpiresAt = dirtest.Epoch.Add(-20 * time.Hour).Unix()
	e.serve(e.f.Sign(b))
	e.setNow(dirtest.Epoch)
	if err := m.Refresh(ctx5s(e.t)); err != nil {
		e.t.Fatal(err)
	}
	e.serve(e.sign(43, e.entry("A", directory.StatusActive)))
}

// 本机已经没有能用的列表（这里是过期了）：谈不上「缓存的年龄」，6 小时内从 CF 确认过也要再取，只受一分钟的重试间隔限制。
// 否则 Worker 恢复以后，长期运行的 MCP 进程最多 6 小时都按内置默认节点路由，非默认节点发的码加入不了。
func TestDirManagerRefreshIfStaleWithoutUsableList(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	e.serveExpiring(m)
	e.setNow(dirtest.Epoch.Add(5 * time.Hour)) // 列表一小时前过期，离上次从 CF 确认还不到 6 小时
	if err := m.RefreshIfStale(ctx5s(t)); err != nil {
		t.Fatalf("RefreshIfStale = %v", err)
	}
	if n := e.hitCount(); n != 2 {
		t.Fatalf("CF got %d requests, want 2", n)
	}
	if v, _ := m.Current(); v == nil || v.Serial != 43 {
		t.Fatalf("Current = %+v, want list 43", v)
	}
}

// RefreshIfStale 读缓存只为判断要不要取，不提示：不然缓存的列表刚过期时，后台刷新先打一行「已过期，改用内置默认节点」，
// 紧接着又是「节点列表已更新」，读着像出了事。真要用列表的时候（Current）照常提示。
func TestDirManagerRefreshIfStaleReadsQuietly(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	e.serveExpiring(m)
	e.serve(nil) // CF 这会儿取不到：列表一直是过期的那份
	e.setNow(dirtest.Epoch.Add(5 * time.Hour))
	_ = m.RefreshIfStale(ctx5s(t))
	e.wantLogs(0, "已过期")
	if v, _ := m.Current(); v != nil {
		t.Fatalf("Current = %+v, want no list", v)
	}
	e.wantLogs(1, "[relay]", "已过期", "内置默认节点")
}

// RefreshIfStale 不能拿着读缓存之前的状态去决定：help 启动时的后台刷新与找不到前缀时的同步调用同时到来，
// 而另一个调用在这期间完整做完了一次（请求很快就失败：离线、DNS 失败），这个调用醒来后要看到它，不再发请求。
// 做法：测试持着缓存写锁，调用 B 的读缓存（在 m.mu 之外）被挡住；这期间调用 A 做完一次失败的请求；再放行 B。
// 暂停点是缓存的文件锁，与时钟被调了几次无关。判断与发起请求在同一个临界区里（startLocked 前不放锁）这条，
// 没有可以从外面卡住的地方，靠 RefreshIfStale 的注释和代码评审保证。
func TestDirManagerRefreshIfStaleDecidesAfterReadingCache(t *testing.T) {
	e := newDirEnv(t) // 503：请求很快失败
	m := e.manager()
	if v := m.Offer(e.sign(42, e.entry("A", directory.StatusActive)), "relay 192.0.2.10:8443"); v == nil { // 让缓存文件存在，读它才要拿锁
		t.Fatal("Offer = nil, want list 42")
	}
	e.setNow(dirtest.Epoch.Add(7 * time.Hour)) // 列表签发于 Epoch-1h，已满 6 小时；本进程没从 CF 确认过

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	holder := make(chan error, 1)
	go func() {
		holder <- directory.UpdateCache(e.path, func(*directory.Cache) (bool, error) {
			close(entered)
			<-release
			return false, nil
		})
	}()
	recv5s(t, "the cache write lock", entered)

	b := make(chan error, 1)
	go func() { b <- m.RefreshIfStale(ctx5s(t)) }()
	time.Sleep(200 * time.Millisecond) // 让 B 走到读缓存、被锁挡住
	if err := within5s(t, "the failing Refresh", func() error { return m.Refresh(ctx5s(t)) }); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Refresh = %v, want an HTTP 503 error", err)
	}
	letGo()
	if err := recv5s(t, "the cache write lock holder", holder); err != nil {
		t.Fatal(err)
	}
	if err := recv5s(t, "RefreshIfStale", b); err != nil {
		t.Fatalf("RefreshIfStale = %v, want nil: a request just failed, so it should wait out the retry delay", err)
	}
	if n := e.hitCount(); n != 1 {
		t.Fatalf("CF got %d requests, want 1", n)
	}
}

// CF 发来的列表验不过：不算确认过。缓存里的列表按签发时间已满 6 小时，过了一分钟的重试间隔就要再取。
func TestDirManagerBadListIsNotConfirmation(t *testing.T) {
	e := newDirEnv(t)
	m := e.manager()
	if v := m.Offer(e.sign(42, e.entry("A", directory.StatusActive)), "relay 192.0.2.10:8443"); v == nil { // 本进程没从 CF 确认过
		t.Fatal("Offer = nil, want list 42")
	}
	b := e.f.Body(e.entry("A", directory.StatusActive))
	b.Serial = 43
	rogue, err := directory.SignDirectory(dirtest.SeedKey("rogue"), b)
	if err != nil {
		t.Fatal(err)
	}
	e.serve(rogue)
	at := dirtest.Epoch.Add(5 * time.Hour) // 列表签发于 Epoch-1h
	e.setNow(at)
	if err := m.Refresh(ctx5s(t)); !errors.Is(err, directory.ErrBadSignature) {
		t.Fatalf("Refresh = %v, want ErrBadSignature", err)
	}
	e.setNow(at.Add(2 * time.Minute))
	if err := m.RefreshIfStale(ctx5s(t)); !errors.Is(err, directory.ErrBadSignature) {
		t.Fatalf("RefreshIfStale = %v, want another request (ErrBadSignature again)", err)
	}
	if n := e.hitCount(); n != 2 {
		t.Fatalf("CF got %d requests, want 2", n)
	}
}

// 记下的尝试、确认时间不带单调时钟读数：Linux、macOS 的单调时钟在睡眠时不走，带着它比较的话，笔记本睡一夜醒来，
// 「6 小时没从 CF 确认过」要再醒着过 6 小时才成立。
func TestDirManagerTimesUseWallClock(t *testing.T) {
	e := newDirEnv(t)
	e.serve(e.sign(42, e.entry("A", directory.StatusActive))) // 签发于 Epoch-1h
	offset := dirtest.Epoch.Add(7 * time.Hour).Sub(time.Now())
	clock := func() time.Time { return time.Now().Add(offset) } // 和真的时钟一样带单调读数，墙上时间在 Epoch+7h 附近
	check := func(how string, m *dirManager) {
		t.Helper()
		m.mu.Lock()
		attempted, checked := m.attemptedAt, m.checkedAt
		m.mu.Unlock()
		for name, at := range map[string]time.Time{"attemptedAt": attempted, "checkedAt": checked} {
			if at.IsZero() {
				t.Errorf("%s: %s was not recorded", how, name)
			}
			if strings.Contains(at.String(), " m=") {
				t.Errorf("%s: %s = %s carries a monotonic clock reading", how, name, at)
			}
		}
	}
	m := e.manager()
	m.now = clock
	if err := m.Refresh(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	check("Refresh", m)
	// 另一个进程：列表已满 6 小时，RefreshIfStale 自己发请求。
	m = e.manager()
	m.now = clock
	if err := m.RefreshIfStale(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	if n := e.hitCount(); n != 2 {
		t.Fatalf("CF got %d requests, want 2", n)
	}
	check("RefreshIfStale", m)
}

func TestDirManagerRunFetchesAtStartThenPeriodically(t *testing.T) {
	e := newDirEnv(t)
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))

	// 启动时立刻取一次，之后按间隔取。
	m := e.manager()
	m.every = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor(t, "the startup fetch", func() bool { return e.hitCount() == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := e.hitCount(); n != 1 {
		t.Fatalf("CF got %d requests before the first interval, want 1", n)
	}
	cancel()
	recv5s(t, "Run after cancel", done)

	m = e.manager()
	m.every = 20 * time.Millisecond
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor(t, "periodic fetches", func() bool { return e.hitCount() >= 4 })
	cancel()
	recv5s(t, "Run after cancel", done)
	// 等可能还在进行的请求结束（Refresh 会等它），免得它在测试结束、临时目录删掉之后才写缓存。
	_ = within5s(t, "draining Refresh", func() error { return m.Refresh(ctx5s(t)) })
}

// 没有 CF 地址（REMOTE_RELAY_DIRECTORY 设成空）：不取 CF，缓存与 relay 转发照常可用。
func TestDirManagerWithoutURL(t *testing.T) {
	e := newDirEnv(t)
	e.serve(e.sign(42, e.entry("A", directory.StatusActive)))
	m := e.manager()
	m.url = ""
	if err := m.Refresh(ctx5s(t)); !errors.Is(err, errDirectoryOff) {
		t.Fatalf("Refresh without a URL = %v, want errDirectoryOff", err)
	}
	if e.logCount() != 0 {
		t.Fatalf("Refresh without a URL printed %q", e.logsWith(""))
	}
	if err := m.RefreshIfStale(ctx5s(t)); err != nil {
		t.Fatalf("RefreshIfStale without a URL = %v, want nil (nothing to do)", err)
	}
	done := make(chan struct{})
	go func() { m.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run without a URL did not return")
	}
	if v := m.Offer(e.sign(42, e.entry("A", directory.StatusActive)), "relay 192.0.2.10:8443"); v == nil {
		t.Fatal("Offer without a URL = nil, want the list")
	}
	if v, _ := m.Current(); v == nil {
		t.Fatal("Current without a URL = nil, want the cached list")
	}
	if n := e.hitCount(); n != 0 {
		t.Fatalf("CF got %d requests without a URL", n)
	}
}

// 没有管理器（拿不到主目录、没有内置根）：一律当作没有列表，不出错。
func TestDirManagerNil(t *testing.T) {
	var m *dirManager
	if v, c := m.Current(); v != nil || c != nil {
		t.Fatalf("nil Current = %v, %v", v, c)
	}
	if s, k := m.Position(); s != 0 || k != 0 {
		t.Fatalf("nil Position = (%d, %d)", s, k)
	}
	if v := m.Offer([]byte("{}"), "relay 192.0.2.10:8443"); v != nil {
		t.Fatalf("nil Offer = %v", v)
	}
	if err := m.Refresh(ctx5s(t)); err == nil {
		t.Fatal("nil Refresh succeeded")
	}
	if err := m.RefreshIfStale(ctx5s(t)); err != nil {
		t.Fatalf("nil RefreshIfStale = %v", err)
	}
	m.Run(context.Background())
}

func TestDirectoryURLFromEnv(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	t.Setenv(directoryEnv, "")
	os.Unsetenv(directoryEnv)
	if got := directoryURLFromEnv(logf); got != directory.DefaultURL {
		t.Fatalf("unset: %q, want the default %q", got, directory.DefaultURL)
	}
	for _, tc := range []struct{ env, want string }{
		{"", ""},
		{"   ", ""},
		// Windows 的 cmd 里 set REMOTE_RELAY_DIRECTORY= 是删掉变量，留不出空值，所以要有一个显式的关闭值。
		{"off", ""},
		{" OFF ", ""},
		{"https://relay-list.example.test/v1/directory", "https://relay-list.example.test/v1/directory"},
		{" https://relay-list.example.test/v1/directory ", "https://relay-list.example.test/v1/directory"},
	} {
		t.Setenv(directoryEnv, tc.env)
		if got := directoryURLFromEnv(logf); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.env, got, tc.want)
		}
	}
	if len(logs) != 0 {
		t.Fatalf("valid values printed %q", logs)
	}
	// 设了但不能用：不取 CF（而不是悄悄改用默认地址），并提示。
	t.Setenv(directoryEnv, "http://relay-list.example.test/v1/directory")
	if got := directoryURLFromEnv(logf); got != "" {
		t.Fatalf("http URL: got %q, want empty", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], directoryEnv) || !strings.Contains(logs[0], "[relay]") {
		t.Fatalf("http URL printed %q, want one line naming %s", logs, directoryEnv)
	}
}

func TestNewDirManager(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(directoryEnv, "")
	os.Unsetenv(directoryEnv)
	m := newDirManager(func(string, ...any) {})
	if m == nil {
		t.Fatal("newDirManager = nil with a home directory")
	}
	if want := filepath.Join(home, directoryCacheFile); m.path != want {
		t.Errorf("path = %q, want %q", m.path, want)
	}
	if m.url != directory.DefaultURL {
		t.Errorf("url = %q, want %q", m.url, directory.DefaultURL)
	}
	if !m.roots.Usable() {
		t.Error("roots are not usable, want the builtin roots")
	}
	if m.http == nil || m.http.Timeout != directoryFetchTimeout || m.http.CheckRedirect == nil {
		t.Errorf("http client = %+v, want the directory client with a %s timeout", m.http, directoryFetchTimeout)
	}
	if m.every != directoryRefreshEvery || m.now == nil || m.logf == nil {
		t.Errorf("every = %s, now set %v, logf set %v", m.every, m.now != nil, m.logf != nil)
	}
	const custom = "https://relay-list.example.test/v1/directory"
	t.Setenv(directoryEnv, custom)
	if m := newDirManager(func(string, ...any) {}); m == nil || m.url != custom {
		t.Fatalf("newDirManager with %s set = %+v, want url %q", directoryEnv, m, custom)
	}

	// 拿不到主目录：不启用，自动模式退回内置默认节点。
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if m := newDirManager(func(string, ...any) {}); m != nil {
		t.Fatalf("newDirManager = %+v without a home directory, want nil", m)
	}
}
