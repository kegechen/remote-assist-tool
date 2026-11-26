package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/proto"
)

// 这一组用例针对「TCP 连得上、relay 发回的数据却收不到」：握手、请求都过得去，relay 发回的数据从某一刻起被网络
// 丢掉（现场：share 拿到了注册应答、显示了协助码，之后什么都收不到；help 连加入应答都等不到）。

// shrinkDuration 把一个 var 时长临时调成 d，测试结束时还原。
func shrinkDuration(t *testing.T, p *time.Duration, d time.Duration) {
	t.Helper()
	orig := *p
	*p = d
	t.Cleanup(func() { *p = orig })
}

// resetTCPStalls 清掉进程级的 tcpStalls，测试结束时再清一次：别的用例不能继承这里记下的地址。
func resetTCPStalls(t *testing.T) {
	t.Helper()
	reset := func() {
		tcpStalls.Lock()
		tcpStalls.addrs = map[string]time.Time{}
		tcpStalls.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func newMsg(typ proto.MessageType, payload any) *proto.Message {
	msg, err := proto.NewMessage(typ, payload)
	if err != nil {
		panic(err)
	}
	return msg
}

// scriptRelay 是明文的假 relay，只接一条连接：收到的每条消息交给 onMsg，它返回的消息发回去（nil 不回）；
// push 主动往这条连接上发。
type scriptRelay struct {
	addr      string
	connected chan struct{}

	mu  sync.Mutex
	enc *json.Encoder
}

func startScriptRelay(t *testing.T, onMsg func(*proto.Message) *proto.Message) *scriptRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &scriptRelay{addr: ln.Addr().String(), connected: make(chan struct{})}
	var conn atomic.Pointer[net.Conn]
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Store(&c)
		r.mu.Lock()
		r.enc = json.NewEncoder(c)
		r.mu.Unlock()
		close(r.connected)
		dec := json.NewDecoder(c)
		for {
			var msg proto.Message
			if dec.Decode(&msg) != nil {
				return
			}
			if reply := onMsg(&msg); reply != nil {
				r.mu.Lock()
				_ = r.enc.Encode(reply)
				r.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		if c := conn.Load(); c != nil {
			(*c).Close()
		}
	})
	return r
}

func (r *scriptRelay) push(msg *proto.Message) {
	<-r.connected
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.enc.Encode(msg)
}

// answerRegister 回注册请求，heartbeat 为 true 时也回心跳（真 relay 的行为），别的不理。
func answerRegister(code string, heartbeat bool) func(*proto.Message) *proto.Message {
	return func(m *proto.Message) *proto.Message {
		switch {
		case m.Type == proto.MsgRegisterRequest:
			return newMsg(proto.MsgRegisterResponse, &proto.RegisterResponse{Code: code, CodeSticky: true})
		case m.Type == proto.MsgHeartbeat && heartbeat:
			return newMsg(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()})
		}
		return nil
	}
}

// plainShare 返回连明文 relay addr 的 share、它的控制台输出和协助码文件路径。
func plainShare(t *testing.T, addr string) (*ShareMode, *syncBuf, string) {
	t.Helper()
	stubClipboard(t)
	codeFile := filepath.Join(t.TempDir(), "code.json")
	s := NewShareMode(&Config{ServerAddr: addr}, "127.0.0.1:22", true, agent.SandboxConfig{}, codeFile, "")
	out := &syncBuf{}
	s.console = newTransferProgress(out, false, nil)
	t.Cleanup(s.client.Close)
	return s, out, codeFile
}

// 注册应答到了，心跳却没有回应：这条连接已经收不到 relay 发来的数据，协助码交出去也用不了。register 必须失败，
// 而且什么都不显示、不写协助码文件——否则用户看到的是 share 成功了，协助端却怎么都连不上。
func TestShareRegisterFailsWhenHeartbeatGetsNoReply(t *testing.T) {
	shrinkDuration(t, &downlinkProbeTimeout, 300*time.Millisecond)
	r := startScriptRelay(t, answerRegister("SILENT1234", false))
	s, out, codeFile := plainShare(t, r.addr)
	if err := s.client.Connect(); err != nil {
		t.Fatal(err)
	}

	err := mustFinishWithin(t, 5*time.Second, "ShareMode.register", s.register)
	var silent *relaySilentError
	if !errors.As(err, &silent) {
		t.Fatalf("心跳没有回应时注册应失败并返回 *relaySilentError，实际: %v", err)
	}
	if silent.what != "心跳回应" {
		t.Fatalf("等的应是心跳回应，实际: %q", silent.what)
	}
	if !isNetTimeout(err) {
		t.Fatalf("包装后仍应认得出是超时: %v", err)
	}
	if s.code != "" {
		t.Fatalf("失败的注册不应记下协助码，s.code=%q", s.code)
	}
	if got := out.String(); strings.Contains(got, "SILENT1234") || strings.Contains(got, "等待协助端连接") {
		t.Fatalf("失败的注册不应显示协助码，输出:\n%s", got)
	}
	if _, err := os.Stat(codeFile); !os.IsNotExist(err) {
		t.Fatalf("失败的注册不应写协助码文件: %v", err)
	}
}

