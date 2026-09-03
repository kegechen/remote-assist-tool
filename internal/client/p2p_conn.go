package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
)

// P2PConn adapts a p2p.UDPTunnel to the JSON message protocol used by
// mcp.Bridge (MsgConn interface) and the help-side read loop.
//
// UDPTunnel provides a reliable, ordered byte stream (sequence numbers +
// ACK + retransmit + in-order delivery). We layer json.Encoder/Decoder
// on top — the same framing that client.Client uses over TCP.
type P2PConn struct {
	tunnel *p2p.UDPTunnel
	enc    *json.Encoder
	dec    *json.Decoder
	mu     sync.Mutex // protects enc (writes must be serialized)
}

// NewP2PConn wraps a UDPTunnel in a JSON message connection.
func NewP2PConn(tunnel *p2p.UDPTunnel) *P2PConn {
	return &P2PConn{
		tunnel: tunnel,
		enc:    json.NewEncoder(tunnel),
		dec:    json.NewDecoder(tunnel),
	}
}

// SendMessage implements mcp.MsgConn — encodes a proto.Message as JSON
// and writes it to the tunnel.
func (c *P2PConn) SendMessage(msgType proto.MessageType, payload interface{}) error {
	msg, err := proto.NewMessage(msgType, payload)
	if err != nil {
		return fmt.Errorf("p2p SendMessage: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(msg)
}

// ReadMessage decodes the next JSON message from the tunnel.
func (c *P2PConn) ReadMessage() (*proto.Message, error) {
	var msg proto.Message
	if err := c.dec.Decode(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// Close closes the underlying tunnel.
func (c *P2PConn) Close() error {
	return c.tunnel.Close()
}

// P2P tunnel mode headers: the first 2 bytes on the tunnel identify the mode.
// SSH mode (existing help.go path) sends raw SSH bytes with no header.
// Tool mode (MCP bootstrap) sends this magic header before JSON messages,
// allowing the share side to branch between SSH and tool handling.
var p2pToolModeHeader = [2]byte{0x00, 'T'}

// WriteModeHeader sends the tool-mode header as the first bytes on the tunnel.
// Must be called exactly once, before any JSON messages.
func (c *P2PConn) WriteModeHeader() error {
	_, err := c.tunnel.Write(p2pToolModeHeader[:])
	return err
}

// ReadModeHeader reads the first 2 bytes from the tunnel and returns true
// if they match the tool-mode header. If not, the consumed bytes are returned
// so the caller can prepend them to the SSH byte stream.
func ReadModeHeader(tunnel *p2p.UDPTunnel) (toolMode bool, consumed [2]byte, err error) {
	n, err := tunnel.Read(consumed[:])
	if err != nil {
		return false, consumed, err
	}
	if n < 2 {
		// Partial read — read the second byte
		n2, err := tunnel.Read(consumed[n:])
		if err != nil {
			return false, consumed, err
		}
		n += n2
	}
	return consumed == p2pToolModeHeader, consumed, nil
}

// errP2PReadTimeout 标记 P2P 隧道上的读超时，供调用方区分「对端没按预期说话」
// （应关隧道回退 relay）与真正的隧道错误。
var errP2PReadTimeout = errors.New("p2p read timeout")

// ReadModeHeaderTimeout 是 ReadModeHeader 的带超时版本。
//
// 为什么必须有超时：P2P 的成功判定是单向的（一端收到对方的打洞包就宣告成功），
// 两端完全可能得出相反结论。若 share 认为通了而 help 认为没通，share 会永远阻塞
// 在裸 ReadModeHeader 上——UDPTunnel.Read 只在 peerTimeout(60s) 后才醒——期间它
// 根本不读 relay，help 在 relay 上的 ToolHello 无人应答，整条会话就此卡死。
// 超时后调用方须 Close 隧道并回退 relay：底层阻塞的 goroutine 会因 stopCh 关闭
// 拿到 io.EOF 退出，不会泄漏。
func ReadModeHeaderTimeout(tunnel *p2p.UDPTunnel, d time.Duration) (toolMode bool, consumed [2]byte, err error) {
	type result struct {
		toolMode bool
		consumed [2]byte
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		tm, c, e := ReadModeHeader(tunnel)
		ch <- result{tm, c, e}
	}()
	select {
	case r := <-ch:
		return r.toolMode, r.consumed, r.err
	case <-time.After(d):
		return false, consumed, errP2PReadTimeout
	}
}

const (
	// p2pModeHeaderTimeout share 端等 help 端 mode header 的上限。必须显著小于 help
	// 端 relay 工具握手的 15s 超时，保证「share 先醒回 relay、help 后到」的顺序，
	// 两端才能在 relay 上重新相遇。
	p2pModeHeaderTimeout = 8 * time.Second
	// p2pProbeTimeout help 端等 pong 的上限：隧道已建成时 RTT 是毫秒级，等不到就是
	// 反方向不通（对称 NAT / 出向过滤），早退早回退。
	p2pProbeTimeout = 5 * time.Second
)

// ProbeBidirectional 在已建成的隧道上发 ping 并等 pong，**双向**证实可达后才允许
// 把工具流量切过来。
//
// 这是本方案的关键一环：UDP 打洞的成功判定天然是单向的（收到对方的包就宣告成功），
// 而一旦某端先建成隧道，它此后发的是隧道二进制包，对端仍在用 json.Unmarshal 解
// P2PTestPacket，解不出就丢——单向状态无法自愈。只有在隧道上真正跑一个来回，
// 才能确认两个方向都通。
//
// ping/pong 复用 MsgHeartbeat，不扩协议：旧版对端对 Heartbeat 是 ignore、不回 pong，
// 这里超时后干净回退 relay，天然向后兼容。
func (c *P2PConn) ProbeBidirectional(d time.Duration) error {
	if err := c.SendMessage(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()}); err != nil {
		return fmt.Errorf("p2p probe send: %w", err)
	}
	deadline := time.Now().Add(d)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return errP2PReadTimeout
		}
		msg, err := c.ReadMessageTimeout(remain)
		if err != nil {
			return err
		}
		if msg.Type == proto.MsgHeartbeat {
			return nil
		}
		// 其他消息（对端抢跑的工具响应等）在此阶段不该出现，忽略并继续等 pong。
	}
}

// ReadMessageTimeout 是 ReadMessage 的带超时版本，用于 P2P 升级握手这类
// 「对端可能压根收不到我」的场景。超时语义与 ReadModeHeaderTimeout 相同：
// 调用方须随即 Close 隧道，阻塞的 goroutine 才能退出。
func (c *P2PConn) ReadMessageTimeout(d time.Duration) (*proto.Message, error) {
	type result struct {
		msg *proto.Message
		err error
	}
	ch := make(chan result, 1)
	go func() {
		m, e := c.ReadMessage()
		ch <- result{m, e}
	}()
	select {
	case r := <-ch:
		return r.msg, r.err
	case <-time.After(d):
		return nil, errP2PReadTimeout
	}
}
