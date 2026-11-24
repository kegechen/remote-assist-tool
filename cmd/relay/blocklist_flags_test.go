package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBlocklistFlagDefaults(t *testing.T) {
	opts, err := parseRelayOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.blocklistEnabled {
		t.Error("blocklist-enabled 默认应为 true（替代 fail2ban relay-scan）")
	}
	if opts.blocklistDB != defaultBlocklistDB("default") {
		t.Errorf("blocklist-db 默认=%q", opts.blocklistDB)
	}
	cfg := blocklistConfigFromOptions(opts)
	if cfg.Path != opts.blocklistDB || cfg.Threshold != 20 || cfg.Window != 10*time.Minute ||
		cfg.BanBase != time.Hour || cfg.BanMax != 24*time.Hour || cfg.OffenseDecay != 7*24*time.Hour ||
		cfg.UnbanGrace != 24*time.Hour || cfg.MaxTracked != 4096 {
		t.Fatalf("默认配置不对: %+v", cfg)
	}
}

func TestBlocklistFlagsParseAndValidate(t *testing.T) {
	db := filepath.Join(t.TempDir(), "b2.json")
	opts, err := parseRelayOptions([]string{
		"--blocklist-db", db,
		"--blocklist-threshold", "30", "--blocklist-window", "5m",
		"--blocklist-ban-base", "30m", "--blocklist-ban-max", "12h",
		"--blocklist-offense-decay", "48h", "--blocklist-unban-grace", "2h",
		"--blocklist-max-tracked", "100",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := blocklistConfigFromOptions(opts)
	if opts.blocklistDB != db || cfg.Threshold != 30 || cfg.Window != 5*time.Minute ||
		cfg.BanBase != 30*time.Minute || cfg.BanMax != 12*time.Hour || cfg.OffenseDecay != 48*time.Hour ||
		cfg.UnbanGrace != 2*time.Hour || cfg.MaxTracked != 100 {
		t.Fatalf("解析不对: %+v", cfg)
	}
	memOnly, err := parseRelayOptions([]string{"--blocklist-db="}, &bytes.Buffer{})
	if err != nil || memOnly.blocklistDB != "" {
		t.Fatalf("显式空路径应为纯内存: %q %v", memOnly.blocklistDB, err)
	}
	off, err := parseRelayOptions([]string{"--blocklist-enabled=false"}, &bytes.Buffer{})
	if err != nil || off.blocklistEnabled {
		t.Fatalf("应可显式关闭: %v %v", off.blocklistEnabled, err)
	}
	for _, args := range [][]string{
		{"--blocklist-threshold", "0"},
		{"--blocklist-window", "0s"},
		{"--blocklist-ban-base", "-1h"},
		{"--blocklist-ban-base", "2h", "--blocklist-ban-max", "1h"},
		{"--blocklist-offense-decay", "0s"},
		{"--blocklist-unban-grace", "0s"},
		{"--blocklist-max-tracked", "0"},
	} {
		if _, err := parseRelayOptions(args, &bytes.Buffer{}); err == nil {
			t.Errorf("parseRelayOptions(%q) 应报错", args)
		}
	}
}

// 封禁记录文件由 --blocklist-db 派生（不新增标志），与名单共用同一套开关；记录在名单关闭后再关。
func TestOpenBlocklistDerivesRecordsFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "bl.json")
	opts, err := parseRelayOptions([]string{"--blocklist-db", db}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	bl, recs := openBlocklist(opts)
	if bl == nil || recs == nil || !bl.Persistent() || !recs.Persistent() {
		t.Fatalf("名单与记录都应以持久化模式打开: %v %v", bl, recs)
	}
	if _, err := bl.Ban("198.51.100.1", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(recs.Snapshot()); n != 1 {
		t.Fatalf("名单应接上记录，实际 %d 条", n)
	}
	if err := bl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bl-records.json")); err != nil {
		t.Fatalf("记录文件应写在名单文件旁边: %v", err)
	}

	memOnly, err := parseRelayOptions([]string{"--blocklist-db="}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	bl, recs = openBlocklist(memOnly)
	if bl == nil || recs == nil || recs.Persistent() {
		t.Fatal("名单纯内存时记录也应纯内存")
	}
	_ = bl.Close()
	_ = recs.Close()

	off, err := parseRelayOptions([]string{"--blocklist-enabled=false"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if bl, recs = openBlocklist(off); bl != nil || recs != nil {
		t.Fatal("名单关闭时不应打开记录")
	}
}

// S5：时长类参数下限为 1 分钟，跟踪上限不得过小。
func TestBlocklistFlagLowerBounds(t *testing.T) {
	for _, args := range [][]string{
		{"--blocklist-window", "30s"},
		{"--blocklist-ban-base", "30s"},
		{"--blocklist-unban-grace", "10s"},
		{"--blocklist-offense-decay", "59s"},
		{"--blocklist-max-tracked", "10"},
		{"--blocklist-max-tracked", "16384"}, // P1：超过上限会让持久化文件撑过加载上限
	} {
		if _, err := parseRelayOptions(args, &bytes.Buffer{}); err == nil {
			t.Errorf("parseRelayOptions(%q) 应报错", args)
		}
	}
}