// 对照：relay 回心跳时照常注册、显示协助码。
func TestShareRegisterSucceedsWhenHeartbeatIsAnswered(t *testing.T) {
	shrinkDuration(t, &downlinkProbeTimeout, 2*time.Second)
	r := startScriptRelay(t, answerRegister("ECHOED1234", true))
	s, out, codeFile := plainShare(t, r.addr)
	if err := s.client.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := mustFinishWithin(t, 5*time.Second, "ShareMode.register", s.register); err != nil {
		t.Fatalf("relay 回了心跳，注册应成功: %v", err)
	}
	if s.code != "ECHOED1234" || !strings.Contains(out.String(), "等待协助端连接") {
		t.Fatalf("应显示协助码，s.code=%q 输出:\n%s", s.code, out.String())
	}
	if _, err := os.Stat(codeFile); err != nil {
		t.Fatalf("应写协助码文件: %v", err)
	}
}

// 确认下行时先到的不是心跳回应（比如 relay 保留了配对、会话就绪先到），这条消息不能丢：之后的 ReadMessage 先拿到它。
func TestProbeDownlinkKeepsMessageThatArrivesFirst(t *testing.T) {
	r := startScriptRelay(t, func(m *proto.Message) *proto.Message {
		if m.Type == proto.MsgHeartbeat {
			return newMsg(proto.MsgSessionReady, &proto.SessionReady{SessionID: "first"})
		}
		return nil
	})
	c := NewClient(&Config{ServerAddr: r.addr})
	t.Cleanup(c.Close)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := c.probeDownlink(2 * time.Second); err != nil {
		t.Fatalf("收到了消息就说明下行是通的: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("确认下行时读到的会话就绪丢了: %v", err)
	}
	var ready proto.SessionReady
	if msg.Type != proto.MsgSessionReady || proto.DecodePayload(msg, &ready) != nil || ready.SessionID != "first" {
		t.Fatalf("应先拿到确认下行时读到的会话就绪，实际: %s %s", msg.Type, msg.Payload)
	}
}

// 等协助端时 relay 太久一条消息都没发来（心跳也没有回应），就是连接已经不通了：返回去重连，不能一直停在
// 「等待协助端连接...」。
func TestWaitSessionReadyGivesUpWhenRelayGoesSilent(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, 300*time.Millisecond)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	s, _, _ := plainShare(t, r.addr)
	if err := s.client.Connect(); err != nil {
		t.Fatal(err)
	}
	err := mustFinishWithin(t, 5*time.Second, "ShareMode.waitSessionReady", func() error {
		_, err := s.waitSessionReady(nil)
		return err
	})
	var silent *relaySilentError
	if !errors.As(err, &silent) || silent.what != relaySilenceWhat {
		t.Fatalf("relay 静默时应返回 *relaySilentError，实际: %v", err)
	}
}

// 对照：心跳回应一直在来，等多久都照常等（读 deadline 每读一条之前重新设，不能只设一次）。
func TestWaitSessionReadyKeepsWaitingWhileHeartbeatsArrive(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, 500*time.Millisecond)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	s, _, _ := plainShare(t, r.addr)
	if err := s.client.Connect(); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := 0; i < 10; i++ {
			time.Sleep(100 * time.Millisecond)
			r.push(newMsg(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()}))
		}
		r.push(newMsg(proto.MsgSessionReady, &proto.SessionReady{SessionID: "late"}))
	}()
	var ready *proto.SessionReady
	err := mustFinishWithin(t, 5*time.Second, "ShareMode.waitSessionReady", func() error {
		var err error
		ready, err = s.waitSessionReady(nil)
		return err
	})
	if err != nil || ready == nil || ready.SessionID != "late" {
		t.Fatalf("心跳一直在来时应等到会话就绪，实际: ready=%+v err=%v", ready, err)
	}
}

