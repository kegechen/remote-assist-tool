package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/proto"
)

// 复审 329f7f5 之后补的一组：常驻读循环里的静默不归咎于 TCP、下行确认补发心跳、先拨 QUIC 却仍用 TCP 时说明原因、
// 静默按「多久一个字节都没收到」算，以及 MCP 的两个读循环（RunMCPMode、helpLink.relayLoop）同样会发现静默。

func heartbeatMsg() *proto.Message {
	return newMsg(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()})
}

// 连着之后很久收不到 relay 的数据：断网、切换网络、relay 掉电都是这样，分不出是不是发回的数据被丢掉。提示不能
// 断言「TCP 连接已经建立、被网络干扰」，单独这一次也不能据此记下这个地址的 TCP 收不到数据（之后 15 分钟每次都先拨
// QUIC）。
func TestReadLoopSilenceDoesNotBlameTCP(t *testing.T) {
	resetTCPStalls(t)
	shrinkDuration(t, &relaySilenceTimeout, 300*time.Millisecond)
	r := startTLSScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	s, _ := tlsShare(t, r.addr, "")
	if err := s.client.Connect(); err != nil {
		t.Fatal(err)
	}
	if s.client.transport != TransportTCP {
		t.Fatalf("前置：auto 模式下应走 TCP，实际 %q", s.client.transport)
	}
	err := mustFinishWithin(t, 5*time.Second, "ShareMode.waitSessionReady", func() error {
		_, err := s.waitSessionReady(nil)
		return err
	})
	var silent *relaySilentError
	if !errors.As(err, &silent) {
		t.Fatalf("relay 静默时应返回 *relaySilentError，实际: %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "被网络干扰") || strings.Contains(msg, "收不到它发回的数据") || !strings.Contains(msg, "连接可能已经断了") {
		t.Fatalf("常驻读循环里的静默不应归咎于 TCP，提示: %s", msg)
	}
	if !strings.Contains(msg, RelaySilentMarker) {
		t.Fatalf("提示里应有 RelaySilentMarker（GUI 靠它归类）: %s", msg)
	}
	if silent.fallback || tcpStalled(r.addr) || quicRetryNote(err) != "" {
		t.Fatalf("常驻读循环里的静默不应改走 QUIC：fallback=%v stalled=%v note=%q", silent.fallback, tcpStalled(r.addr), quicRetryNote(err))
	}
}

// 干扰从连上一阵子之后才开始丢 relay 发回的数据：每次重连、确认下行都通过，过一阵又收不到。单独一次静默不改传输
// （上一个用例），短时间内第二次就要记下这个地址的 TCP 收不到数据、之后先拨 QUIC，否则会一直在 TCP 上「连上、
// 静默、重连」。真断网时重连本身就连不上，不会走到第二次。只在 auto 模式下的 TCP 上算。
func TestRepeatedReadLoopSilenceMovesToQUIC(t *testing.T) {
	const addr = "relay.example.test:8443"
	cases := []struct {
		name      string
		cfg       Config
		transport string
		fallback  bool
	}{
		{name: "auto 下的 TCP", cfg: Config{UseTLS: true}, transport: TransportTCP, fallback: true},
		{name: "--transport tcp", cfg: Config{UseTLS: true, Transport: TransportTCP}, transport: TransportTCP},
		{name: "已经在 QUIC 上", cfg: Config{UseTLS: true}, transport: TransportQUIC},
		{name: "明文 relay 走不了 QUIC", cfg: Config{}, transport: TransportTCP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetTCPStalls(t)
			logs := captureLog(t)
			cfg := tc.cfg
			cfg.ServerAddr = addr
			c := NewClient(&cfg)
			c.transport = tc.transport
			first := c.silentErr(timeoutErr(), relaySilenceWhat, relaySilenceTimeout, false)
			if quicRetryNote(first) != "" || tcpStalled(addr) {
				t.Fatalf("第一次静默不应改传输：note=%q stalled=%v", quicRetryNote(first), tcpStalled(addr))
			}
			second := c.silentErr(timeoutErr(), relaySilenceWhat, relaySilenceTimeout, false)
			var silent *relaySilentError
			if !errors.As(second, &silent) {
				t.Fatalf("读超时应换成 *relaySilentError，实际: %v", second)
			}
			if silent.fallback != tc.fallback || tcpStalled(addr) != tc.fallback || (quicRetryNote(second) != "") != tc.fallback {
				t.Fatalf("第二次静默：fallback=%v stalled=%v note=%q，期望 %v", silent.fallback, tcpStalled(addr), quicRetryNote(second), tc.fallback)
			}
			if tc.fallback && !strings.Contains(logs.String(), "连上之后又一次收不到它发回的数据") {
				t.Fatalf("应说明为什么改先拨 QUIC，日志:\n%s", logs.String())
			}
		})
	}
}

