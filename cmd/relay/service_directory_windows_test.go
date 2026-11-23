//go:build windows

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// config.json 的 directory_url / directory_refresh 原样转成 relay 的旗标，且转出来的参数能被 relay 解析。
func TestServiceConfigDirectoryFlags(t *testing.T) {
	t.Setenv("REMOTE_RELAY_LIMITS_FILE", "")
	paths := windowsServicePaths{dataDir: `C:\ProgramData\RemoteAssistRelay`, configFile: `C:\ProgramData\RemoteAssistRelay\config.json`,
		certsDir: `C:\ProgramData\RemoteAssistRelay\certs`, auditFile: `C:\ProgramData\RemoteAssistRelay\logs\audit.jsonl`}
	cfg := defaultWindowsServiceConfig(paths)
	if a := strings.Join(cfg.relayArgs(), " "); strings.Contains(a, "directory") {
		t.Fatalf("默认配置不应开启节点模式: %s", a)
	}

	const u = "https://relay-list.example.test/v1/directory"
	cfg.DirectoryURL, cfg.DirectoryRefresh = u, "10m"
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	args := cfg.relayArgs()
	if a := strings.Join(args, " "); !strings.Contains(a, "--directory-url "+u) || !strings.Contains(a, "--directory-refresh 10m") {
		t.Fatalf("节点模式参数未传给 relay: %s", a)
	}
	opts, err := parseRelayOptions(args, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("服务配置转出的参数 relay 解析不了: %v", err)
	}
	if opts.directoryURL != u || opts.directoryRefresh != 10*time.Minute {
		t.Fatalf("解析结果 %q %v", opts.directoryURL, opts.directoryRefresh)
	}
}

func TestServiceConfigRejectsBadDirectory(t *testing.T) {
	paths := windowsServicePaths{dataDir: `C:\ProgramData\RemoteAssistRelay`, configFile: `C:\ProgramData\RemoteAssistRelay\config.json`,
		certsDir: `C:\ProgramData\RemoteAssistRelay\certs`, auditFile: `C:\ProgramData\RemoteAssistRelay\logs\audit.jsonl`}
	cases := []struct {
		name string
		mod  func(*windowsServiceConfig)
	}{
		{"http 地址", func(c *windowsServiceConfig) { c.DirectoryURL = "http://relay-list.example.test/v1/directory" }},
		{"没有 state_dir", func(c *windowsServiceConfig) { c.StateDir = "" }},
		{"plain", func(c *windowsServiceConfig) { c.Plain = true }},
		{"no_auth", func(c *windowsServiceConfig) { c.NoAuth = true }},
		{"码长不是 12", func(c *windowsServiceConfig) { c.CodeLength = 10 }},
		{"拉取间隔写错", func(c *windowsServiceConfig) { c.DirectoryRefresh = "abc" }},
		{"拉取间隔太短", func(c *windowsServiceConfig) { c.DirectoryRefresh = "30s" }},
	}
	for _, tc := range cases {
		cfg := defaultWindowsServiceConfig(paths)
		cfg.DirectoryURL = "https://relay-list.example.test/v1/directory"
		tc.mod(&cfg)
		if cfg.validate() == nil {
			t.Errorf("%s: validate 应报错", tc.name)
		}
	}
}