// 会话建立后的常驻读循环（handleTunnel）同样靠每读一条之前重设的读 deadline 发现 relay 静默：
// 否则下行被干扰时隧道已经死了，用户却停在「已连接」上永远等不到数据。
func TestShareHandleTunnelGivesUpWhenRelayGoesSilent(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, 300*time.Millisecond)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	s, _, _ := plainShare(t, r.addr)
	if err := s.client.Connect(); err != nil {
		t.Fatal(err)
	}
	err := mustFinishWithin(t, 5*time.Second, "ShareMode.handleTunnel", s.handleTunnel)
	var silent *relaySilentError
	if !errors.As(err, &silent) || silent.what != relaySilenceWhat {
		t.Fatalf("relay 静默时 handleTunnel 应返回 *relaySilentError，实际: %v", err)
	}
}

// 同 share.handleTunnel：help 的常驻读循环太久收不到 relay 的任何消息也要返回 silentErr 结束隧道。
func TestHelpHandleTunnelGivesUpWhenRelayGoesSilent(t *testing.T) {
	shrinkDuration(t, &relaySilenceTimeout, 300*time.Millisecond)
	r := startScriptRelay(t, func(*proto.Message) *proto.Message { return nil })
	h := NewHelpMode(&Config{ServerAddr: r.addr}, "CODE1234", "127.0.0.1:0")
	t.Cleanup(h.client.Close)
	if err := h.client.Connect(); err != nil {
		t.Fatal(err)
	}
	err := mustFinishWithin(t, 5*time.Second, "HelpMode.handleTunnel", h.handleTunnel)
	var silent *relaySilentError
	if !errors.As(err, &silent) || silent.what != relaySilenceWhat {
		t.Fatalf("relay 静默时 handleTunnel 应返回 *relaySilentError，实际: %v", err)
	}
}

func timeoutErr() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
}

// silentErr 只在 auto 模式下的 TCP 上记下「这个地址的 TCP 收不到数据」，之后拨它先走 QUIC；用户限定了 TCP 时
// 提示可以改用 QUIC；别的错误原样返回。
func TestSilentErrMarksStalledTCPOnlyInAutoMode(t *testing.T) {
	const addr = "Relay.Example.Test:8443"
	cases := []struct {
		name      string
		cfg       Config
		transport string
		node      *nodePin
		fallback  bool
		hint      bool // 提示 --transport quic
		tcpNote   bool // 说明 TCP 连接还在、收不到数据
	}{
		{name: "auto 下的 TCP", cfg: Config{UseTLS: true}, transport: TransportTCP, fallback: true, tcpNote: true},
		{name: "显式 auto", cfg: Config{UseTLS: true, Transport: TransportAuto}, transport: TransportTCP, fallback: true, tcpNote: true},
		{name: "--transport tcp", cfg: Config{UseTLS: true, Transport: TransportTCP}, transport: TransportTCP, hint: true, tcpNote: true},
		{name: "已经在 QUIC 上", cfg: Config{UseTLS: true}, transport: TransportQUIC},
		{name: "明文 relay 走不了 QUIC", cfg: Config{}, transport: TransportTCP, tcpNote: true},
		{name: "节点只声明了 TCP", cfg: Config{UseTLS: true}, transport: TransportTCP, node: &nodePin{mode: TransportTCP}, tcpNote: true},
		{name: "节点两种都有", cfg: Config{UseTLS: true}, transport: TransportTCP, node: &nodePin{mode: TransportAuto}, fallback: true, tcpNote: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetTCPStalls(t)
			cfg := tc.cfg
			cfg.ServerAddr = addr
			c := NewClient(&cfg)
			c.transport, c.node = tc.transport, tc.node
			err := c.silentErr(timeoutErr(), "加入应答", 15*time.Second)
			var silent *relaySilentError
			if !errors.As(err, &silent) {
				t.Fatalf("读超时应换成 *relaySilentError，实际: %v", err)
			}
			if !isNetTimeout(err) || !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("应保留底层的超时错误: %v", err)
			}
			if silent.fallback != tc.fallback || tcpStalled("relay.example.test:8443") != tc.fallback {
				t.Fatalf("fallback=%v stalled=%v，期望都是 %v", silent.fallback, tcpStalled(addr), tc.fallback)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "中转服务 "+addr+" 在 15 秒内没有发来加入应答") {
				t.Fatalf("提示: %s", msg)
			}
			if strings.Contains(msg, "--transport quic") != tc.hint || strings.Contains(msg, "收不到它发回的数据") != tc.tcpNote {
				t.Fatalf("提示: %s", msg)
			}
			if (quicRetryNote(err) != "") != tc.fallback {
				t.Fatalf("quicRetryNote=%q", quicRetryNote(err))
			}
		})
	}

	c := NewClient(&Config{ServerAddr: addr, UseTLS: true})
	c.transport = TransportTCP
	if err := c.silentErr(io.EOF, "加入应答", time.Second); err != io.EOF {
		t.Fatalf("不是超时的错误应原样返回，实际: %v", err)
	}
}

