package blocklist

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 第四轮 R4：客户端自报串与备注、事件 detail 里的双向控制符与其他控制/格式字符在保存时剔除，
// 避免判定提示里"身份可伪造"的警示被倒序渲染（界面欺骗）。
func TestBidiAndFormatCharsStripped(t *testing.T) {
	clk := newFakeClock()
	l := openMem(t, clk)
	const ip = "203.0.113.120"
	evil := "财务部\u202e\u2066\u200f\u061c\u2028\u2029\u200b-张三\x07"
	l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1\u202d.2", Host: evil, ClientID: "c"})
	l.RecordViolation(ip, KindUnknownMessage, "x\u2028y\u202ez")
	if _, err := l.Ban(ip, time.Hour, false, "op", "备注\u202e倒序\u2066"); err != nil {
		t.Fatal(err)
	}
	d := entryOf(t, l, ip)
	all := d.Clients[0].Host + d.Clients[0].Version + d.Events[0].Detail + d.Ban.Note + d.Verdict
	for _, r := range all {
		if isUnsafeRune(r) {
			t.Fatalf("仍含控制/格式字符 %U：%q", r, all)
		}
	}
	if d.VerdictSelf == "" || !strings.Contains(d.Verdict, d.VerdictSelf) {
		t.Fatalf("判定应单独给出自报片段供前端 <bdi> 隔离: %q / %q", d.Verdict, d.VerdictSelf)
	}
	if d.Clients[0].Host != "财务部-张三" || d.Clients[0].Version != "v1.2" {
		t.Fatalf("剔除后的值不对: %q %q", d.Clients[0].Host, d.Clients[0].Version)
	}
}

// 第四轮 R3：两族自动封禁在上限规模下全部带着需要 JSON 转义的自报串与事件 detail，重启后
// 不丢任何自动封禁、也不丢记忆。
func TestMaliciousFillAtLimitKeepsAutoBansAndMemory(t *testing.T) {
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
	evil := strings.Repeat("\"\u2028\\", 22)
	for i := 0; i < MaxTrackedLimit; i++ {
		for _, ip := range []string{fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&0xff, i&0xff), fmt.Sprintf("2001:db8:%x:%x::1", i>>8+1, i&0xff)} {
			for c := 0; c < 10; c++ {
				l.ObserveClient(ip, ClientInfo{Role: "share", Version: evil, Host: evil, ClientID: fmt.Sprintf("cid-%d-%d", i, c)})
			}
			for j := 0; j < 20; j++ {
				l.RecordViolation(ip, KindUnknownMessage, evil)
			}
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	rep := l.lastSaveReport.Load()
	t.Logf("恶意填充（上限规模）文件 %d 字节，裁剪报告 %+v", fileSize(t, path), rep)
	if rep == nil || rep.AutoDropped+rep.MemoryDropped+rep.ManualDropped > 0 {
		t.Fatalf("预算裁剪丢了自动封禁或记忆: %+v", rep)
	}
}

// 第四轮 R2：写盘快照持读锁的时间要短：上限规模下写盘期间 CheckAccept 的最长等待。
func TestSaveSnapshotLockHoldShort(t *testing.T) {
	if testing.Short() {
		t.Skip("规模测试")
	}
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.Path = filepath.Join(t.TempDir(), "bl.json")
	cfg.MaxTracked = MaxTrackedLimit
	cfg.SaveInterval = time.Hour
	l := openAt(t, cfg)
	defer l.Close()
	for i := 0; i < MaxTrackedLimit; i++ {
		for _, ip := range []string{fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&0xff, i&0xff), fmt.Sprintf("2001:db8:%x:%x::1", i>>8+1, i&0xff)} {
			for c := 0; c < 10; c++ {
				l.ObserveClient(ip, ClientInfo{Role: "share", Version: "v1.12.3", Host: "someone@some-workstation kylin arm64", ClientID: fmt.Sprintf("cid-%d-%d", i, c)})
			}
			hit(l, ip, KindProtoDecode, 10)
		}
	}
	var stop atomic.Bool
	var maxWait atomic.Int64
	var wg sync.WaitGroup
	// 写锁请求方：没有它，读锁之间不互斥，CheckAccept 不会被写盘快照挡住。
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			l.RecordViolation("198.51.100.1", KindTLSAborted, "eof")
		}
	}()
	go func() {
		defer wg.Done()
		for !stop.Load() {
			s := time.Now()
			l.CheckAccept("10.0.0.1")
			if d := int64(time.Since(s)); d > maxWait.Load() {
				maxWait.Store(d)
			}
		}
	}()
	runtime.GC() // 先回收构造数据产生的垃圾，避免它在测量期间触发的 GC 计入持锁时长
	holds := make([]time.Duration, 0, 7)
	for i := 0; i < 7; i++ {
		if err := l.saveNow(); err != nil {
			t.Fatal(err)
		}
		holds = append(holds, time.Duration(l.snapshotHold.Load()))
	}
	stop.Store(true)
	wg.Wait()
	sort.Slice(holds, func(i, j int) bool { return holds[i] < holds[j] })
	median := holds[len(holds)/2]
	t.Logf("上限规模写盘 7 次：快照单次持读锁中位数 %s、最大 %s；期间 CheckAccept 最长等待 %s", median, holds[len(holds)-1], time.Duration(maxWait.Load()))
	// 判据用 7 次写盘快照单次持读锁的中位数：单次可能碰上 GC 停顿或调度抖动（本机即使不写盘，
	// CheckAccept 单次最长也有约 10ms），中位数反映结构性的持锁时长。
	// 结构性持锁约 1–2ms；阈值 8ms 留出负载余量，仍能拦住修订前 17–24ms 的回归。
	if median > 8*time.Millisecond {
		t.Fatalf("写盘快照单次持读锁中位数 %s（> 8ms），全部 %v", median, holds)
	}
	// 计时判据依赖机器快慢：整表一次持锁的回归在本机中位数约 9ms，更快的机器可能低于
	// 8ms 而漏报。再加一条与机器无关的结构判据：分块必须远小于上限规模，否则分块形同虚设。
	if limit := l.hardCap() / 8; snapshotChunk > limit {
		t.Fatalf("snapshotChunk=%d 超过上限规模 %d 的 1/8（%d），写盘快照会退化为长时间持锁", snapshotChunk, l.hardCap(), limit)
	}
}

