package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// connect 的 schema 明写着 "'http://host:port' is rejected"。承诺了就得真拒绝：
// 不拦的话 NormalizeServerAddr 不会报错，只会把它揉成更离谱的形状再去拨号，
// 报错完全指不到根因——而"让 connect 的失败指得到根因"正是这批改动的目的。
func TestValidateServerAddrRejectsURLScheme(t *testing.T) {
	bad := []string{
		"http://113.44.139.100:8443",
		"https://relay.example.com",
		"https://relay.example.com:8443",
		"tcp://1.2.3.4:8443",
		"113.44.139.100:8443/path",
		"1.2.3.4:8443?x=1",
	}
	for _, s := range bad {
		if err := ValidateServerAddr(s); err == nil {
			t.Errorf("ValidateServerAddr(%q) 应报错，实际通过", s)
		}
	}
	good := []string{
		"113.44.139.100:8443",
		"113.44.139.100",
		"relay.example.com:8443",
		"localhost:8443",
		"[::1]:8443",
		"", // 空 = 用默认 relay，由调用方跳过校验
	}
	for _, s := range good {
		if err := ValidateServerAddr(s); err != nil {
			t.Errorf("ValidateServerAddr(%q) 应通过，实际报错: %v", s, err)
		}
	}
}

// 钉住"揉成垃圾"这个前提：一旦 NormalizeServerAddr 哪天自己会处理 scheme 了，
// 这条会失败，提醒回来重新审视 ValidateServerAddr 还有没有必要。
func TestNormalizeServerAddrDoesNotUnderstandURLScheme(t *testing.T) {
	got := NormalizeServerAddr("http://1.2.3.4:8443")
	if got == "1.2.3.4:8443" {
		t.Fatal("NormalizeServerAddr 现在会剥掉 scheme 了——请重新评估 ValidateServerAddr 的必要性")
	}
	// 当前实际行为：含冒号的 host 被当成 IPv6 加方括号 → 一个必然拨号失败的地址
	if !strings.Contains(got, "http://") {
		t.Fatalf("行为变了，got %q", got)
	}
}

// doConnect 必须在拨号前就把带 scheme 的地址挡下来，并且错误要说人话。
func TestDoConnectRejectsServerWithScheme(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	raw := json.RawMessage(`{"code":"ABCD-EFGHIJ","server":"http://113.44.139.100:8443"}`)
	_, err := b.doConnect(context.Background(), raw)
	if err == nil {
		t.Fatal("带 http:// 的 server 应报错，实际通过")
	}
	if !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("错误应告诉调用方正确写法，实际: %v", err)
	}
}