// 两次静默隔得久（各是一次断网、换网络）不算反复：不改传输。
func TestReadLoopSilencesFarApartKeepTCP(t *testing.T) {
	resetTCPStalls(t)
	shrinkDuration(t, &tcpRepeatSilenceWindow, 50*time.Millisecond)
	const addr = "relay.example.test:8443"
	c := NewClient(&Config{ServerAddr: addr, UseTLS: true})
	c.transport = TransportTCP
	_ = c.silentErr(timeoutErr(), relaySilenceWhat, relaySilenceTimeout, false)
	time.Sleep(100 * time.Millisecond)
	err := c.silentErr(timeoutErr(), relaySilenceWhat, relaySilenceTimeout, false)
	if quicRetryNote(err) != "" || tcpStalled(addr) {
		t.Fatalf("隔得久的两次静默不应改传输：note=%q stalled=%v", quicRetryNote(err), tcpStalled(addr))
	}
}

// 等待时长小于心跳个数（只有测试会这样调）时不能因为补发的间隔是零而 panic。
func TestProbeDownlinkWithTinyTimeoutDoesNotPanic(t *testing.T) {
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	c := NewClient(&Config{ServerAddr: r.addr})
	t.Cleanup(c.Close)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	var silent *relaySilentError
	if err := c.probeDownlink(time.Nanosecond); !errors.As(err, &silent) {
		t.Fatalf("relay 不回心跳时应返回 *relaySilentError，实际: %v", err)
	}
}

// relay 对心跳限流时直接丢掉，不回也不断开（同一出口 IP 一下子来了很多连接时）：下行确认不能因为正好被丢的那一个
// 就判失败，要补发。
func TestProbeDownlinkResendsHeartbeatWhenOneIsDropped(t *testing.T) {
	var beats atomic.Int32
	r := startScriptRelay(t, func(m *proto.Message) *proto.Message {
		if m.Type == proto.MsgHeartbeat && beats.Add(1) > 1 { // 第一个被限流丢掉
			return heartbeatMsg()
		}
		return nil
	})
	c := NewClient(&Config{ServerAddr: r.addr})
	t.Cleanup(c.Close)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := c.probeDownlink(900 * time.Millisecond); err != nil {
		t.Fatalf("第一个心跳被丢、补发的有回应，下行是通的，不应失败: %v（relay 收到 %d 个心跳）", err, beats.Load())
	}
}