// 第四轮 R6：加载时因上限裁掉的自动封禁与运行时 ban_evicted 一致，清零递增次数。
func TestLoadTrimClearsOffenses(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "bl.json")
	cfg := testConfig(clk)
	cfg.Path = path
	cfg.MaxTracked = 512
	l := openAt(t, cfg)
	for i := 0; i < 400; i++ {
		clk.Advance(time.Second)
		hit(l, fmt.Sprintf("10.61.%d.%d", i/250, i%250), KindProtoDecode, 10)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.MaxTracked = 64
	l2 := openAt(t, cfg)
	defer l2.Close()
	if l2.IsBanned("10.61.0.200") { // 第 200 个：被裁掉，但递增记忆仍在（记忆保留最近 256 条）
		t.Fatal("最早到期的应被裁掉")
	}
	if d, ok := l2.Detail("10.61.0.200"); ok && d.Offenses != 0 {
		t.Fatalf("加载时被裁掉的自动封禁应清零递增次数，got %d", d.Offenses)
	}
	if hit(l2, "10.61.0.200", KindProtoDecode, 10); !l2.IsBanned("10.61.0.200") {
		t.Fatal("应能再次被封")
	}
	if d, _ := l2.Detail("10.61.0.200"); d.Ban == nil || d.Ban.Until-d.Ban.Since != int64(time.Hour/time.Second) {
		t.Fatalf("清零后再犯应从基础时长 1 小时开始: %+v", d.Ban)
	}
}

// 第四轮 R6：写盘连续失败时按退避持续重试，恢复后不需要新的变更也能写成功。
func TestSaveRetriesWithBackoff(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "bl.json")
	cfg := testConfig(clk)
	cfg.Path = path
	cfg.SaveInterval = 5 * time.Millisecond
	cfg.UrgentSaveInterval = 5 * time.Millisecond
	cfg.retryBackoff = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
	l := openAt(t, cfg)
	defer l.Close()
	// 先等初始写盘落地（文件存在）再让 rename 失败；固定睡一小会儿在负载高时不够。
	for i := 0; i < 500; i++ {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 让 rename 失败：目标路径变成一个非空目录
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0700); err != nil {
		t.Fatal(err)
	}
	before := l.SaveCount()
	if _, err := l.Ban("192.0.2.9", time.Hour, false, "op", "n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if n := l.saveFailures.Load(); n < 4 {
		t.Fatalf("写盘失败后应按退避持续重试，只试了 %d 次", n)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if l.SaveCount() == before {
		t.Fatal("恢复后重试应写成功")
	}
}

func isUnsafeRune(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) ||
		(r >= 0x2066 && r <= 0x2069) || r == 0x061c || r == 0x2028 || r == 0x2029
}
