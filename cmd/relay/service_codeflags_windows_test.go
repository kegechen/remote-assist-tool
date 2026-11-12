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
