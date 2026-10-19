package client

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// connect 的参数名写错时必须显式报错。静默忽略会让 connect 悄悄连上内置默认 relay，
// 超时错误里只有默认地址、指不到根因——臆造 relay_url 的现场就是这么来的。
// cfg 用 127.0.0.1:1：万一未知字段没被拦住，拨号会立刻 refuse 而不是挂住测试。
func TestDoConnectRejectsUnknownFields(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	raw := json.RawMessage(`{"code":"ABCD-EFGHIJ","relay_url":"http://113.44.139.100:8443","session_name":"demo-session"}`)

	_, err := b.doConnect(context.Background(), raw)
	if err == nil {
		t.Fatal("未知字段 relay_url 应报错，实际通过")
	}
	if !strings.Contains(err.Error(), "relay_url") {
		t.Fatalf("错误应指名违规字段，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "server") {
		t.Fatalf("错误应提示正确参数名 server，实际: %v", err)
	}
}

// reconnect 走 Marshal(connectArgs) → doConnect 的回环，DisallowUnknownFields
// 不能把它自己产出的 JSON 判成非法（否则传输中自动重连全线失效）。
func TestConnectArgsRoundTripSurvivesStrictDecode(t *testing.T) {
	for _, a := range []connectArgs{
		{Code: "ABCD-EFGHIJ"},
		{Code: "ABCD-EFGHIJ", Server: "113.44.139.100:8443"},
		{Code: "", NoAuth: true},
		{Code: "ABCD-EFGHIJ", MinProto: "1"},
	} {
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal %+v: %v", a, err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var got connectArgs
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("strict decode 拒绝了自产 JSON %s: %v", raw, err)
		}
		if got != a {
			t.Fatalf("round-trip 不一致: got %+v want %+v", got, a)
		}
	}
}

// min_proto 是一道安全闸，非法取值必须拒绝，不能静默回落到默认值：调用方写了
// min_proto="3" 却连上了 v2，会以为自己设了什么。校验发生在拨号之前。
func TestDoConnectRejectsInvalidMinProto(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	for _, bad := range []string{"3", "0", "abc", "v1"} {
		raw, _ := json.Marshal(map[string]any{"code": "ABCD-EFGHIJ", "min_proto": bad})
		_, err := b.doConnect(context.Background(), raw)
		if err == nil || !strings.Contains(err.Error(), "bad args: min_proto") {
			t.Fatalf("min_proto=%q 应报 bad args，实际: %v", bad, err)
		}
	}
}

// 传输中自动重连必须沿用上一次 connect 的 min_proto。漏掉的话，靠 min_proto="1"
// 连上旧版 share 的会话一断线，重连就退回默认值被拒，续传全线失败。
// 这里把缓存值设成非法值：它若被带进 doConnect，会在拨号前就报 bad args；
// 没带上的话会去拨 127.0.0.1:1，报的是 relay connect failed。
func TestReconnectCarriesMinProto(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	b.lastCode = "ABCD-EFGHIJ"
	b.lastMinProto = "bogus"
	err := b.reconnect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad args: min_proto") {
		t.Fatalf("reconnect 应把缓存的 min_proto 带进 doConnect，实际: %v", err)
	}
}