// 同一个地址只在第一次记下时打一行，之后的超时不重复说。
func TestSilentErrLogsStallOnce(t *testing.T) {
	resetTCPStalls(t)
	logs := captureLog(t)
	c := NewClient(&Config{ServerAddr: "relay.example.test:8443", UseTLS: true})
	c.transport = TransportTCP
	for i := 0; i < 3; i++ {
		_ = c.silentErr(timeoutErr(), relaySilenceWhat, relaySilenceTimeout)
	}
	if n := strings.Count(logs.String(), "之后连它先走 QUIC"); n != 1 {
		t.Fatalf("应只打一次，实际 %d 次:\n%s", n, logs.String())
	}
}

// 记下 TCP 收不到数据之后，再拨这个地址先拨 QUIC，即使上一次用的是 TCP：竞速只比握手谁先完成，这种 TCP 永远赢。
func TestDialTriesQUICFirstAfterTCPStall(t *testing.T) {
	resetTCPStalls(t)
	var mu sync.Mutex
	var kinds []string
	orig := dialOneFunc
	t.Cleanup(func() { dialOneFunc = orig })
	dialOneFunc = func(_ context.Context, _, kind string, _ *tls.Config) (net.Conn, error) {
		mu.Lock()
		kinds = append(kinds, kind)
		mu.Unlock()
		return nil, errors.New("refused") // 立刻失败，另一路马上起跑：拨的先后就是记录的先后
	}
	order := func(preferred string) []string {
		mu.Lock()
		kinds = nil
		mu.Unlock()
		_, _, _, _ = dialRelayAddr(context.Background(), "relay.example.test:8443", TransportAuto, &tls.Config{}, preferred)
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), kinds...)
	}
	if got := order(TransportTCP); len(got) != 2 || got[0] != TransportTCP {
		t.Fatalf("前置：没记下时先拨 TCP，实际 %v", got)
	}
	markTCPStalled("RELAY.example.test:8443") // 地址不分大小写
	if got := order(TransportTCP); len(got) != 2 || got[0] != TransportQUIC {
		t.Fatalf("记下 TCP 收不到数据之后应先拨 QUIC，实际 %v", got)
	}
}

// 「TCP 收不到数据」的记录有时效：一过性的原因（切换网络、休眠唤醒）也会记下，过期后照旧先拨 TCP，可以重新记下。
func TestTCPStallsExpire(t *testing.T) {
	resetTCPStalls(t)
	shrinkDuration(t, &tcpStallTTL, 40*time.Millisecond)
	if !markTCPStalled("relay.example.test:8443") {
		t.Fatal("前置：第一次记下应返回 true")
	}
	if !tcpStalled("relay.example.test:8443") {
		t.Fatal("刚记下时应有效")
	}
	time.Sleep(120 * time.Millisecond)
	if tcpStalled("relay.example.test:8443") {
		t.Fatal("过期后不应再算数")
	}
	if !markTCPStalled("relay.example.test:8443") {
		t.Fatal("过期后应能重新记下")
	}
}

// 因为 TCP 收不到数据才先拨的 QUIC，不能再打「TCP 不可达」：TCP 是连得上的，这么说会把排查引错方向。
func TestLogTransportQuietWhenQUICChosenForStalledTCP(t *testing.T) {
	resetTCPStalls(t)
	logs := captureLog(t)
	const addr = "relay.example.test:8443"
	markTCPStalled(addr)
	logTransport(addr, TransportQUIC, TransportTCP, nil)
	if strings.Contains(logs.String(), "不可达") {
		t.Fatalf("不应说 TCP 不可达:\n%s", logs.String())
	}
}

