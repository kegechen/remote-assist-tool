package relay

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/ratelimit"
)

// capturingConn 可设置 remoteAddr 且捕获写出数据的 mock Conn。
type capturingConn struct {
	buf        bytes.Buffer
	remoteAddr string
}

func (c *capturingConn) Read(p []byte) (int, error)         { return 0, nil }
func (c *capturingConn) Write(p []byte) (int, error)        { return c.buf.Write(p) }
func (c *capturingConn) Close() error                       { return nil }
func (c *capturingConn) RemoteAddr() string                 { return c.remoteAddr }
func (c *capturingConn) SetWriteDeadline(t time.Time) error { return nil }

// parseLastJoinResponse 从捕获缓冲解析最后一条 JoinResponse（倒数第一个 '\n' 分隔的消息）。
func parseLastJoinResponse(t *testing.T, buf *bytes.Buffer) *proto.JoinResponse {
	t.Helper()
	data := buf.Bytes()
	if len(data) == 0 {
		t.Fatal("no data written")
	}
	// 最后一条消息：按 '\n' 倒序找最后一个非空行
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) == 0 {
		t.Fatal("no lines in buffer")
	}
	lastLine := lines[len(lines)-1]
	var msg proto.Message
	if err := json.Unmarshal(lastLine, &msg); err != nil {
		t.Fatalf("unmarshal last msg: %v (raw: %s)", err, lastLine)
	}
	if msg.Type != proto.MsgJoinResponse {
		t.Fatalf("expected MsgJoinResponse, got %s", msg.Type)
	}
	var resp proto.JoinResponse
	if err := proto.DecodePayload(&msg, &resp); err != nil {
		t.Fatalf("decode JoinResponse: %v", err)
	}
	return &resp
}

