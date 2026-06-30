package client

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestCopyToClipboardRoundTrip 验证 copyToClipboard 真把文本写进系统剪贴板。
// 仅 Windows 跑（用 Get-Clipboard 读回）；其它平台缺 xclip/pbcopy 时跳过。
func TestCopyToClipboardRoundTrip(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("clipboard round-trip 仅在 windows 验证")
	}
	want := "WXYZ-9A8B7C"
	if err := copyToClipboard(want); err != nil {
		t.Fatalf("copyToClipboard: %v", err)
	}
	out, err := exec.Command("powershell", "-NoProfile", "-Command", "Get-Clipboard").Output()
	if err != nil {
		t.Fatalf("Get-Clipboard: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("clipboard got %q want %q", got, want)
	}
}
