package client

import (
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
)

func TestFormatShareInstructions(t *testing.T) {
	expiresAt := time.Date(2026, 8, 18, 15, 21, 15, 0, time.Local)
	cfg := &Config{
		ServerAddr:   "23.95.78.14:8443",
		UseTLS:       true,
		InsecureSkip: true,
	}

	got := formatShareInstructions("ABCD123456", "Windows 11 amd64 / host-01", cfg, nil, expiresAt)
	wants := []string{
		"请通过 remote-debug MCP 连接并协助排查此设备",
		"协助码: ABCD-1234-56",
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

// 节点模式（spec 6.7）：「中转服务」一行格式不变（MCP 靠它解析），只换传输说明；第三方节点紧接着一行运营者。
func TestFormatShareInstructionsOnNode(t *testing.T) {
	cfg := &Config{ServerAddr: "relay-w.example.test:8443", UseTLS: true, InsecureSkip: true}
	node := &directory.Node{NodeCert: directory.NodeCert{Prefix: "W", Operator: "Example-Ops", Official: true}}

	got := formatShareInstructions("W000000001", "u@h", cfg, node, time.Time{})
	want := "中转服务: relay-w.example.test:8443 (TLS，证书已按节点列表校验)\n有效期至: "
	if !strings.Contains(got, want) || strings.Contains(got, "节点运营者") {
		t.Fatalf("官方节点的协助信息不对:\n%s", got)
	}

	node.Official = false
	got = formatShareInstructions("W000000001", "u@h", cfg, node, time.Time{})
	want = "中转服务: relay-w.example.test:8443 (TLS，证书已按节点列表校验)\n节点运营者: Example-Ops（非官方节点）\n有效期至: "
	if !strings.Contains(got, want) {
		t.Fatalf("第三方节点的协助信息不对:\n%s", got)
	}
}

// 升级链接（spec 6.6）：只有第三方节点换成内置的官方地址。
func TestUpdateLinks(t *testing.T) {
	info := proto.UpdateInfo{LatestVersion: "v99.0.0", DownloadURL: "https://downloads.example.test/x", ReleasePage: "https://downloads.example.test/r"}
	for _, tc := range []struct {
		name     string
		node     *directory.Node
		wantDL   string
		wantPage string
	}{
		{"显式模式", nil, info.DownloadURL, info.ReleasePage},
		{"官方节点", &directory.Node{NodeCert: directory.NodeCert{Official: true}}, info.DownloadURL, info.ReleasePage},
		{"第三方节点", &directory.Node{NodeCert: directory.NodeCert{Official: false}}, update.DefaultDownloadURL, update.DefaultReleasePage},
	} {
		dl, page := updateLinks(tc.node, info)
		if dl != tc.wantDL || page != tc.wantPage {
			t.Errorf("%s: updateLinks = (%q, %q)，想要 (%q, %q)", tc.name, dl, page, tc.wantDL, tc.wantPage)
		}
	}
}
