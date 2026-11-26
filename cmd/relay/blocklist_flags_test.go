package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/relay/blocklist"
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
	bl, dev, recs := openBlocklist(opts)
	if bl == nil || recs == nil || !bl.Persistent() || !recs.Persistent() {
		t.Fatalf("名单与记录都应以持久化模式打开: %v %v", bl, recs)
	}
	if _, err := bl.Ban("198.51.100.1", time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(recs.Snapshot()); n != 1 {
		t.Fatalf("名单应接上记录，实际 %d 条", n)
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
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
	bl, dev, recs = openBlocklist(memOnly)
	if bl == nil || dev == nil || recs == nil || dev.Persistent() || recs.Persistent() {
		t.Fatal("名单纯内存时设备名单与记录也应纯内存")
	}
	_ = dev.Close()
	_ = bl.Close()
	_ = recs.Close()

	off, err := parseRelayOptions([]string{"--blocklist-enabled=false"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if bl, dev, recs = openBlocklist(off); bl != nil || dev != nil || recs != nil {
		t.Fatal("名单关闭时不应打开设备名单与记录")
	}
}

// 设备名单（设计 2026-11-23 §3.5）：文件同样由 --blocklist-db 派生（blocklist.json → blocklist-devices.json），
// 不新增标志，参数与 IP 名单同值，与 IP 名单共用封禁记录。
func TestOpenBlocklistDerivesDeviceFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "bl.json")
	opts, err := parseRelayOptions([]string{"--blocklist-db", db, "--blocklist-threshold", "7"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	bl, dev, recs := openBlocklist(opts)
	if bl == nil || dev == nil || recs == nil || !dev.Persistent() {
		t.Fatalf("设备名单应以持久化模式打开: %v %v %v", bl, dev, recs)
	}
	key := dev.DeviceKey(blocklist.DeviceByClientID, "cid_test")
	if key == "" {
		t.Fatal("第二本名单应是设备名单")
	}
	if got := dev.Config().Threshold; got != 7 {
		t.Fatalf("设备名单参数应与 IP 名单同值，阈值 %d", got)
	}
	if _, err := dev.Ban(key, time.Hour, false, "op", ""); err != nil {
		t.Fatal(err)
	}
	snap := recs.Snapshot()
	if len(snap) != 1 || snap[0].Target != blocklist.TargetDevice {
		t.Fatalf("设备封禁应记进共用的封禁记录：%+v", snap)
	}
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recs.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "bl-devices.json"))
	if err != nil {
		t.Fatalf("设备名单文件应写在名单文件旁边: %v", err)
	}
	if !bytes.Contains(data, []byte(key)) {
		t.Fatal("设备名单文件里应有这条封禁")
	}
	if ipData, err := os.ReadFile(db); err == nil && bytes.Contains(ipData, []byte(key)) {
		t.Fatal("设备封禁不应写进 IP 名单文件")
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
