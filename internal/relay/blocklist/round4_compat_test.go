package blocklist

// 第四轮 review 的回归测试，只用公开 API，可在修复前的代码上原样运行拿到 RED。

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// specialBoundaryAddrs 每个"按单地址跟踪"前缀的边界地址，以及与其 /64 重叠但在前缀之外的地址。
func specialBoundaryAddrs() []string {
	return []string{
		// Teredo 2001::/32
		"2001::", "2001::1", "2001:0:ffff:ffff:ffff:ffff:ffff:ffff", "2001:1::1", "2000:ffff:ffff:ffff::1",
		// NAT64 64:ff9b::/96 及同一 /64 内、/96 之外的部分
		"64:ff9b::", "64:ff9b::1.2.3.4", "64:ff9b::ffff:ffff", "64:ff9b::1:0:1", "64:ff9b::ffff:ffff:ffff:ffff", "64:ff9b:0:1::1",
		// 本地 NAT64 64:ff9b:1::/48
		"64:ff9b:1::", "64:ff9b:1::1", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff", "64:ff9b:2::1",
		// 链路本地 fe80::/10
		"fe80::", "fe80::1", "febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "fec0::1", "fe7f:ffff::1",
		// IPv4-mapped 与普通地址
		"::ffff:1.2.3.4", "::ffff:0:0", "::1", "::", "2001:db8::1", "1.2.3.4",
	}
}

// N1：键规则自洽——任何地址求得的键都必须被 NormalizeKey 原样接受，否则持久化文件加载会失败。
func TestKeyOfRoundTripsThroughNormalizeKey(t *testing.T) {
	for _, ip := range specialBoundaryAddrs() {
		k, err := KeyOf(ip)
		if err != nil {
			t.Fatalf("KeyOf(%s): %v", ip, err)
		}
		n, err := NormalizeKey(k)
		if err != nil || n != k {
			t.Errorf("KeyOf(%s)=%s，NormalizeKey 得到 %q, %v", ip, k, n, err)
		}
	}
	// 特殊前缀内随机地址
	for _, base := range []string{"2001::", "64:ff9b::", "64:ff9b:1::", "fe80::"} {
		b := netip.MustParseAddr(base).As16()
		for i := 0; i < 200; i++ {
			a := b
			for j := 4; j < 16; j++ {
				a[j] ^= byte(i*31 + j*17)
			}
			ip := netip.AddrFrom16(a).String()
			k, _ := KeyOf(ip)
			if n, err := NormalizeKey(k); err != nil || n != k {
				t.Errorf("KeyOf(%s)=%s 往返失败: %q %v", ip, k, n, err)
			}
		}
	}
}

// N1：含特殊前缀键的永久手动封禁做一次持久化往返，文件必须能加载、封禁全在。
func TestSpecialKeysPersistenceRoundTrip(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "bl.json")
	cfg := testConfig(clk)
	cfg.Path = path
	l := openAt(t, cfg)
	var banned []string
	for _, ip := range specialBoundaryAddrs() {
		if _, err := l.Ban(ip, 0, true, "op", "special"); err != nil {
			t.Fatalf("Ban(%s): %v", ip, err)
		}
		banned = append(banned, ip)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(cfg)
	if err != nil {
		t.Fatalf("含特殊前缀键的文件加载失败（手动封禁全部丢失、持久化停用）: %v", err)
	}
	defer l2.Close()
	for _, ip := range banned {
		if !l2.IsBanned(ip) {
			t.Errorf("%s 的永久手动封禁重启后丢失", ip)
		}
	}
}

// N1：文件里单条非法或重复的键只丢弃该条（记日志），不拒绝整份文件。
func TestLoadDropsOnlyBadEntries(t *testing.T) {
	clk := newFakeClock()
	path := filepath.Join(t.TempDir(), "bl.json")
	cfg := testConfig(clk)
	cfg.Path = path
	l := openAt(t, cfg)
	if _, err := l.Ban("192.0.2.7", 0, true, "op", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	s := string(raw)
	bad := `{"ip":"10.0.0.0/8","first_seen":1,"last_seen":1,"connections":0,"rejected":0,"clients":null,"events":null,"history":null,"grace_until":0,"ban_count":0,"ban":{"since":1,"until":0,"permanent":true,"reason":"manual","actor":"x","note":""}},`
	dup := `{"ip":"192.0.2.7","first_seen":1,"last_seen":1,"connections":0,"rejected":0,"clients":null,"events":null,"history":null,"grace_until":0,"ban_count":0},`
	s = strings.Replace(s, `"entries":[`, `"entries":[`+bad, 1)
	badMem := `{"key":"not-an-ip","offenses":1,"last_ban_end":0,"last_good":0}`
	if strings.Contains(s, `"memory":[]`) {
		s = strings.Replace(s, `"memory":[]`, `"memory":[`+badMem+`]`, 1)
	} else {
		s = strings.Replace(s, `"memory":[`, `"memory":[`+badMem+`,`, 1)
	}
	// 重复键追加在后面：保留第一条
	s = strings.Replace(s, `],"memory"`, `,`+strings.TrimSuffix(dup, ",")+`],"memory"`, 1)
	if err := os.WriteFile(path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(cfg)
	if err != nil {
		t.Fatalf("单条坏键导致整份文件被拒: %v", err)
	}
	defer l2.Close()
	if !l2.IsBanned("192.0.2.7") {
		t.Fatal("合法条目丢失（重复键应保留第一条）")
	}
	_ = time.Second
	_ = fmt.Sprint()
}
