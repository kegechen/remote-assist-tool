//go:build windows

package main

import (
	"strings"
	"testing"
)

func TestServiceConfigCodeFlags(t *testing.T) {
	cfg := windowsServiceConfig{
		ListenAddr: ":8443", CodeTTL: "30m", CodeLength: 12,
		CertsDir: `C:\relay\certs`, AuditLog: `C:\relay\audit.log`, Plain: true,
	}
	if a := strings.Join(cfg.relayArgs(), " "); strings.Contains(a, "code-max-age") || strings.Contains(a, "legacy-code-expiry") {
		t.Fatalf("默认配置不应带新开关: %s", a)
	}
	cfg.CodeMaxAge, cfg.LegacyCodeExpiry = "24h", true
	a := strings.Join(cfg.relayArgs(), " ")
	if !strings.Contains(a, "--code-max-age 24h") || !strings.Contains(a, "--legacy-code-expiry") {
		t.Fatalf("新开关未传给 relay: %s", a)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	cfg.CodeMaxAge = "abc"
	if cfg.validate() == nil {
		t.Fatal("非法 code_max_age 应校验失败")
	}
}

// 默认服务配置：max-age 24h（非零）且密钥落在数据目录 state 下；"0" 才是显式关闭。
func TestDefaultServiceConfigCodeMaxAgeAndStateDir(t *testing.T) {
	paths := windowsServicePaths{dataDir: `C:\ProgramData\RemoteAssistRelay`, configFile: `C:\ProgramData\RemoteAssistRelay\config.json`,
		certsDir: `C:\ProgramData\RemoteAssistRelay\certs`, auditFile: `C:\ProgramData\RemoteAssistRelay\logs\audit.jsonl`}
	cfg := defaultWindowsServiceConfig(paths)
	if cfg.CodeMaxAge != "24h" {
		t.Fatalf("默认 code_max_age 应为 24h，得 %q", cfg.CodeMaxAge)
	}
	a := strings.Join(cfg.relayArgs(), " ")
	if !strings.Contains(a, "--code-max-age 24h") || !strings.Contains(a, `--state-dir C:\ProgramData\RemoteAssistRelay\state`) {
		t.Fatalf("默认参数缺少 max-age/state-dir: %s", a)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	cfg.CodeMaxAge = "0"
	if a := strings.Join(cfg.relayArgs(), " "); !strings.Contains(a, "--code-max-age 0") {
		t.Fatalf("显式 0 应原样传给 relay: %s", a)
	}
	cfg.StateDir = "relative"
	if cfg.validate() == nil {
		t.Fatal("state_dir 应要求绝对路径")
	}
}
