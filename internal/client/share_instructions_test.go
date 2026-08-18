package client

import (
	"strings"
	"testing"
	"time"
)

func TestFormatShareInstructions(t *testing.T) {
	expiresAt := time.Date(2026, 8, 18, 15, 21, 15, 0, time.Local)
	cfg := &Config{
		ServerAddr:   "23.95.78.14:8443",
		UseTLS:       true,
		InsecureSkip: true,
	}

	got := formatShareInstructions("ABCD123456", "Windows 11 amd64 / host-01", cfg, expiresAt)
	wants := []string{
		"请通过 remote-debug MCP 连接并协助排查此设备",
		"协助码: ABCD-123456",
		"本机标识: Windows 11 amd64 / host-01",
		"中转服务: 23.95.78.14:8443 (TLS，跳过证书校验)",
		"有效期至: 2026-08-18 15:21:15",
		mcpSetupGuideURL,
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("instructions missing %q:\n%s", want, got)
		}
	}
}
