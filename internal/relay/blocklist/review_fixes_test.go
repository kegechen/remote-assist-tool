package blocklist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// banAndExpire 让 ip 被自动封一次再到期，留下"只剩递增记忆"的条目。
func banAndExpire(t *testing.T, l *List, clk *fakeClock, ip string) {
	t.Helper()
	if !hit(l, ip, KindProtoDecode, 10) {
		t.Fatalf("%s 应被自动封禁", ip)
	}
}

// B1：大量"只剩递增记忆"的到期封禁条目不得挤占计分表——扫描器照样要在阈值处被封，
// 且跟踪条目总数有硬上限。
func TestOffenseMemoryDoesNotStarveScoring(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 100
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 300; i++ {
		banAndExpire(t, l, clk, fmt.Sprintf("10.2.%d.%d", i/250, i%250))
	}
	clk.Advance(2 * time.Hour)
	l.Sweep()
	// 扫描器每次违规之间穿插一个新 IP 的普通连接（真实场景：公网上源源不断的新来源）。
	banned := false
	for i := 0; i < 40 && !banned; i++ {
		l.ObserveConnect(fmt.Sprintf("10.3.%d.%d", i/250, i%250))
		banned = l.RecordViolation("198.51.100.99", KindTLSGarbage, "not_tls")
	}
	if !banned {
		t.Fatal("受保护的递增记忆把计分条目挤掉了：扫描器 40 次违规仍未被封")
	}
	if n := l.Query(Filter{Status: "tracked"}).Tracked; n > 3*100 {
		t.Fatalf("跟踪条目数 %d 超过硬上限", n)
	}
}

// B1：被挤掉的自动封禁要同时清掉递增次数（否则它下次被封会直接跳到长时长）。
func TestEvictedAutoBanClearsOffenses(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 2
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, ip := range []string{"10.9.0.1", "10.9.0.2", "10.9.0.3"} {
		clk.Advance(time.Second)
		hit(l, ip, KindProtoDecode, 10)
	}
	if l.IsBanned("10.9.0.1") {
		t.Fatal("自动封禁上限（= MaxTracked）满时应淘汰最早到期的")
	}
	if e, ok := l.Detail("10.9.0.1"); ok && e.Offenses != 0 {
		t.Fatalf("被淘汰的封禁应清零递增次数，got %d", e.Offenses)
	}
}

// B1：CheckAccept 在大量受保护条目 + 新 IP 洪峰下吞吐不能塌。
func TestCheckAcceptThroughputUnderChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试")
	}
	clk := newFakeClock()
	l, err := Open(testConfig(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 15000; i++ {
		banAndExpire(t, l, clk, scaleIP(i))
	}
	clk.Advance(2 * time.Hour)
	l.Sweep()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 新 IP 洪峰
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			l.ObserveConnect(fmt.Sprintf("172.16.%d.%d", (i/250)%250, i%250))
		}
	}()
	var ops int64
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		l.CheckAccept("192.0.2.1")
		atomic.AddInt64(&ops, 1)
	}
	close(stop)
	wg.Wait()
	t.Logf("15000 条递增记忆 + 新 IP 洪峰下 CheckAccept：%d 次/秒", ops)
	if ops < 100000 {
		t.Fatalf("CheckAccept 吞吐塌陷：%d 次/秒", ops)
	}
}

// B1：持久化文件大小有界。
func TestPersistedFileSizeBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试")
	}
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 1000
	cfg.Path = filepath.Join(t.TempDir(), "bl.json")
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6000; i++ {
		banAndExpire(t, l, clk, scaleIP(i))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("6000 次自动封禁（MaxTracked=1000）后文件 %d 字节", info.Size())
	if info.Size() > 2<<20 {
		t.Fatalf("文件 %d 字节，超过 2 MiB", info.Size())
	}
}

