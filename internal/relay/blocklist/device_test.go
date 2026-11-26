package blocklist

// 按设备封禁（设计 2026-11-23 §3）：relay 另开一个 List，键是设备键（dev- 加 24 位小写十六进制）而不是 IP。
// 两本名单键空间不同、文件不同、临时文件命名不同，共用一份封禁记录；IP 名单的行为与文件格式保持不变。

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testDev = "dev-0123456789abcdef01234567"

func openDeviceMem(t *testing.T, clk *fakeClock) *List {
	t.Helper()
	cfg := testConfig(clk)
	cfg.Keyer = DeviceKeyer
	l, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func deviceHit(l *List, key string, src DeviceSource, n int) bool {
	banned := false
	for i := 0; i < n; i++ {
		if l.RecordDeviceViolation(key, src, KindJoinFailed, "code_invalid") {
			banned = true
		}
	}
	return banned
}

func TestNormalizeDeviceKey(t *testing.T) {
	for in, want := range map[string]string{
		testDev:             testDev,
		" " + testDev + " ": testDev,
	} {
		got, err := NormalizeDeviceKey(in)
		if err != nil || got != want {
			t.Errorf("NormalizeDeviceKey(%q) = %q, %v；want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "198.51.100.7", "2001:db8::/64", "dev-",
		"dev-0123456789ABCDEF01234567",  // 大写
		"DEV-0123456789abcdef01234567",  // 前缀大写
		"dev-0123456789abcdef0123456",   // 23 位
		"dev-0123456789abcdef012345678", // 25 位
		"dev-0123456789abcdef0123456g",  // 非十六进制
		"dev-0123456789abcdef 1234567",
	} {
		if _, err := NormalizeDeviceKey(bad); !errors.Is(err, ErrInvalidDevice) {
			t.Errorf("NormalizeDeviceKey(%q) 应返回 ErrInvalidDevice，got %v", bad, err)
		}
	}
}

// 设备名单只认设备键：IP、非规范键既不计分也不建条目，管理入口明确报错；IP 名单照旧不认设备键。
func TestDeviceKeyerAcceptsOnlyDeviceKeys(t *testing.T) {
	l := openDeviceMem(t, newFakeClock())
	for _, bad := range []string{"198.51.100.7", "2001:db8::1", "dev-0123456789ABCDEF01234567", ""} {
		if l.RecordViolation(bad, KindJoinFailed, "x") || l.RecordDeviceViolation(bad, DeviceSource{Via: "198.51.100.7"}, KindJoinFailed, "x") {
			t.Errorf("%q 不是设备键，不应被封", bad)
		}
		if _, err := l.Ban(bad, time.Hour, false, "op", ""); !errors.Is(err, ErrInvalidDevice) {
			t.Errorf("Ban(%q) 应返回 ErrInvalidDevice，got %v", bad, err)
		}
		if !l.CheckAccept(bad) || l.IsBanned(bad) {
			t.Errorf("%q 不是设备键，准入应放行", bad)
		}
	}
	if n := trackedCount(l); n != 0 {
		t.Fatalf("非法键不应建条目，实际 %d 条", n)
	}
	if !hit(l, testDev, KindJoinFailed, l.Config().Threshold) {
		t.Fatal("设备键应照常计分并自动封禁")
	}
	if l.CheckAccept(testDev) || !l.IsBanned(testDev) {
		t.Fatal("被封设备应被拒")
	}

	ip := openMem(t, newFakeClock())
	if _, err := ip.Ban(testDev, time.Hour, false, "op", ""); !errors.Is(err, ErrInvalidIP) {
		t.Fatalf("IP 名单不认设备键：Ban 应返回 ErrInvalidIP，got %v", err)
	}
	if hit(ip, testDev, KindJoinFailed, ip.Config().Threshold) || trackedCount(ip) != 0 {
		t.Fatal("IP 名单不应为设备键计分或建条目")
	}
}

// 设备键由名单自己的 salt 派生：同一名单内稳定、两类来源互不相撞、salt 不同就不同、重开持久化文件后不变；
// 原始标识不落盘。IP 名单不发设备键。
func TestDeviceKeyDerivation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocklist-devices.json")
	clk := newFakeClock()
	open := func() *List {
		t.Helper()
		cfg := testConfig(clk)
		cfg.Path, cfg.Keyer = path, DeviceKeyer
		l, err := Open(cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return l
	}
	l := open()
	const cid = "cid_00112233445566778899aabbccddeeff"
	k := l.DeviceKey(DeviceByClientID, cid)
	if !isDeviceKey(k) {
		t.Fatalf("DeviceKey = %q，应为 dev- 加 24 位小写十六进制", k)
	}
	if l.DeviceKey(DeviceByClientID, cid) != k {
		t.Fatal("同一标识应得到同一设备键")
	}
	if l.DeviceKey(DeviceByHost, cid) == k {
		t.Fatal("不同来源类别的同一标识不得相撞")
	}
	if l.DeviceKey(DeviceByClientID, cid+"0") == k {
		t.Fatal("不同标识不应相撞")
	}
	if l.DeviceKey(DeviceByClientID, "") != "" || l.DeviceKey(DeviceByHost, "") != "" {
		t.Fatal("空标识不建设备键")
	}
	if openDeviceMem(t, newFakeClock()).DeviceKey(DeviceByClientID, cid) == k {
		t.Fatal("salt 不同的设备名单应得到不同的设备键")
	}
	if openMem(t, newFakeClock()).DeviceKey(DeviceByClientID, cid) != "" {
		t.Fatal("IP 名单不发设备键")
	}
	if !hit(l, k, KindJoinFailed, l.Config().Threshold) {
		t.Fatal("应被自动封禁")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(cid)) {
		t.Fatal("设备名单文件不得落盘原始 ClientID")
	}
	l = open()
	defer l.Close()
	if l.DeviceKey(DeviceByClientID, cid) != k {
		t.Fatal("重启后同一标识的设备键应不变（salt 随文件持久化）")
	}
	if !l.IsBanned(k) {
		t.Fatal("设备封禁应持久化")
	}
}

// 设备条目记下最近来源 IP（via）与自报的客户端信息，随文件持久化；via 跟着最近一次违规走。
func TestDeviceEntryRecordsViaAndClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocklist-devices.json")
	clk := newFakeClock()
	cfg := testConfig(clk)
	cfg.Path, cfg.Keyer = path, DeviceKeyer
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	src := DeviceSource{Via: "198.51.100.7", Version: "v1.3.0", Host: "alice@alice-pc windows amd64"}
	if !deviceHit(l, testDev, src, l.Config().Threshold) {
		t.Fatal("应被自动封禁")
	}
	e := entryOf(t, l, testDev)
	if e.Via != src.Via || len(e.Clients) != 1 || e.Clients[0].Host != src.Host || e.Clients[0].Version != src.Version || e.Clients[0].Role != "help" {
		t.Fatalf("设备条目应记下来源与客户端：via=%q clients=%+v", e.Via, e.Clients)
	}
	src2 := src
	src2.Via = "203.0.113.9"
	l.RecordDeviceViolation(testDev, src2, KindJoinFailed, "code_invalid")
	if got := entryOf(t, l, testDev).Via; got != src2.Via {
		t.Fatalf("via 应是最近一次的来源，got %q", got)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"via":"203.0.113.9"`)) {
		t.Fatalf("via 应随设备名单持久化：%s", raw)
	}
	l, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if e := entryOf(t, l, testDev); !l.IsBanned(testDev) || e.Via != src2.Via || len(e.Clients) == 0 || e.Clients[0].Host != src.Host {
		t.Fatalf("重启后应保留封禁、via 与客户端：banned=%v %+v", l.IsBanned(testDev), e)
	}
}

// IP 名单上 RecordDeviceViolation 等同 RecordViolation：不写 via、不记客户端。写出的 blocklist.json 仍能被线上版本
// （a45533f，开了 DisallowUnknownFields）严格解码——多一个字段，回滚后整份名单就会进入纯内存模式。
func TestIPListNeverWritesVia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocklist.json")
	cfg := testConfig(newFakeClock())
	cfg.Path = path
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	src := DeviceSource{Via: "203.0.113.9", Version: "v1.3.0", Host: "alice@alice-pc windows amd64"}
	if !deviceHit(l, "198.51.100.8", src, l.Config().Threshold) {
		t.Fatal("IP 名单上应照常计分封禁")
	}
	if e := entryOf(t, l, "198.51.100.8"); e.Via != "" || len(e.Clients) != 0 {
		t.Fatalf("IP 条目不应有 via 或由违规带入的客户端：%+v", e)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"via"`)) {
		t.Fatalf("blocklist.json 不得出现 via：%s", raw)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fd a45533fFileData
	if err := dec.Decode(&fd); err != nil {
		t.Fatalf("线上版本解码失败：%v\n%s", err, raw)
	}
}

// 篡改过的 blocklist.json 里 IP 条目带了 via：加载时丢掉，写回的文件里也没有。
func TestIPListDropsViaOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocklist.json")
	cfg := testConfig(newFakeClock())
	cfg.Path = path
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ban("198.51.100.9", 0, true, "op", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(raw, []byte(`"ip":"198.51.100.9"`), []byte(`"ip":"198.51.100.9","via":"203.0.113.1"`), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("前置条件：没找到要篡改的条目")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if e := entryOf(t, l, "198.51.100.9"); e.Via != "" || !l.IsBanned("198.51.100.9") {
		t.Fatalf("加载时应丢掉 IP 条目的 via 并保留封禁：%+v", e)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if bytes.Contains(raw, []byte(`"via"`)) {
		t.Fatalf("写回的 blocklist.json 不得出现 via：%s", raw)
	}
}

// 两本名单共用一份封禁记录：设备封禁记成 target=device（带 via 与身份串标签），IP 的照旧；重启对账时
// 各自只处理自己那类记录，不会把对方生效中的记录当成丢失结束掉。
func TestDeviceAndIPListsShareRecords(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock()
	ipPath := filepath.Join(dir, "blocklist.json")
	devPath, recPath := SiblingPath(ipPath, "devices"), SiblingPath(ipPath, "records")
	open := func() (*List, *List, *Records) {
		t.Helper()
		r, err := OpenRecords(RecordsConfig{Path: recPath, Now: clk.Now})
		if err != nil {
			t.Fatal(err)
		}
		ipCfg := testConfig(clk)
		ipCfg.Path, ipCfg.Records = ipPath, r
		ipl, err := Open(ipCfg)
		if err != nil {
			t.Fatal(err)
		}
		devCfg := testConfig(clk)
		devCfg.Path, devCfg.Records, devCfg.Keyer = devPath, r, DeviceKeyer
		devl, err := Open(devCfg)
		if err != nil {
			t.Fatal(err)
		}
		return ipl, devl, r
	}
	closeAll := func(ipl, devl *List, r *Records) {
		t.Helper()
		for _, c := range []interface{ Close() error }{devl, ipl, r} {
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}

	ipl, devl, r := open()
	const ip = "198.51.100.7"
	autoBan(t, ipl, ip)
	src := DeviceSource{Via: "203.0.113.9", Version: "v1.3.0", Host: "alice@alice-pc windows amd64"}
	if !deviceHit(devl, testDev, src, devl.Config().Threshold) {
		t.Fatal("设备应被自动封禁")
	}
	if d := onlyRec(t, r, testDev); d.Target != TargetDevice || d.Via != src.Via || d.Label != src.Host || d.Reason != "auto" || d.End != "" {
		t.Fatalf("设备封禁记录不对：%+v", d)
	}
	if x := onlyRec(t, r, ip); x.Target != TargetIP || x.Via != "" || x.Label != "" {
		t.Fatalf("IP 封禁记录不对：%+v", x)
	}
	closeAll(ipl, devl, r)

	ipl, devl, r = open()
	for _, k := range []string{testDev, ip} {
		if x := onlyRec(t, r, k); x.End != "" {
			t.Errorf("%s 的记录重启后应仍在生效：%+v", k, x)
		}
	}
	if !devl.IsBanned(testDev) || !ipl.IsBanned(ip) {
		t.Fatal("两本名单的封禁都应持久化")
	}
	if err := devl.Unban(testDev, "op", "", nil); err != nil {
		t.Fatal(err)
	}
	if onlyRec(t, r, testDev).End != EndUnban || onlyRec(t, r, ip).End != "" {
		t.Fatal("解禁设备只应结束设备那条记录")
	}
	closeAll(ipl, devl, r)
}

// 两本名单在同一目录：写盘的临时文件各用各的前缀，打开时也只清理自己命名的残留，不会删掉对方正在写的。
func TestDeviceListTempFilesDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock()
	ipStale, devStale := filepath.Join(dir, ".blocklist-stale.tmp"), filepath.Join(dir, ".bldev-stale.tmp")
	touch := func(p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}
	// 两本名单各有一个后台写盘协程，都会调 record：追加要加锁。
	var (
		mu       sync.Mutex
		patterns []string
	)
	record := func(path string, data []byte, pattern string) error {
		mu.Lock()
		patterns = append(patterns, filepath.Base(path)+" "+pattern)
		mu.Unlock()
		return writeFileAtomicPattern(path, data, pattern)
	}

	touch(ipStale)
	touch(devStale)
	devCfg := testConfig(clk)
	devCfg.Path, devCfg.Keyer, devCfg.atomicWrite = filepath.Join(dir, "blocklist-devices.json"), DeviceKeyer, record
	devl, err := Open(devCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(ipStale) {
		t.Fatal("设备名单打开时不应删 IP 名单的临时文件")
	}
	if exists(devStale) {
		t.Fatal("设备名单打开时应清理自己的残留临时文件")
	}
	touch(devStale)
	ipCfg := testConfig(clk)
	ipCfg.Path, ipCfg.atomicWrite = filepath.Join(dir, "blocklist.json"), record
	ipl, err := Open(ipCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(devStale) {
		t.Fatal("IP 名单打开时不应删设备名单的临时文件")
	}
	if exists(ipStale) {
		t.Fatal("IP 名单打开时应清理自己的残留临时文件")
	}
	if err := devl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ipl.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), patterns...)
	mu.Unlock()
	var sawDev, sawIP bool
	for _, p := range got {
		switch {
		case strings.HasPrefix(p, "blocklist-devices.json "):
			sawDev = true
			if p != "blocklist-devices.json .bldev-*.tmp" {
				t.Errorf("设备名单的临时文件命名不对：%s", p)
			}
		case strings.HasPrefix(p, "blocklist.json "):
			sawIP = true
			if p != "blocklist.json .blocklist-*.tmp" {
				t.Errorf("IP 名单的临时文件命名不对：%s", p)
			}
		}
	}
	if !sawDev || !sawIP {
		t.Fatalf("两本名单都应写过盘：%q", got)
	}
}

// StaticAllowed 暴露 --blocklist-allow 的命中判断：relay 据此让放行来源上的设备也不计分、不被拒。
func TestStaticAllowedExported(t *testing.T) {
	l, _ := openStatic(t, "203.0.113.0/24")
	if !l.StaticAllowed("203.0.113.9") || !l.StaticAllowed("::ffff:203.0.113.9") {
		t.Fatal("放行范围内应返回 true")
	}
	if l.StaticAllowed("198.51.100.7") || l.StaticAllowed("not-an-ip") || l.StaticAllowed("") {
		t.Fatal("范围外与非法地址应返回 false")
	}
	var nilList *List
	if nilList.StaticAllowed("203.0.113.9") {
		t.Fatal("nil 名单应返回 false")
	}
}