// 重试提示要说清楚这次改走 QUIC。
func TestReconnectFailureSaysRetryUsesQUIC(t *testing.T) {
	silent := &relaySilentError{addr: "relay.example.test:8443", transport: TransportTCP, what: "心跳回应",
		wait: 10 * time.Second, fallback: true, err: timeoutErr()}
	line, _, _ := reconnectFailure(1, silent, time.Second)
	want := "连接失败(第 1 次): 中转服务 relay.example.test:8443 在 10 秒内没有发来心跳回应（TCP 连接还在，但收不到它发回的数据，可能被网络干扰）；1s 后改走 QUIC（UDP）重试...\n"
	if line != want {
		t.Fatalf("提示:\n%q\n期望:\n%q", line, want)
	}
	if line, _, _ := reconnectFailure(1, errors.New("refused"), time.Second); line != "连接失败(第 1 次): refused；1s 后重试...\n" {
		t.Fatalf("普通失败的提示不应提 QUIC: %q", line)
	}
}

// stallingProxy 在 TCP 上冒充 relay（同一张证书，指纹与 QUIC 那边一致），把明文转给真正的 relay：客户端发来的
// 照转；relay 发回的每条连接只转前 pass 条消息，之后的全部丢掉、连接照样开着。效果同「握手、请求都过得去，
// relay 发回的数据从某一刻起被网络丢掉」。客户端关连接时关掉到 relay 的那条，relay 才腾出位置。
type stallingProxy struct {
	conns atomic.Int32 // 接到过的 TCP 连接数
}

