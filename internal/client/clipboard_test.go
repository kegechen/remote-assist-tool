package client

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// TestCopyToClipboardRoundTrip 验证 copyToClipboard 真把文本写进系统剪贴板。
// Windows 用 UTF-16 原生格式写入，再让 PowerShell 以 Base64 返回 UTF-16 字节，避免
// 测试读取路径本身经过控制台代码页、把“读乱码”误判成“写乱码”。
func TestCopyToClipboardRoundTrip(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("clipboard round-trip 仅在 windows 验证")
	}
	want := "请通过 remote-debug MCP 连接：\r\n协助码: WXYZ-9A8B7C\r\n本机标识: Windows 11 中文主机"
	if err := copyToClipboard(want); err != nil {
		t.Fatalf("copyToClipboard: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command",
		"$value = Get-Clipboard -Raw; [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($value))").Output()
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("Get-Clipboard timed out: %v", ctx.Err())
		}
		t.Fatalf("Get-Clipboard: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("decode clipboard base64: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("clipboard UTF-16 byte count is odd: %d", len(raw))
	}
	codeUnits := make([]uint16, len(raw)/2)
	for i := range codeUnits {
		codeUnits[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	if got := string(utf16.Decode(codeUnits)); got != want {
		t.Fatalf("clipboard got %q want %q", got, want)
	}
}

func TestSelectClipboardCommandUsesUTF8Targets(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		available map[string]bool
		want      clipboardCommand
	}{
		{"macOS", "darwin", nil, clipboardCommand{name: "pbcopy", env: []string{"LC_CTYPE=UTF-8"}}},
		{"Wayland", "linux", map[string]bool{"wl-copy": true}, clipboardCommand{name: "wl-copy", args: []string{"--type", "text/plain;charset=utf-8"}}},
		{"X11 xclip", "linux", map[string]bool{"xclip": true}, clipboardCommand{name: "xclip", args: []string{"-selection", "clipboard", "-target", "UTF8_STRING"}}},
		{"X11 xsel", "linux", map[string]bool{"xsel": true}, clipboardCommand{name: "xsel", args: []string{"--clipboard", "--input"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectClipboardCommand(test.goos, func(name string) bool { return test.available[name] })
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("command = %#v, want %#v", got, test.want)
			}
		})
	}
}