// 收到回应就不再补发，probeDownlink 返回之后也不会再有心跳从它那里发出：失败时调用方会马上关掉连接重连，迟到的
// 心跳落到新连接上就成了注册前的消息，relay 会把新连接关掉。
func TestProbeDownlinkStopsResendingOnceAnswered(t *testing.T) {
	var beats atomic.Int32
	r := startScriptRelay(t, func(m *proto.Message) *proto.Message {
		if m.Type == proto.MsgHeartbeat {
			beats.Add(1)
			return heartbeatMsg()
		}
		return nil
	})
	c := NewClient(&Config{ServerAddr: r.addr})
	t.Cleanup(c.Close)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := c.probeDownlink(300 * time.Millisecond); err != nil {
		t.Fatalf("relay 回了心跳，不应失败: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // 超过整个等待时长：还有补发的话早该到了
	if n := beats.Load(); n != 1 {
		t.Fatalf("第一个心跳就有回应，不应再补发，relay 收到 %d 个", n)
	}
}

// 记着这个地址的 TCP 收不到数据、先拨的 QUIC，QUIC 却连不上（UDP 也不通），最后连上的还是 TCP：要说清楚 QUIC
// 为什么没用上，不然重试提示里的「优先用 QUIC」看着像没起作用。
func TestConnectLogsWhenQUICFailsAfterTCPStall(t *testing.T) {
	resetTCPStalls(t)
	logs := captureLog(t)
	const addr = "relay.example.test:8443"
	orig := dialOneFunc
	t.Cleanup(func() { dialOneFunc = orig })
	dialOneFunc = func(_ context.Context, _, kind string, _ *tls.Config) (net.Conn, error) {
		if kind == TransportQUIC {
			return nil, errors.New("udp blocked")
		}
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		return a, nil
	}
	markTCPStalled(addr)
	c := NewClient(tlsConfigFor(t, addr, ""))
	t.Cleanup(c.Close)
	c.transport = TransportTCP
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if c.transport != TransportTCP {
		t.Fatalf("前置：QUIC 连不上时应连上 TCP，实际 %q", c.transport)
	}
	if got := logs.String(); !strings.Contains(got, "QUIC（UDP）连不上（udp blocked），仍用 TCP 连接") {
		t.Fatalf("应说明 QUIC 连不上、仍用 TCP，日志:\n%s", got)
	}
}

// 慢链路上的一条消息要读很久，但一直在收数据：常驻读循环不能把它当成 relay 静默。每段间隔（100ms）远小于静默时限
// （300ms），整条却要读约 0.9 秒。
const (
	slowChunks = 10
	slowGap    = 100 * time.Millisecond
	slowIdle   = 300 * time.Millisecond
)

func TestWaitSessionReadyToleratesSlowMessage(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, slowIdle)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	s, _, _ := plainShare(t, r.addr)
	if err := s.client.Connect(); err != nil {
		t.Fatal(err)
	}
	go r.pushSlowly(newMsg(proto.MsgSessionReady, &proto.SessionReady{SessionID: "slow"}), slowChunks, slowGap)
	var ready *proto.SessionReady
	err := mustFinishWithin(t, 5*time.Second, "ShareMode.waitSessionReady", func() error {
		var err error
		ready, err = s.waitSessionReady(nil)
		return err
	})
	if err != nil || ready == nil || ready.SessionID != "slow" {
		t.Fatalf("一直在收数据时不应判静默，应等到会话就绪：ready=%+v err=%v", ready, err)
	}
}

func TestShareHandleTunnelToleratesSlowMessage(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, slowIdle)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	s, _, _ := plainShare(t, r.addr)
	if err := s.client.Connect(); err != nil {
		t.Fatal(err)
	}
	go func() {
		r.pushSlowly(heartbeatMsg(), slowChunks, slowGap)
		r.hangUp()
	}()
	if err := mustFinishWithin(t, 5*time.Second, "ShareMode.handleTunnel", s.handleTunnel); err != nil {
		t.Fatalf("慢消息读完后 relay 正常关闭，handleTunnel 应返回 nil，实际: %v", err)
	}
}

func TestHelpHandleTunnelToleratesSlowMessage(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, slowIdle)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	h := NewHelpMode(&Config{ServerAddr: r.addr}, "CODE1234", "127.0.0.1:0")
	t.Cleanup(h.client.Close)
	if err := h.client.Connect(); err != nil {
		t.Fatal(err)
	}
	go func() {
		r.pushSlowly(heartbeatMsg(), slowChunks, slowGap)
		r.hangUp()
	}()
	err := mustFinishWithin(t, 5*time.Second, "HelpMode.handleTunnel", h.handleTunnel)
	var silent *relaySilentError
	if err == nil || errors.As(err, &silent) {
		t.Fatalf("慢消息读完后 relay 关闭，应返回连接关闭的错误而不是静默，实际: %v", err)
	}
}