func startStallingProxy(t *testing.T, addr, relayAddr, cert, key string, pass int) *stallingProxy {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	p := &stallingProxy{}
	var mu sync.Mutex
	var open []net.Conn
	track := func(c net.Conn) {
		mu.Lock()
		open = append(open, c)
		mu.Unlock()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.conns.Add(1)
			track(c)
			go func() {
				up, err := tls.Dial("tcp", relayAddr, &tls.Config{InsecureSkipVerify: true})
				if err != nil {
					c.Close()
					return
				}
				track(up)
				go func() {
					_, _ = io.Copy(up, c)
					up.Close()
				}()
				r := bufio.NewReader(up)
				for n := 0; ; n++ {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					if n < pass {
						_, _ = c.Write(line)
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range open {
			c.Close()
		}
		mu.Unlock()
	})
	return p
}

// stalledTCPRelay 起一个真 relay：QUIC 在 addr 上直通，TCP 在 addr 上经 stallingProxy（relay 发回的每条连接只过前
// pass 条消息）。
func stalledTCPRelay(t *testing.T, pass int) (string, *stallingProxy) {
	t.Helper()
	addr, relayTCP := freePort(t), freePort(t)
	cert, key := newCertPair(t)
	startRelay(t, relayTCP, addr, cert, key)
	return addr, startStallingProxy(t, addr, relayTCP, cert, key, pass)
}

func tlsConfigFor(t *testing.T, addr, transport string) *Config {
	return &Config{
		ServerAddr: addr, UseTLS: true, InsecureSkip: true, Transport: transport,
		trustStore: &crypto.TrustStore{Path: filepath.Join(t.TempDir(), crypto.KnownHostsFileName)},
	}
}

func tlsShare(t *testing.T, addr, transport string) (*ShareMode, *syncBuf) {
	t.Helper()
	stubClipboard(t)
	s := NewShareMode(tlsConfigFor(t, addr, transport), "127.0.0.1:22", true, agent.SandboxConfig{}, filepath.Join(t.TempDir(), "code.json"), "")
	out := &syncBuf{}
	s.console = newTransferProgress(out, false, nil)
	t.Cleanup(s.client.Close)
	return s, out
}

func tlsHelp(t *testing.T, addr, code, transport string) *HelpMode {
	t.Helper()
	h := NewHelpMode(tlsConfigFor(t, addr, transport), code, "127.0.0.1:0")
	t.Cleanup(h.client.Close)
	return h
}

// registeredShareOverQUIC 让一个只走 QUIC 的 share 在 addr 上注册好，返回协助码。
func registeredShareOverQUIC(t *testing.T, addr string) string {
	t.Helper()
	s, _ := tlsShare(t, addr, TransportQUIC)
	_ = mustFinishWithin(t, 15*time.Second, "share 注册", func() error { s.reconnectWithBackoff(); return nil })
	if s.code == "" {
		t.Fatal("前置：share 没有注册上")
	}
	return s.code
}

// 现场：share 在 TCP 上拿到了注册应答，之后 relay 发来的什么都收不到。协助码不能在这条 TCP 上显示出来，
// 要改走 QUIC 注册好再显示，而且显示出来的协助码协助端真的连得上。
func TestShareMovesToQUICBeforeShowingCodeWhenTCPDownlinkStalls(t *testing.T) {
	resetTCPStalls(t)
	shrinkDuration(t, &downlinkProbeTimeout, 500*time.Millisecond)
	setQuicRaceDelay(t, 2*time.Second)
	addr, _ := stalledTCPRelay(t, 1) // 每条 TCP 连接只过得来第一条消息（注册应答）

	s, out := tlsShare(t, addr, "")
	_ = mustFinishWithin(t, 20*time.Second, "share 注册", func() error { s.reconnectWithBackoff(); return nil })
	if s.client.transport != TransportQUIC {
		t.Fatalf("TCP 收不到数据时应改走 QUIC 注册，实际 transport=%q，输出:\n%s", s.client.transport, out.String())
	}
	got := out.String()
	if n := strings.Count(got, "请复制给你的 AI 助手"); n != 1 {
		t.Fatalf("协助码应只显示一次（QUIC 上注册好之后），实际 %d 次，输出:\n%s", n, got)
	}
	if !strings.Contains(got, "改走 QUIC（UDP）重试") {
		t.Fatalf("重试提示应说改走 QUIC，输出:\n%s", got)
	}

	// 显示出来的协助码真的用得了：协助端加入，share 收到会话就绪。
	ready := make(chan error, 1)
	go func() {
		_, err := s.waitSessionReady(nil)
		ready <- err
	}()
	h := tlsHelp(t, addr, s.code, "")
	if err := mustFinishWithin(t, 20*time.Second, "help 加入", func() error { _, err := h.connectAndJoin(); return err }); err != nil {
		t.Fatalf("协助端应能加入: %v", err)
	}
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("share 应收到会话就绪: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("share 没有收到会话就绪")
	}
}

// 现场：help 在 TCP 上发出了加入请求，加入应答（或之后的数据）收不到。auto 模式下要改走 QUIC 再加入一次，
// TCP 只试这一次。
func TestHelpJoinMovesToQUICWhenTCPDownlinkStalls(t *testing.T) {
	cases := []struct {
		name string
		pass int
	}{
		{"加入应答也收不到", 0},
		{"收到加入应答之后收不到", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetTCPStalls(t)
			shrinkJoinTimeout(t, time.Second)
			shrinkDuration(t, &downlinkProbeTimeout, 500*time.Millisecond)
			setQuicRaceDelay(t, 2*time.Second)
			addr, proxy := stalledTCPRelay(t, tc.pass)
			code := registeredShareOverQUIC(t, addr)

			h := tlsHelp(t, addr, code, "")
			var resp *proto.JoinResponse
			err := mustFinishWithin(t, 20*time.Second, "help 加入", func() error {
				var err error
				resp, err = h.connectAndJoin()
				return err
			})
			if err != nil {
				t.Fatalf("TCP 收不到数据时应改走 QUIC 加入成功: %v", err)
			}
			if h.client.transport != TransportQUIC || resp.SessionID == "" {
				t.Fatalf("应在 QUIC 上加入，transport=%q resp=%+v", h.client.transport, resp)
			}
			if n := proxy.conns.Load(); n != 1 {
				t.Fatalf("TCP 应只试一次，实际 %d 次", n)
			}
		})
	}
}

// 用户限定了 --transport tcp：不改走 QUIC，也不记下这个地址，提示里告诉用户可以改用 --transport quic。
func TestHelpJoinWithExplicitTCPSuggestsQUIC(t *testing.T) {
	resetTCPStalls(t)
	shrinkJoinTimeout(t, time.Second)
	addr, proxy := stalledTCPRelay(t, 0)
	code := registeredShareOverQUIC(t, addr)

	h := tlsHelp(t, addr, code, TransportTCP)
	err := mustFinishWithin(t, 20*time.Second, "help 加入", func() error { _, err := h.connectAndJoin(); return err })
	var silent *relaySilentError
	if !errors.As(err, &silent) || !strings.Contains(err.Error(), "--transport quic") {
		t.Fatalf("应提示可以改用 --transport quic，实际: %v", err)
	}
	if n := proxy.conns.Load(); n != 1 || tcpStalled(addr) {
		t.Fatalf("限定了 TCP 时不应重试或记下地址：连接 %d 次，stalled=%v", n, tcpStalled(addr))
	}
}
