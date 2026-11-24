package blocklist

// 第三轮 review 的回归测试。只用公开 API（以及上一轮已有的结构），以便在修复前的代码上
// 原样运行拿到 RED。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openAt(t *testing.T, cfg Config) *List {
	t.Helper()
	l, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// P1：攻击者用超长、需要 JSON 转义的身份串注册大量 /64 再触发自动封禁，持久化文件必须仍在
// 加载上限以内且能被重新加载（否则重启后所有手动封禁和白名单丢失、持久化长期停用）。
func TestPersistedFileStaysLoadableUnderMaliciousFill(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "bl.json")
	cfg := testConfig(clk)
	cfg.Path = path
	cfg.SaveInterval = time.Hour
	l := openAt(t, cfg)
	if _, err := l.Ban("192.0.2.1", 0, true, "op", "保留"); err != nil {
		t.Fatal(err)
	}
	if err := l.Allow("192.0.2.2", 0, true, "op", "保留"); err != nil {
		t.Fatal(err)
	}
	evil := strings.Repeat("<", 120)
	for i := 0; i < 4096; i++ {
		ip := fmt.Sprintf("2001:db8:%x:%x::1", i/256+1, i%256)
		for c := 0; c < 10; c++ {
			l.ObserveClient(ip, ClientInfo{Role: "share", Version: evil, Host: evil, ClientID: fmt.Sprintf("cid-%d-%d", i, c)})
		}
		hit(l, ip, KindProtoDecode, 10)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	size := fileSize(t, path)
	t.Logf("恶意填充后文件 %d 字节（加载上限 %d）", size, maxFileBytes)
	if size > maxFileBytes {
		t.Fatalf("文件 %d 字节超过加载上限 %d", size, maxFileBytes)
	}
	l2, err := Open(cfg)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	defer l2.Close()
	if !l2.IsBanned("192.0.2.1") {
		t.Fatal("手动封禁丢失")
	}
	if e, ok := l2.Detail("192.0.2.2"); !ok || e.Allow == nil {
		t.Fatal("白名单丢失")
	}
}

// P1：--blocklist-max-tracked 取上限（MaxTrackedLimit）且两族自动封禁都打满正常数据时，文件仍可加载，
// 且不因预算丢弃任何封禁。（修复前上限不存在，以 16384 运行同样的数据即超过加载上限。）
func TestPersistedFileLoadableAtMaxTrackedLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试")
	}
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "bl.json")
	cfg := testConfig(clk)
	cfg.Path = path
	cfg.MaxTracked = MaxTrackedLimit
	cfg.SaveInterval = time.Hour
	l := openAt(t, cfg)
	if _, err := l.Ban("192.0.2.1", 0, true, "op", "保留"); err != nil {
		t.Fatal(err)
	}
	host := "someone@some-workstation-name kylin arm64"
	for i := 0; i < MaxTrackedLimit; i++ {
		for _, ip := range []string{fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&0xff, i&0xff), fmt.Sprintf("2001:db8:%x:%x::1", i>>8+1, i&0xff)} {
			for c := 0; c < 10; c++ {
				l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.12.3", Host: host, ClientID: fmt.Sprintf("cid-%d-%d", i, c)})
			}
			hit(l, ip, KindProtoDecode, 10)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	size := fileSize(t, path)
	t.Logf("max-tracked=%d 两族各 %d 条自动封禁：文件 %d 字节（加载上限 %d）", MaxTrackedLimit, MaxTrackedLimit, size, maxFileBytes)
	if rep := l.lastSaveReport.Load(); rep == nil || rep.AutoDropped+rep.ManualDropped+rep.ManualCompacted > 0 {
		t.Fatalf("上限规模的正常数据不应因预算丢弃封禁：%+v", rep)
	}
	if size > maxFileBytes {
		t.Fatalf("文件 %d 字节超过加载上限 %d", size, maxFileBytes)
	}
	l2, err := Open(cfg)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	defer l2.Close()
	if !l2.IsBanned("192.0.2.1") {
		t.Fatal("手动封禁丢失")
	}
}

// S1：注册不需要凭据，仅注册不得授予计分豁免。
func TestRegisterOnlyDoesNotGrantExemption(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "203.0.113.90"
	l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.1.0", ClientID: "cid"})
	if !hit(l, ip, KindTLSGarbage, 20) {
		t.Fatal("扫描器注册一次就免疫握手计分")
	}
}

// S2：仅注册（未认证）的客户端，控制台判定不得说成"正常客户端"，并注明身份串为自报。
func TestVerdictDistinguishesRegisterOnly(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "203.0.113.91"
	l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.1.0", Host: "财务部-张三-PC", ClientID: "cid"})
	v := entryOf(t, l, ip).Verdict
	if strings.Contains(v, "正常") || !strings.Contains(v, "未认证") || !strings.Contains(v, "自报") {
		t.Fatalf("仅注册的判定=%q", v)
	}
}

