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
	"github.com/remote-assist/tool/internal/proto"
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

// dialRelay 按 Config.Transport 建立到 Config.ServerAddr 的连接（含 TLS 握手），返回实际使用的传输。
// preferred 是上一次成功的传输（空=TCP），auto 模式下它先拨。auto 模式下 QUIC 胜出时，
// tcpErr 是 TCP 一路的失败原因（TCP 还没返回为 nil），供 logTransport 如实说明为什么改用 QUIC。
func (c *Client) dialRelay(tlsConfig *tls.Config, preferred string) (conn net.Conn, transport string, tcpErr, err error) {
	return dialRelayAddr(context.Background(), c.config.ServerAddr, c.config.Transport, tlsConfig, preferred)
}

// dialRelayAddr 是 dialRelay 的无状态版本：拨 addr，传输模式 mode（空串=auto），parent 取消时拨号中止。
// 整个拨号（TCP/QUIC 竞速、各自的 TLS 握手）受 dialTimeout 限制。已经建好的连接不受 parent 之后取消的影响。
// 自动选节点要同时拨几个节点、各自带着不同的地址与证书指纹，所以不能走 Client 里的单一配置。
func dialRelayAddr(parent context.Context, addr, mode string, tlsConfig *tls.Config, preferred string) (conn net.Conn, transport string, tcpErr, err error) {
	if mode == "" {
		mode = TransportAuto
	}
	if !ValidTransport(mode) {
		return nil, "", nil, fmt.Errorf("unknown transport %q (want auto, tcp or quic)", mode)
	}
	// QUIC 强制 TLS。明文 relay（--tls=false，仅限内网/测试）只能走 TCP。
	if tlsConfig == nil {
		if mode == TransportQUIC {
			return nil, "", nil, errors.New("transport quic requires TLS")
		}
		mode = TransportTCP
	}

	ctx, cancel := context.WithTimeout(parent, dialTimeout)
	defer cancel()
	// 先取到本地：竞速里的 goroutine 可能比这次调用活得久，读全局变量会和后面的测试替换它冲突。
	dial := dialOneFunc

	switch mode {
	case TransportTCP, TransportQUIC:
		conn, err := dial(ctx, addr, mode, tlsConfig)
		return conn, mode, nil, err
	}

	first, second := TransportTCP, TransportQUIC
	if preferred == TransportQUIC {
		first, second = TransportQUIC, TransportTCP
	}
	results := make(chan dialResult, 2)
	start := func(kind string) {
		go func() {
			conn, err := dial(ctx, addr, kind, tlsConfig)
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
			// 已经取消了就不起另一路（理由同下），等首选一路因取消返回。
			if started == 1 && ctx.Err() == nil {
				start(second)
				started = 2
			}
		case r := <-results:
			received++
			if r.err == nil {
				cancel()
				closeLosers(results, started-received)
				return r.conn, r.transport, tcpErr, nil
			}
			if errors.Is(r.err, proto.ErrBlocked) {
				// relay 已明确拒绝本机：封禁与传输无关，换 QUIC 也一样被拒，还会让用户
				// 先看到一行误导的「TCP 不可达，已改用 QUIC」。直接结束竞速，不等另一路。
				cancel()
				closeLosers(results, started-received)
				return nil, "", nil, r.err
			}
			if r.transport == TransportTCP {
				tcpErr = r.err
			}
			errs = append(errs, fmt.Errorf("%s: %w", r.transport, r.err))
			if started == 1 {
				if ctx.Err() != nil {
					// 调用方取消了（或者到了截止时间）：别拿已取消的 ctx 再起另一路。quic-go 这时发不发
					// Initial 包取决于它内部的时序，发了就是 relay 那边一次没头没尾的握手。
					return nil, "", nil, errors.Join(errs...)
				}
				// 首选一路已经明确失败（如端口拒绝），不必等满延迟，立刻换另一路。
				start(second)
				started = 2
				continue
			}
			if received == started {
				return nil, "", nil, errors.Join(errs...)
			}
		}
	}
}

// closeLosers 在后台收齐竞速中还没返回的 pending 路结果：它们可能稍后也握手成功，
// 关掉，不泄漏连接。
func closeLosers(results <-chan dialResult, pending int) {
	if pending <= 0 {
		return
	}
	go func() {
		for i := 0; i < pending; i++ {
			if lost := <-results; lost.conn != nil {
				abortConn(lost.conn)
			}
		}
	}()
}

// dialOneFunc 是 dialRelayAddr 拨每一路用的函数，测试替换它来观察拨了哪几路。
var dialOneFunc = dialOne

func dialOne(ctx context.Context, addr, kind string, tlsConfig *tls.Config) (net.Conn, error) {
	if kind == TransportQUIC {
		// relay 对被封 IP 的 QUIC 连接在它那边握手完成后才带原因关闭，那时这边的握手早已
		// 完成，封禁通常由 quictransport.Conn 的读写识别；但关闭也可能赶在开流之前到。
		qc, err := quic.DialAddr(ctx, addr, quictransport.TLSConfig(tlsConfig), quictransport.ClientConfig())
		if err != nil {
			return nil, err
		}
		stream, err := qc.OpenStreamSync(ctx)
		if err != nil {
			_ = qc.CloseWithError(0, "")
			if quictransport.IsBlockedClose(err) {
				return nil, proto.ErrBlocked
			}
			return nil, err
		}
		return quictransport.NewClientConn(qc, stream), nil
	}
	dialer := &net.Dialer{}
	if tlsConfig == nil {
		// 明文 relay（仅内网/测试）被封时不回任何信号，无从识别。
		return dialer.DialContext(ctx, "tcp", addr)
	}
	conn, err := (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	if err != nil {
		// relay 对被封 IP 的 TCP 连接不做 TLS 握手，回一条 TLS 致命 alert access_denied。
		if isTLSAccessDenied(err) {
			return nil, proto.ErrBlocked
		}
		return nil, err
	}
	return conn, nil
}

// isTLSAccessDenied 报告 err 是否是对端发来的 TLS 致命 alert access_denied(49)。
// crypto/tls 把收到的 alert 包成 &net.OpError{Op: "remote error", Err: alert}，alert
// 类型未导出，只能比对它的文本。
func isTLSAccessDenied(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "remote error" && op.Err != nil &&
		op.Err.Error() == "tls: access denied"
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
// 只会刷屏。tcpErr 是 TCP 一路的失败原因（见 dialRelay）。
func logTransport(addr, transport, previous string, tcpErr error) {
	if transport == TransportQUIC && previous != TransportQUIC {
		if tcpErr == nil || tcpUnreachable(tcpErr) {
			log.Printf("relay %s 的 TCP 不可达，已改用 QUIC（UDP）连接", addr)
		} else {
			log.Printf("relay %s 的 TCP 连接失败（%v），已改用 QUIC（UDP）连接", addr, tcpErr)
		}
	} else if transport == TransportTCP && previous == TransportQUIC {
		log.Printf("relay %s 已恢复走 TCP 连接", addr)
	}
}

// tcpUnreachable 报告 TCP 失败是不是「连不上」：拨号阶段就失败（拒绝、不可达）或超时。
// 连上之后被对端关闭、重置、TLS 握手失败都不算——那时说「不可达」会把排查引向网络。
// 例如 relay 给被封连接回提示的名额占满时会直接关连接，客户端回落 QUIC 后才收到封禁。
func tcpUnreachable(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}
