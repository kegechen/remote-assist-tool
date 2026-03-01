package relay

import (
	"io"
	"sync"
)

// Tunnel 双向隧道
type Tunnel struct {
	Share  *ClientConn
	Help   *ClientConn
	closed bool
	mu     sync.Mutex
}

// NewTunnel 创建隧道
func NewTunnel(share, help *ClientConn) *Tunnel {
	return &Tunnel{
		Share: share,
		Help:  help,
	}
}

// Start 启动隧道转发
func (t *Tunnel) Start() {
	go t.copyLoop(t.Share, t.Help)
	go t.copyLoop(t.Help, t.Share)
}

// Close 关闭隧道
func (t *Tunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}
	t.closed = true

	if t.Share != nil && t.Share.Conn != nil {
		t.Share.Conn.Close()
	}
	if t.Help != nil && t.Help.Conn != nil {
		t.Help.Conn.Close()
	}
}

func (t *Tunnel) copyLoop(src, dst *ClientConn) {
	defer t.Close()

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Conn.Read(buf)
		if err != nil {
			if err != io.EOF {
			}
			return
		}

		_, err = dst.Conn.Write(buf[:n])
		if err != nil {
			return
		}
	}
}
