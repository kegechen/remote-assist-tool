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
