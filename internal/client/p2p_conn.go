package client

import (
	"encoding/json"
	"fmt"
	"sync"

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
