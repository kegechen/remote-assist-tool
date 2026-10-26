package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/remote-assist/tool/internal/quictransport"
)

// 传输方式（--transport）。
const (
	TransportAuto = "auto" // TCP 与 QUIC 竞速，默认
	TransportTCP  = "tcp"  // 只走 TCP（旧行为）
	TransportQUIC = "quic" // 只走 QUIC（UDP）
)

// quicRaceDelay auto 模式下首选传输先跑这么久，还没握完手才并行拨另一路。
//
// TCP 正常时握手远小于 3s，QUIC 根本不会被拨，不给 relay 和网络添额外负载；被丢回程
// TCP 的 relay（SYN 到得了、SYN-ACK 回不来）会让 TCP 一直挂到 dialTimeout，这时 3s 后
// QUIC 接手，用户只多等 3s 而不是 20s。var 仅为测试可调。
var quicRaceDelay = 3 * time.Second

// ValidTransport 报告 s 是否是合法的 --transport 取值（空串视作 auto）。
func ValidTransport(s string) bool {
	switch s {
	case "", TransportAuto, TransportTCP, TransportQUIC:
		return true
	}
	return false
}

type dialResult struct {
	conn      net.Conn
	transport string
	err       error
}

// dialRelay 按 Config.Transport 建立到 relay 的连接（含 TLS 握手），返回实际使用的传输。
// preferred 是上一次成功的传输（空=TCP），auto 模式下它先拨。
func (c *Client) dialRelay(tlsConfig *tls.Config, preferred string) (net.Conn, string, error) {
	mode := c.config.Transport
	if mode == "" {
		mode = TransportAuto
	}
	if !ValidTransport(mode) {
		return nil, "", fmt.Errorf("unknown transport %q (want auto, tcp or quic)", mode)
	}
	// QUIC 强制 TLS。明文 relay（--tls=false，仅限内网/测试）只能走 TCP。
	if tlsConfig == nil {
		if mode == TransportQUIC {
			return nil, "", errors.New("transport quic requires TLS")
		}
		mode = TransportTCP
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	switch mode {
	case TransportTCP, TransportQUIC:
		conn, err := c.dialOne(ctx, mode, tlsConfig)
		return conn, mode, err
	}

	first, second := TransportTCP, TransportQUIC
	if preferred == TransportQUIC {
		first, second = TransportQUIC, TransportTCP
	}
	results := make(chan dialResult, 2)
	start := func(kind string) {
		go func() {
			conn, err := c.dialOne(ctx, kind, tlsConfig)
			results <- dialResult{conn: conn, transport: kind, err: err}
		}()
	}

	start(first)
	started, received := 1, 0
	timer := time.NewTimer(quicRaceDelay)
	defer timer.Stop()
	var errs []error
	for {
		select {
		case <-timer.C:
			if started == 1 {
				start(second)
				started = 2
			}
		case r := <-results:
			received++
			if r.err == nil {
				cancel()
				// 输掉的那一路可能稍后也握手成功：收尾时关掉，不泄漏连接。
				if pending := started - received; pending > 0 {
					go func() {
						for i := 0; i < pending; i++ {
							if lost := <-results; lost.conn != nil {
								abortConn(lost.conn)
							}
						}
					}()
				}
				return r.conn, r.transport, nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", r.transport, r.err))
			if started == 1 {
				// 首选一路已经明确失败（如端口拒绝），不必等满延迟，立刻换另一路。
				start(second)
				started = 2
				continue
			}
			if received == started {
				return nil, "", errors.Join(errs...)
			}
		}
	}
}

func (c *Client) dialOne(ctx context.Context, kind string, tlsConfig *tls.Config) (net.Conn, error) {
	addr := c.config.ServerAddr
	if kind == TransportQUIC {
		qc, err := quic.DialAddr(ctx, addr, quictransport.TLSConfig(tlsConfig), quictransport.ClientConfig())
		if err != nil {
			return nil, err
		}
		stream, err := qc.OpenStreamSync(ctx)
		if err != nil {
			_ = qc.CloseWithError(0, "")
			return nil, err
		}
		return quictransport.NewClientConn(qc, stream), nil
	}
	dialer := &net.Dialer{}
	if tlsConfig == nil {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	return (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", addr)
}

// abortConn 关闭竞速中输掉、从未使用过的连接。QUIC 直接中止，不让 relay 看到一条空流。
func abortConn(conn net.Conn) {
	if qc, ok := conn.(*quictransport.Conn); ok {
		qc.Abort()
		return
	}
	conn.Close()
}

// logTransport 只在实际走了 QUIC、或传输与上次不同时打一行：TCP 是常态，每次重连都打
// 只会刷屏。
func logTransport(addr, transport, previous string) {
	if transport == TransportQUIC && previous != TransportQUIC {
		log.Printf("relay %s 的 TCP 不可达，已改用 QUIC（UDP）连接", addr)
	} else if transport == TransportTCP && previous == TransportQUIC {
		log.Printf("relay %s 已恢复走 TCP 连接", addr)
	}
}
