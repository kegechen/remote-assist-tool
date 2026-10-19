package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/proto"
)

func TestBootstrapCallToolNotConnectedError(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	_, err := b.CallTool(context.Background(), "read_file", json.RawMessage(`{"path":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "not_connected") {
		t.Fatalf("expected not_connected error, got %v", err)
	}
}

func TestBootstrapConnectRequiresCode(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	_, err := b.CallTool(context.Background(), "connect", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "code required") {
		t.Fatalf("expected code required error, got %v", err)
	}
}

func TestBootstrapConnectReusesMatchingActiveSession(t *testing.T) {
	cfg := &Config{ServerAddr: "relay.example"}
	b := NewHelpMCPBootstrap(cfg)
	activeClient := NewClient(cfg)
	want := connectResult{
		Connected:    true,
		SessionID:    "session-existing",
		Server:       "relay.example:8443",
		P2P:          true,
		PeerVersion:  "peer-version",
		HelpVersion:  "help-version",
		ProtoVersion: proto.ToolProtocolVersionV2,
	}
	b.help = &HelpMode{client: activeClient}
	b.bridge = mcp.NewBridge(nil, proto.Session{})
	// activeTarget 只由 doConnect 写入，记的是归一化后的生效下限，默认即 DefaultMinProto。
	b.activeTarget = connectTarget{Code: "ABCDEF", Server: "relay.example:8443", MinProto: proto.DefaultMinProto}
	b.activeResult = want

	raw, err := b.doConnect(context.Background(), json.RawMessage(`{"code":"ABCD-EF"}`))
	if err != nil {
		t.Fatalf("重复 connect 应复用活动会话: %v", err)
	}
	var got connectResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got != want {
		t.Fatalf("result=%+v, want %+v", got, want)
	}
	if activeClient.IsClosed() {
		t.Fatal("重复 connect 关闭了仍健康的 client")
	}
}

// 复用判断看的是「现有会话满不满足本次下限」，下限只是放宽不是指定版本：
//   - 现有 v2 会话：带 min_proto="2" 或 "1" 重复 connect 都应复用，不该拆掉健康会话；
//   - 现有 v1 会话：不带 min_proto 的 connect 下限是 v2，必须重新握手，不能复用。
func TestBootstrapConnectReuseRespectsMinProtoFloor(t *testing.T) {
	setup := func(version, minProto string) (*HelpMCPBootstrap, *Client) {
		cfg := &Config{ServerAddr: "127.0.0.1:1"}
		b := NewHelpMCPBootstrap(cfg)
		active := NewClient(cfg)
		b.help = &HelpMode{client: active}
		b.bridge = mcp.NewBridge(nil, proto.Session{})
		b.activeTarget = connectTarget{Code: "ABCDEF", Server: "127.0.0.1:1", MinProto: minProto}
		b.activeResult = connectResult{Connected: true, SessionID: "session-existing", ProtoVersion: version}
		b.lastCode, b.lastServer, b.lastMinProto = "ABCD-EF", "127.0.0.1:1", minProto
		return b, active
	}

	for _, mp := range []string{"2", "1"} {
		b, active := setup(proto.ToolProtocolVersionV2, proto.DefaultMinProto)
		raw, _ := json.Marshal(map[string]any{"code": "ABCD-EF", "min_proto": mp})
		if _, err := b.doConnect(context.Background(), raw); err != nil {
			t.Fatalf("现有 v2 会话满足 min_proto=%s，应复用: %v", mp, err)
		}
		if active.IsClosed() {
			t.Fatalf("min_proto=%s：复用路径不应关闭健康的 client", mp)
		}
	}

	b, active := setup(proto.ToolProtocolVersionV1, proto.ToolProtocolVersionV1)
	_, err := b.doConnect(context.Background(), json.RawMessage(`{"code":"ABCD-EF"}`))
	if err == nil || !strings.Contains(err.Error(), "relay connect failed") {
		t.Fatalf("现有 v1 会话不满足默认下限，应重新连接（拨 127.0.0.1:1 必失败），实际: %v", err)
	}
	if !active.IsClosed() {
		t.Fatal("重新连接时旧会话应被拆除")
	}
	// 新意图握手失败后，缓存参数必须作废：否则传输中的自动重连会用旧的 min_proto="1"
	// 把用户刚以默认下限否决掉的 v1 会话悄悄连回去。
	if b.lastMinProto != "" || b.lastCode != "" {
		t.Fatalf("新连接意图失败后缓存应清空，实际 lastCode=%q lastMinProto=%q", b.lastCode, b.lastMinProto)
	}
	if err := b.reconnect(context.Background()); err == nil || !strings.Contains(err.Error(), "no cached assist code") {
		t.Fatalf("缓存清空后自动重连不应再连回旧会话，实际: %v", err)
	}
}

// 旧会话已被 teardown 拆掉（b.help == nil）时，调用方的新意图同样要让缓存作废：
// 否则这次 connect 失败后，传输中的自动重连会用旧的 min_proto="1" 悄悄连回 v1。
// 与缓存相同的重试则保留缓存，自动重连仍有依据。
func TestBootstrapConnectNewIntentInvalidatesCacheWithoutSession(t *testing.T) {
	newB := func() *HelpMCPBootstrap {
		b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
		b.lastCode, b.lastServer, b.lastMinProto = "ABCD-EF", "127.0.0.1:1", "1"
		return b
	}

	b := newB()
	if _, err := b.doConnect(context.Background(), json.RawMessage(`{"code":"ABCD-EF","server":"127.0.0.1:1"}`)); err == nil {
		t.Fatal("拨 127.0.0.1:1 应失败")
	}
	if b.lastCode != "" || b.lastMinProto != "" {
		t.Fatalf("新意图（默认下限）失败后缓存应清空，实际 lastCode=%q lastMinProto=%q", b.lastCode, b.lastMinProto)
	}

	b = newB()
	if _, err := b.doConnect(context.Background(), json.RawMessage(`{"code":"ABCD-EF","server":"127.0.0.1:1","min_proto":"1"}`)); err == nil {
		t.Fatal("拨 127.0.0.1:1 应失败")
	}
	if b.lastCode != "ABCD-EF" || b.lastMinProto != "1" {
		t.Fatalf("与缓存相同的重试不应清空缓存，实际 lastCode=%q lastMinProto=%q", b.lastCode, b.lastMinProto)
	}
}

// 复用命中时若本次下限更严格，记录要收紧：之后的自动重连以最近一次意图为准。
func TestBootstrapConnectReuseTightensMinProto(t *testing.T) {
	cfg := &Config{ServerAddr: "127.0.0.1:1"}
	b := NewHelpMCPBootstrap(cfg)
	b.help = &HelpMode{client: NewClient(cfg)}
	b.bridge = mcp.NewBridge(nil, proto.Session{})
	b.activeTarget = connectTarget{Code: "ABCDEF", Server: "127.0.0.1:1", MinProto: proto.ToolProtocolVersionV1}
	b.activeResult = connectResult{Connected: true, ProtoVersion: proto.ToolProtocolVersionV2}
	b.lastCode, b.lastServer, b.lastMinProto = "ABCD-EF", "127.0.0.1:1", "1"

	if _, err := b.doConnect(context.Background(), json.RawMessage(`{"code":"ABCD-EF","server":"127.0.0.1:1"}`)); err != nil {
		t.Fatalf("现有 v2 会话满足默认下限，应复用: %v", err)
	}
	if b.lastMinProto != "" || b.activeTarget.MinProto != proto.DefaultMinProto {
		t.Fatalf("复用时应收紧到本次下限，实际 lastMinProto=%q activeTarget.MinProto=%q", b.lastMinProto, b.activeTarget.MinProto)
	}
}

// 排队等锁的自动重连必须用拿到锁那一刻的缓存，而不是排队前拍的快照：多个传输并发断线时，
// 后到的重连会在 connectMu 上等，期间调用方的新意图可能已经作废了缓存。
// 这里先占住 connectMu 让重连排队，再作废缓存（模拟新意图），放锁后重连不得再拨号。
func TestReconnectReadsCacheUnderConnectLock(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	b.lastCode, b.lastServer, b.lastMinProto = "ABCD-EF", "127.0.0.1:1", "1"

	b.connectMu.Lock()
	done := make(chan error, 1)
	go func() { done <- b.reconnect(context.Background()) }()
	time.Sleep(100 * time.Millisecond) // 让重连走到等锁处（若在锁外拍快照，此时已拍完）

	b.mu.Lock()
	b.lastCode, b.lastServer, b.lastNoAuth, b.lastMinProto = "", "", false, ""
	b.mu.Unlock()
	b.connectMu.Unlock()

	err := <-done
	if err == nil || !strings.Contains(err.Error(), "no cached assist code") {
		t.Fatalf("缓存已被作废，排队的重连不应再拨号，实际: %v", err)
	}
}

// 已取消的传输不该再触发拨号：重连在锁上排队期间调用方可能已经放弃。
func TestReconnectSkipsDialWhenCancelled(t *testing.T) {
	b := NewHelpMCPBootstrap(&Config{ServerAddr: "127.0.0.1:1"})
	b.lastCode, b.lastServer = "ABCD-EF", "127.0.0.1:1"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.reconnect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 已取消应直接返回 context.Canceled，实际: %v", err)
	}
}