// S3：IPv6 一侧的海量 /64 自动封禁不得挤掉 IPv4 的封禁；同一 /48 的封禁也挤不掉其他 /48 的。
func TestIPv6FloodCannotEvictOthersBans(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 64
	l := openAt(t, cfg)
	defer l.Close()
	v4 := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"}
	for _, ip := range v4 {
		hit(l, ip, KindProtoDecode, 10)
	}
	other := "2001:db8:ffff:1::1" // 另一个 /48 的受害者
	hit(l, other, KindProtoDecode, 10)
	clk.Advance(time.Minute)
	for i := 0; i < 300; i++ {
		hit(l, fmt.Sprintf("2001:db8:1:%x::1", i), KindProtoDecode, 10) // 攻击者持有 2001:db8:1::/48
	}
	for _, ip := range append(v4, other) {
		if !l.IsBanned(ip) {
			t.Fatalf("%s 的自动封禁被同一 /48 的 IPv6 洪泛挤掉了", ip)
		}
	}
}

// S4：以大上限运行后改小重启，加载时按当前上限裁剪，新扫描器仍能被跟踪并封禁。
func TestLoadTrimsToCurrentCaps(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "bl.json")
	cfg := testConfig(clk)
	cfg.Path = path
	cfg.MaxTracked = 512
	l := openAt(t, cfg)
	for i := 0; i < 400; i++ {
		hit(l, fmt.Sprintf("10.60.%d.%d", i/250, i%250), KindProtoDecode, 10)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.MaxTracked = 64
	l2 := openAt(t, cfg)
	defer l2.Close()
	if !hit(l2, "198.51.100.200", KindProtoDecode, 10) {
		t.Fatalf("缩小上限重启后新扫描器不再被跟踪：tracked=%d untracked=%d", l2.Query(Filter{Status: "tracked"}).Tracked, l2.Query(Filter{}).Untracked)
	}
}

// S5：Teredo / NAT64 / 链路本地地址按单地址跟踪，不按 /64 聚合。
func TestSpecialIPv6PrefixesNotAggregated(t *testing.T) {
	for _, ip := range []string{"2001:0:4136:e378:8000:63bf:3fff:fdd2", "64:ff9b::c000:221", "fe80::1"} {
		k, err := KeyOf(ip)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(k, "/") {
			t.Fatalf("%s 不应按 /64 聚合，got %s", ip, k)
		}
	}
	if k, _ := KeyOf("2001:db8:1:2::5"); k != "2001:db8:1:2::/64" {
		t.Fatalf("普通 IPv6 仍按 /64，got %s", k)
	}
}

// S6：堆顶已到期但尚未 Sweep 的自动封禁被配额挤出时按"到期"处理，保留递增记忆。
func TestExpiredHeapTopTreatedAsExpiry(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 2
	l := openAt(t, cfg)
	defer l.Close()
	hit(l, "10.70.0.1", KindProtoDecode, 10)
	clk.Advance(time.Hour + time.Second) // 到期但未 Sweep
	hit(l, "10.70.0.2", KindProtoDecode, 10)
	hit(l, "10.70.0.3", KindProtoDecode, 10)
	e := entryOf(t, l, "10.70.0.1")
	if e.Offenses != 1 {
		t.Fatalf("已到期的封禁被当成 ban_evicted 清零了递增次数：offenses=%d", e.Offenses)
	}
	if last := e.History[len(e.History)-1].Action; last != "ban_expired" {
		t.Fatalf("应记为 ban_expired，got %s", last)
	}
}

// S7：开发期 v1 格式的文件给出可操作的错误信息。
func TestSchemaV1ErrorIsActionable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bl.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"salt":"00","entries":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Path = path
	l, err := Open(cfg)
	defer l.Close()
	if err == nil || !strings.Contains(err.Error(), "delete") {
		t.Fatalf("v1 文件的错误信息应提示改名或删除后重启，got %v", err)
	}
}

// S11：事件满 20 条后每次违规不再复制整个事件切片。
func TestRecordViolationNoSliceCopyAtCap(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "198.51.100.77"
	for i := 0; i < 40; i++ {
		l.RecordViolation(ip, KindTLSAborted, "eof")
	}
	r := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			l.RecordViolation(ip, KindTLSAborted, "eof")
		}
	})
	t.Logf("事件满后每次违规 %d B/op, %d allocs/op", r.AllocedBytesPerOp(), r.AllocsPerOp())
	if r.AllocedBytesPerOp() > 256 {
		t.Fatalf("每次违规分配 %d 字节，仍在复制事件切片", r.AllocedBytesPerOp())
	}
}

// S12：Sweep 把分值衰减完的条目移回 idle 时按最近出现时间归位，不推到队尾。
func TestSweepKeepsIdleOrder(t *testing.T) {
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.MaxTracked = 3
	l := openAt(t, cfg)
	defer l.Close()
	l.ObserveConnect("10.80.0.1") // X t=0
	clk.Advance(5 * time.Second)
	l.RecordViolation("10.80.0.3", KindTLSGarbage, "x") // Z t=5（计分）
	clk.Advance(5 * time.Second)
	l.ObserveConnect("10.80.0.2") // Y t=10
	clk.Advance(11 * time.Minute)
	l.Sweep() // Z 分值衰减完回到 idle，应排在 X 与 Y 之间
	l.ObserveConnect("10.80.0.4")
	l.ObserveConnect("10.80.0.5")
	if _, ok := l.Detail("10.80.0.2"); !ok {
		t.Fatal("较新的 Y 被淘汰了：Sweep 把很久没出现的 Z 刷成了最近使用")
	}
	if _, ok := l.Detail("10.80.0.3"); ok {
		t.Fatal("较旧的 Z 应先于 Y 被淘汰")
	}
}
