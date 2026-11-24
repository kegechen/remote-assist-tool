package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// --blocklist-allow：启动参数里的放行名单（IP / CIDR，逗号分隔），默认空。
func TestBlocklistAllowFlag(t *testing.T) {
	def, err := parseRelayOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocklistConfigFromOptions(def).StaticAllow) != 0 {
		t.Fatal("默认不放行任何地址")
	}

	opts, err := parseRelayOptions([]string{"--blocklist-allow", "198.51.100.7, 203.0.113.0/24,2001:db8::/32"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := blocklistConfigFromOptions(opts)
	var got []string
	for _, p := range cfg.StaticAllow {
		got = append(got, p.String())
	}
	if strings.Join(got, ",") != "198.51.100.7/32,203.0.113.0/24,2001:db8::/32" {
		t.Fatalf("StaticAllow = %v", got)
	}

	// 写错立刻失败：不能带着错误的放行配置悄悄运行（即使黑名单被关闭，也一样校验）。
	for _, args := range [][]string{
		{"--blocklist-allow", "not-an-ip"},
		{"--blocklist-allow", "198.51.100.7,,203.0.113.1"},
		{"--blocklist-allow", "0.0.0.0/0"},
		{"--blocklist-allow", "198.51.100.7/33"},
		{"--blocklist-allow", "dev-0123456789abcdef01234567"},
		{"--blocklist-enabled=false", "--blocklist-allow", "not-an-ip"},
	} {
		_, err := parseRelayOptions(args, &bytes.Buffer{})
		if err == nil {
			t.Errorf("parseRelayOptions(%q) 应报错", args)
		} else if !strings.Contains(err.Error(), "blocklist-allow") {
			t.Errorf("parseRelayOptions(%q) 的错误应点明是哪个参数：%v", args, err)
		}
	}
}

// 放行名单经 openBlocklist 进入名单：命中的来源不被封、不被拒。
func TestOpenBlocklistAppliesAllow(t *testing.T) {
	opts, err := parseRelayOptions([]string{"--blocklist-db=", "--blocklist-allow", "198.51.100.0/24"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	bl, recs := openBlocklist(opts)
	if bl == nil {
		t.Fatal("名单应已开启")
	}
	defer func() { _ = bl.Close(); _ = recs.Close() }()
	for i := 0; i < 5*opts.blocklistThreshold; i++ {
		if bl.RecordViolation("198.51.100.7", "join_failed", "x") {
			t.Fatal("放行范围内的来源不应被封")
		}
	}
	if !bl.CheckAccept("198.51.100.7") {
		t.Fatal("放行范围内的来源应放行")
	}
	if _, err := bl.Ban("198.51.100.9", time.Hour, false, "op", ""); err == nil {
		t.Fatal("放行范围内不能手动封禁")
	}
}