// mcpHelp 返回连着假 relay 的 HelpMode 和它的 bridge（MCP 两个读循环的用例用）。
func mcpHelp(t *testing.T, r *scriptRelay) (*HelpMode, *mcp.Bridge) {
	t.Helper()
	h := NewHelpMode(&Config{ServerAddr: r.addr}, "CODE1234", "127.0.0.1:0")
	t.Cleanup(h.client.Close)
	if err := h.client.Connect(); err != nil {
		t.Fatal(err)
	}
	return h, mcp.NewBridge(h.client, proto.Session{})
}

// RunMCPMode 的读循环：relay 太久一个字节都没发来，要断开 bridge（唤醒在途的工具调用），原因是 relaySilentError。
func TestMCPReadLoopGivesUpWhenRelayGoesSilent(t *testing.T) {
	shrinkDuration(t, &mcpRelaySilenceTimeout, 300*time.Millisecond)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	h, bridge := mcpHelp(t, r)
	err := mustFinishWithin(t, 5*time.Second, "HelpMode.mcpReadLoop", func() error { return h.mcpReadLoop(bridge) })
	var silent *relaySilentError
	if !errors.As(err, &silent) || !strings.HasPrefix(err.Error(), "tunnel_lost:") {
		t.Fatalf("relay 静默时应以 tunnel_lost 断开，原因是 *relaySilentError，实际: %v", err)
	}
	if _, cerr := bridge.CallTool(context.Background(), "exec", nil); cerr == nil {
		t.Fatal("bridge 应已断开")
	}
}

func TestMCPReadLoopToleratesSlowMessage(t *testing.T) {
	shrinkDuration(t, &mcpRelaySilenceTimeout, slowIdle)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	h, bridge := mcpHelp(t, r)
	go func() {
		r.pushSlowly(heartbeatMsg(), slowChunks, slowGap)
		r.hangUp()
	}()
	err := mustFinishWithin(t, 5*time.Second, "HelpMode.mcpReadLoop", func() error { return h.mcpReadLoop(bridge) })
	var silent *relaySilentError
	if err == nil || errors.As(err, &silent) {
		t.Fatalf("慢消息读完后 relay 关闭，应因连接关闭断开而不是静默，实际: %v", err)
	}
}

// runRelayLoop 跑一个 helpLink.relayLoop 直到它退出，返回会话拆掉时记下的原因（HelpMCPBootstrap.lastDrop）。
func runRelayLoop(t *testing.T, h *HelpMode, bridge *mcp.Bridge) string {
	t.Helper()
	b := &HelpMCPBootstrap{}
	b.bridge = bridge
	l := newHelpLink(b, h, bridge, proto.Session{}, nil)
	_ = mustFinishWithin(t, 5*time.Second, "helpLink.relayLoop", func() error { l.relayLoop(); return nil })
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastDrop
}

// connect 工具（help_bootstrap）的 relay 读循环同样要发现静默：否则下行被干扰时会话一直挂着，工具调用干等兜底超时。
func TestRelayLoopGivesUpWhenRelayGoesSilent(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, 300*time.Millisecond)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	h, bridge := mcpHelp(t, r)
	if drop := runRelayLoop(t, h, bridge); !strings.HasPrefix(drop, "tunnel_lost:") || !strings.Contains(drop, RelaySilentMarker) {
		t.Fatalf("relay 静默时应以 tunnel_lost 拆会话、说明 relay 没有发来数据，实际: %q", drop)
	}
}

func TestRelayLoopToleratesSlowMessage(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, slowIdle)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	h, bridge := mcpHelp(t, r)
	go func() {
		r.pushSlowly(heartbeatMsg(), slowChunks, slowGap)
		r.hangUp()
	}()
	if drop := runRelayLoop(t, h, bridge); drop == "" || strings.Contains(drop, RelaySilentMarker) {
		t.Fatalf("慢消息读完后 relay 关闭，应因连接关闭拆会话而不是静默，实际: %q", drop)
	}
}