// TestJoinAllInternalFailuresReturnSamePublicError 验证 invalid/expired/has_helper/disconnected
// 四类内部 Join 失败对外返回完全相同的公开响应，不泄露会话状态。
func TestJoinAllInternalFailuresReturnSamePublicError(t *testing.T) {
	cfg := &Config{CodeTTL: 5 * time.Minute, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	// 场景一：code 不存在 → ErrCodeInvalid
	conn1 := &capturingConn{remoteAddr: "10.0.0.1:5000"}
	client1 := &ClientConn{ID: "c1", Conn: conn1}
	srv.handleJoin(client1, "NOTEXIST")
	resp1 := parseLastJoinResponse(t, &conn1.buf)

	// 场景二：code 已过期 → ErrCodeExpired
	share2 := &ClientConn{ID: "share2", Conn: &capturingConn{remoteAddr: "10.0.0.2:5000"}}
	session2, _ := srv.sessions.CreateSession("EXPIRED01", share2, 1*time.Millisecond, "", "10.0.0.2", 100, 1000)
	time.Sleep(50 * time.Millisecond)
	conn2 := &capturingConn{remoteAddr: "10.0.0.2:5001"}
	client2 := &ClientConn{ID: "c2", Conn: conn2}
	srv.handleJoin(client2, "EXPIRED01")
	resp2 := parseLastJoinResponse(t, &conn2.buf)

	// 场景三：session 已有 Help → ErrSessionHasHelper
	share3 := &ClientConn{ID: "share3", Conn: &capturingConn{remoteAddr: "10.0.0.3:5000"}}
	srv.sessions.CreateSession("HASHELP01", share3, 30*time.Minute, "", "10.0.0.3", 100, 1000)
	help3a := &ClientConn{ID: "help3a", Conn: &capturingConn{remoteAddr: "10.0.0.3:5001"}}
	_, _ = srv.sessions.JoinSession("HASHELP01", help3a)
	srv.sessions.ActivateDataPlane(session2.ID, share3.ID, help3a.ID) // 激活后双端在线
	conn3 := &capturingConn{remoteAddr: "10.0.0.3:5002"}
	client3 := &ClientConn{ID: "c3", Conn: conn3}
	srv.handleJoin(client3, "HASHELP01")
	resp3 := parseLastJoinResponse(t, &conn3.buf)

	// 场景四：Share 已断开 → share==nil
	share4 := &ClientConn{ID: "share4", Conn: &capturingConn{remoteAddr: "10.0.0.4:5000"}}
	srv.sessions.CreateSession("DISCO0001", share4, 30*time.Minute, "", "10.0.0.4", 100, 1000)
	srv.sessions.DisconnectClient(share4.ID) // Share 断开
	conn4 := &capturingConn{remoteAddr: "10.0.0.4:5001"}
	client4 := &ClientConn{ID: "c4", Conn: conn4}
	srv.handleJoin(client4, "DISCO0001")
	resp4 := parseLastJoinResponse(t, &conn4.buf)

	// 断言：四种失败的公开响应序列化结果完全相同
	if resp1.Success || resp2.Success || resp3.Success || resp4.Success {
		t.Fatal("所有场景的 Success 必须为 false")
	}
	if resp1.Error != "join failed" || resp2.Error != "join failed" || resp3.Error != "join failed" || resp4.Error != "join failed" {
		t.Fatalf("所有场景必须返回统一 'join failed'，得到: %q, %q, %q, %q", resp1.Error, resp2.Error, resp3.Error, resp4.Error)
	}
	if resp1.SessionID != "" || resp2.SessionID != "" || resp3.SessionID != "" || resp4.SessionID != "" {
		t.Fatal("失败响应不应泄露 SessionID")
	}
}

// TestJoinFiveInternalFailuresCloseConnection 连续 5 次内部 Join 失败后连接关闭。
func TestJoinFiveInternalFailuresCloseConnection(t *testing.T) {
	cfg := &Config{CodeTTL: 5 * time.Minute, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	conn := &capturingConn{remoteAddr: "10.1.1.1:6000"}
	client := &ClientConn{ID: "c-fail5", Conn: conn}

	for i := 1; i <= 4; i++ {
		closeConn := srv.handleJoin(client, "BADCODE0")
		if closeConn {
			t.Fatalf("第 %d 次失败不应关闭连接", i)
		}
		if client.joinFailures != i {
			t.Fatalf("第 %d 次失败后 joinFailures 应=%d，得到 %d", i, i, client.joinFailures)
		}
	}
	closeConn := srv.handleJoin(client, "BADCODE0")
	if !closeConn {
		t.Fatal("第 5 次失败应返回 closeConn=true")
	}
	if client.joinFailures != 5 {
		t.Fatalf("第 5 次失败后 joinFailures 应=5，得到 %d", client.joinFailures)
	}
}

// TestJoinFiveLimiterRejectionsCloseConnection 限流拒绝同样计入失败，第 5 次关闭。
func TestJoinFiveLimiterRejectionsCloseConnection(t *testing.T) {
	cfg := &Config{CodeTTL: 5 * time.Minute, CodeLength: 10, NoAuth: false, DisableSourceIPLimits: false}
	srv, _ := NewServer(cfg)
	// 用极小 limiter 保证每次 Join 都被限流拒绝
	srv.joinLimiterPerIP = ratelimit.NewKeyedLimiter(0, 0, 100, time.Minute)

	conn := &capturingConn{remoteAddr: "10.2.2.2:7000"}
	client := &ClientConn{ID: "c-limiter5", Conn: conn}

	for i := 1; i <= 4; i++ {
		closeConn := srv.handleJoin(client, "ANYCODE0")
		if closeConn {
			t.Fatalf("限流拒绝第 %d 次不应关闭连接", i)
		}
	}
	closeConn := srv.handleJoin(client, "ANYCODE0")
	if !closeConn {
		t.Fatal("限流拒绝第 5 次应返回 closeConn=true")
	}
}

// TestJoinFourFailsThenValidSucceeds 前 4 次失败、第 5 次合法 code 时 Join 成功，不被提前关闭。
func TestJoinFourFailsThenValidSucceeds(t *testing.T) {
	cfg := &Config{CodeTTL: 5 * time.Minute, CodeLength: 10, NoAuth: false}
	srv, _ := NewServer(cfg)

	share := &ClientConn{ID: "share-valid", Conn: &capturingConn{remoteAddr: "10.3.3.3:8000"}}
	srv.sessions.CreateSession("VALIDCODE", share, 30*time.Minute, "", "10.3.3.3", 100, 1000)

	conn := &capturingConn{remoteAddr: "10.3.3.3:8001"}
	client := &ClientConn{ID: "c-4fail", Conn: conn}

	for i := 1; i <= 4; i++ {
		srv.handleJoin(client, "WRONG000")
	}
	if client.joinFailures != 4 {
		t.Fatalf("4 次失败后 joinFailures 应=4，得到 %d", client.joinFailures)
	}

	// 第 5 次用合法 code
	closeConn := srv.handleJoin(client, "VALIDCODE")
	if closeConn {
		t.Fatal("第 5 次合法 Join 不应关闭连接")
	}
	resp := parseLastJoinResponse(t, &conn.buf)
	if !resp.Success {
		t.Fatalf("第 5 次合法 Join 应成功，得到 Success=%v Error=%q", resp.Success, resp.Error)
	}
	if client.joinFailures != 0 {
		t.Fatalf("Join 成功后 joinFailures 应清零，得到 %d", client.joinFailures)
	}
	if client.Type != "help" {
		t.Fatalf("Join 成功后应进入 help 角色，得到 %q", client.Type)
	}
}
