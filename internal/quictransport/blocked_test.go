package quictransport

import (
	"errors"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

func readErr(t *testing.T, c *Conn) error {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	for {
		if _, err := c.Read(buf); err != nil {
			return err
		}
	}
}

// relay 以封禁原因关闭连接：客户端连接上的 Read 与之后的 Write 都返回 proto.ErrBlocked。
func TestClientConnMapsBlockedClose(t *testing.T) {
	client, server := pairWith(t, NewClientConn)
	_ = server.conn.CloseWithError(0, proto.BlockedMessage)

	if err := readErr(t, client); !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("Read 错误 = %v，期望 ErrBlocked", err)
	}
	if _, err := client.Write([]byte("x")); !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("Write 错误 = %v，期望 ErrBlocked", err)
	}
}

// 关闭原因不是封禁文案（旧 relay 不带原因）：不能被认成封禁。
func TestClientConnOtherCloseNotBlocked(t *testing.T) {
	client, server := pairWith(t, NewClientConn)
	_ = server.conn.CloseWithError(0, "")
	if err := readErr(t, client); err == nil || errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("Read 错误 = %v，期望普通关闭错误", err)
	}
}

// relay 侧连接不做映射：客户端关连接时填什么原因都不该改变 relay 看到的错误。
func TestServerConnDoesNotMapBlockedClose(t *testing.T) {
	client, server := pair(t)
	_ = client.conn.CloseWithError(0, proto.BlockedMessage)
	if err := readErr(t, server); err == nil || errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("relay 侧 Read 错误 = %v，不应映射为 ErrBlocked", err)
	}
}
