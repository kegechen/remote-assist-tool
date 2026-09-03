package relay

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// execStreamPayloadBytes 一帧 exec stream 的典型 payload 大小：agent 侧单帧最多读 32 KiB
// 原始字节，AEAD 封装后再经 JSON 的 base64 编码，落到线上约 44 KiB。
const execStreamPayloadBytes = 44 * 1024

// newToolStreamPair 建一对已配对的 share/help 连接，返回 share 侧（发流的一方）、
// help 侧的写缓冲（收流的一方）以及 server。
func newToolStreamPair(t *testing.T) (*Server, *ClientConn, *bytes.Buffer) {
	t.Helper()
	cfg := &Config{CodeTTL: ttl, CodeLength: 10}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	shareConn := &capturingConn{remoteAddr: "10.9.9.1:5000"}
	share := &ClientConn{ID: "share-stream", Conn: shareConn, Type: connStateShare}
	srv.sessions.CreateSession("CODE9999", share, ttl, "", "10.9.9.1", 100, 1000)

	helpConn := &capturingConn{remoteAddr: "10.9.9.2:5001"}
	help := &ClientConn{ID: "help-stream", Conn: helpConn}
	joinMsg, _ := proto.NewMessage(proto.MsgJoinRequest, &proto.JoinRequest{Code: "CODE9999", Version: "1.0"})
	if srv.handleMessage(help, joinMsg) {
		t.Fatal("join 不应关闭连接")
	}
	if help.Type != connStateHelp {
		t.Fatalf("join 后 help.Type=%q", help.Type)
	}
	return srv, share, &helpConn.buf
}

// countForwarded 数 buf 里 MsgToolStream 的条数。
func countForwarded(t *testing.T, buf *bytes.Buffer) int {
	t.Helper()
	n := 0
	for _, line := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var msg proto.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("转发出去的不是合法消息: %v", err)
		}
		if msg.Type == proto.MsgToolStream {
			n++
		}
	}
	return n
}

func streamFrame(t *testing.T, seq uint32) *proto.Message {
	t.Helper()
	msg, err := proto.NewMessage(proto.MsgToolStream, &proto.StreamChunk{
		ID: 1, Seq: seq, Stream: "stdout", Data: make([]byte, execStreamPayloadBytes*3/4),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	return msg
}

// TestToolStreamNotDroppedUnderNormalOutput 一次正常的 exec stream 输出必须一帧不少地
// 转发过去。
//
// 修复前工具通道复用的是 Tunnel data 的"条数/秒"限流（100/s，突发 200）：cat 一个大日志
// 轻松超限，超限后既不回错误也不断连，只 return false 把帧丢掉。两端都察觉不到——payload
// 是 AEAD 的、每帧独立 nonce，丢帧不影响后续解密，Seq 又从来没人读。用户拿到的是中间被
// 悄悄挖掉若干 KB、最终 ToolResp 仍然 OK 的输出。
func TestToolStreamNotDroppedUnderNormalOutput(t *testing.T) {
	srv, share, helpBuf := newToolStreamPair(t)

	const frames = 300 // 约 13 MiB，在 32 MiB 突发额度之内
	for i := 0; i < frames; i++ {
		if srv.handleMessage(share, streamFrame(t, uint32(i))) {
			t.Fatalf("第 %d 帧把连接关掉了", i)
		}
	}
	if got := countForwarded(t, helpBuf); got != frames {
		t.Fatalf("只转发了 %d/%d 帧，其余被静默丢弃", got, frames)
	}
}

// TestToolStreamThrottledButLossless 超过突发额度之后要靠背压压慢，而不是丢帧：
// relay 读一条转一条，不读就是不收，TCP 窗口自然把发送端压慢。慢是可以接受的，
// 少数据不行。
func TestToolStreamThrottledButLossless(t *testing.T) {
	srv, share, helpBuf := newToolStreamPair(t)

	// 900 帧约 38 MiB，超过 32 MiB 的突发额度，必然进入节流路径。
	const frames = 900
	for i := 0; i < frames; i++ {
		if srv.handleMessage(share, streamFrame(t, uint32(i))) {
			t.Fatalf("第 %d 帧把连接关掉了：节流不该升级成断连", i)
		}
	}
	if got := countForwarded(t, helpBuf); got != frames {
		t.Fatalf("节流路径下只转发了 %d/%d 帧", got, frames)
	}
}

// TestToolChannelBurstCoversMaxMessage 限额配置必须保证突发额度装得下最大的一条消息，
// 否则每帧都等不到额度，工具通道一上线就反复断连。
func TestToolChannelBurstCoversMaxMessage(t *testing.T) {
	l := DefaultLimits()
	l.ToolBurstKiBPerConnection = 1
	if err := ValidateLimits(l); err == nil {
		t.Fatal("突发额度小于单条消息上限，ValidateLimits 应当拒绝")
	}
	if err := ValidateLimits(DefaultLimits()); err != nil {
		t.Fatalf("默认限额自身不合法: %v", err)
	}
}