// B1：超大文件拒绝加载（防 OOM），退回纯内存且不覆盖原文件。
func TestLoadRejectsOversizedFile(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.Path = filepath.Join(t.TempDir(), "bl.json")
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Allow("203.0.113.1", 0, true, "a", strings.Repeat("x", 10)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	raw, _ := os.ReadFile(cfg.Path)
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	entries := doc["entries"].([]interface{})
	entries[0].(map[string]interface{})["allow"].(map[string]interface{})["note"] = strings.Repeat("x", 33<<20)
	big, _ := json.Marshal(doc)
	if err := os.WriteFile(cfg.Path, big, 0600); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(cfg)
	if err == nil {
		l2.Close()
		t.Fatal("超过大小上限的文件应拒绝加载")
	}
	if l2 == nil || l2.Persistent() {
		t.Fatal("应退回纯内存模式")
	}
	l2.Close()
	if info, _ := os.Stat(cfg.Path); info.Size() != int64(len(big)) {
		t.Fatal("原文件不得被覆盖")
	}
}

// B1：IPv6 按 /64 聚合计分与封禁；IPv4 仍按单 IP。
func TestIPv6Slash64Aggregation(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	banned := false
	for i := 1; i <= 20; i++ {
		banned = l.RecordViolation(fmt.Sprintf("2001:db8:1:2::%x", i), KindTLSGarbage, "not_tls") || banned
	}
	if !banned || !l.IsBanned("2001:db8:1:2:ffff::1") {
		t.Fatal("同一 /64 内轮换地址应合并计分并整体封禁")
	}
	if l.IsBanned("2001:db8:1:3::1") {
		t.Fatal("相邻 /64 不应被封")
	}
	e, ok := l.Detail("2001:db8:1:2::abcd")
	if !ok || e.IP != "2001:db8:1:2::/64" {
		t.Fatalf("明细键应为 /64 前缀，got %q", e.IP)
	}
	if _, err := l.Ban("2001:db8:9:9::/64", time.Hour, false, "a", "前缀"); err != nil {
		t.Fatalf("应支持按 /64 前缀手动封禁: %v", err)
	}
	if !l.IsBanned("2001:db8:9:9::42") {
		t.Fatal("手动封禁 /64 应覆盖其中任意地址")
	}
	for _, bad := range []string{"2001:db8::/48", "10.0.0.0/24", "10.0.0.1/32"} {
		if _, err := l.Ban(bad, time.Hour, false, "a", ""); err == nil {
			t.Errorf("%s 应被拒绝（只接受单地址或 IPv6 /64）", bad)
		}
	}
	// IPv4 不聚合
	hit(l, "198.51.100.1", KindProtoDecode, 10)
	if l.IsBanned("198.51.100.2") {
		t.Fatal("IPv4 不得按网段聚合")
	}
}

// B2：对端主动终止（alert、EOF、超时、重置）不计分，只记事件。
func TestAbortedHandshakeNotScored(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	for i := 0; i < 100; i++ {
		l.RecordViolation("198.51.100.30", KindTLSAborted, "alert")
	}
	if l.IsBanned("198.51.100.30") {
		t.Fatal("对端中止握手不得计分封禁")
	}
	if e, _ := l.Detail("198.51.100.30"); e.Points != 0 || len(e.Events) == 0 {
		t.Fatalf("应只记事件不计分: points=%d events=%d", e.Points, len(e.Events))
	}
}

// B2/S1：7 天内有过成功加入（持有效协助码 / 完成配对）的 IP，握手类与未知消息不计分；协议解码错误仍计分。
// 仅注册不授予豁免（见 TestRegisterOnlyDoesNotGrantExemption）。
func TestRecentGoodClientExemptsHandshakeAndUnknown(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "203.0.113.40"
	l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.1.0", ClientID: "cid", Verified: true})
	clk.Advance(6 * 24 * time.Hour)
	for i := 0; i < 50; i++ {
		l.RecordViolation(ip, KindTLSGarbage, "not_tls")
		l.RecordViolation(ip, KindUnknownMessage, "x")
	}
	if l.IsBanned(ip) {
		t.Fatal("近 7 天有成功客户端的 IP 不应因握手/未知消息被封")
	}
	if !hit(l, ip, KindProtoDecode, 10) {
		t.Fatal("协议解码错误不在豁免范围内")
	}
	// 超过 7 天后豁免失效
	const ip2 = "203.0.113.41"
	l.ObserveClient(ip2, ClientInfo{Role: "help", Verified: true})
	clk.Advance(8 * 24 * time.Hour)
	if !hit(l, ip2, KindTLSGarbage, 20) {
		t.Fatal("豁免应在 7 天后失效")
	}
}

// S1：未知消息降为 1 分。
func TestUnknownMessageWeight(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	if hit(l, "198.51.100.31", KindUnknownMessage, 19) {
		t.Fatal("未知消息应为 1 分，19 次不应封")
	}
	if !hit(l, "198.51.100.31", KindUnknownMessage, 1) {
		t.Fatal("20 次未知消息应封")
	}
}

// S6：白名单 IP 的命中记录不无限增长。
func TestAllowlistedHitsBounded(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	if err := l.Allow("203.0.113.50", 0, true, "a", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100000; i++ {
		l.RecordViolation("203.0.113.50", KindProtoDecode, "x")
	}
	l.mu.RLock()
	n := len(l.entries["203.0.113.50"].hits)
	l.mu.RUnlock()
	if n > 256 {
		t.Fatalf("白名单 IP 命中记录增长到 %d 条", n)
	}
}

// S8：启动时清理上次崩溃残留的临时文件。
func TestOpenCleansStaleTempFiles(t *testing.T) {
	clk := newFakeClock()
	dir := t.TempDir()
	stale := filepath.Join(dir, ".blocklist-123.tmp")
	other := filepath.Join(dir, "unrelated.tmp")
	for _, p := range []string{stale, other} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig(clk)
	cfg.Path = filepath.Join(dir, "bl.json")
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("残留的 .blocklist-*.tmp 应被清理")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("不得删除无关文件")
	}
}

// B1：IPv6 轮换攻击。同一 /64 内换 1 万个地址仍是一个条目并在阈值处被封；
// 跨 2 万个 /64 的洪峰不会撑爆主表，CheckAccept 仍是常数级。
func TestIPv6RotationBounded(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 1000
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 20000; i++ {
		l.RecordViolation(fmt.Sprintf("2001:db8:%x:%x::1", i>>16, i&0xffff), KindTLSGarbage, "not_tls")
	}
	if n := l.Query(Filter{Status: "tracked"}).Tracked; n > 1000 {
		t.Fatalf("跨 /64 洪峰后主表 %d 条，超过 MaxTracked", n)
	}
	banned := false
	for i := 0; i < 10000 && !banned; i++ {
		banned = l.RecordViolation(fmt.Sprintf("2001:db8:ffff:1:%x:%x::1", i>>8, i&0xff), KindTLSGarbage, "not_tls")
		if i > 25 {
			t.Fatalf("同一 /64 内轮换地址第 %d 次仍未被封", i)
		}
	}
	start := time.Now()
	for i := 0; i < 100000; i++ {
		l.CheckAccept(fmt.Sprintf("2001:db8:ffff:1::%x", i))
	}
	t.Logf("2 万个 /64 洪峰后 CheckAccept 平均 %s（含构造地址字符串）", time.Since(start)/100000)
}
