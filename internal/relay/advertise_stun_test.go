package relay

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// registerJoinFrames 走一遍注册 + 加入，返回两端收到的原始响应帧。
func registerJoinFrames(t *testing.T, cfg *Config) (reg, join proto.Message) {
	t.Helper()
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	shareConn := &reuseProtocolConn{remoteAddr: "203.0.113.7:1"}
	m, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "1.2.0", SupportsUpdateNotice: true})
	if srv.handleMessage(&ClientConn{ID: "share", Conn: shareConn}, m) {
		t.Fatal("register closed")
	}
	frames := shareConn.messages(t)
	var registered proto.RegisterResponse
	if err := proto.DecodePayload(&frames[0], &registered); err != nil {
		t.Fatal(err)
	}
	helpConn := &reuseProtocolConn{remoteAddr: "198.51.100.9:2"}
	m, _ = proto.NewMessage(proto.MsgJoinRequest, &proto.JoinRequest{Code: registered.Code, Version: "1.2.0", SupportsUpdateNotice: true})
	if srv.handleMessage(&ClientConn{ID: "help", Conn: helpConn}, m) {
		t.Fatal("join closed")
	}
	return frames[0], helpConn.messages(t)[0]
}

// 配置了 --advertise-stun 时注册与加入响应都下发；未配置时 JSON 里不出现该字段（与旧版逐字节一致）。
func TestAdvertiseSTUNInRegisterAndJoin(t *testing.T) {
	want := []string{"23.95.78.14:3478", "stun.chat.bilibili.com:3478", "stun.cloudflare.com:3478"}
	reg, join := registerJoinFrames(t, &Config{CodeTTL: time.Minute, AdvertiseSTUN: want})
	var r proto.RegisterResponse
	var j proto.JoinResponse
	_ = proto.DecodePayload(&reg, &r)
	_ = proto.DecodePayload(&join, &j)
	if !reflect.DeepEqual(r.STUNServers, want) || !reflect.DeepEqual(j.STUNServers, want) {
		t.Fatalf("register=%q join=%q, want %q", r.STUNServers, j.STUNServers, want)
	}

	reg, join = registerJoinFrames(t, &Config{CodeTTL: time.Minute})
	for _, f := range []proto.Message{reg, join} {
		if strings.Contains(string(f.Payload), "stun_servers") {
			t.Fatalf("unconfigured relay must not send stun_servers: %s", f.Payload)
		}
	}
}

func TestValidateAdvertiseSTUN(t *testing.T) {
	for _, bad := range [][]string{
		{"no-port"}, {":3478"}, {"host:0"}, {"host:70000"}, {"host:abc"}, {"a b:3478"},
		{"a:1", "b:1", "c:1", "d:1", "e:1"},
	} {
		if _, err := NewServer(&Config{CodeTTL: time.Minute, AdvertiseSTUN: bad}); err == nil {
			t.Errorf("AdvertiseSTUN %q accepted", bad)
		}
	}
	if _, err := NewServer(&Config{CodeTTL: time.Minute, AdvertiseSTUN: []string{"23.95.78.14:3478", "[2001:db8::1]:3478", "stun.miwifi.com:3478"}}); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
}
