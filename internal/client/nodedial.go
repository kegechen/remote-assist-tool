package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
)

// 按节点拨号（spec 6.3）：节点列表里每个节点带着它的 TCP/QUIC 地址和证书指纹。连节点时
//   - 地址按 nodeDialOrder 排：本机专用地址最前，然后列表里的地址，一个一个试；
//   - 每个地址只用它声明过的传输（只在 QUIC 里列出的地址不拨 TCP），都有时照旧 TCP/QUIC 竞速；
//   - 证书必须是列表里这个节点的指纹之一（严格比对，不走 TOFU，也不往指纹表里写）。
//
// 这里只管「拨通并拿到连接」，不决定拨哪些节点（nodepick.go）、也不碰 Client 的状态：自动选节点要同时拨
// 几个节点、输的那路直接关掉，连接建好之后才由赢家交给 Client（adoptNode）。

// CertMismatchError 表示某个地址出示的证书不在节点列表里：可能是中间人，也可能是节点换了证书而列表还没更新。
type CertMismatchError struct {
	Addr string
	FP   string // 本次出示的证书指纹（小写 hex）
}

func (e *CertMismatchError) Error() string {
	fp := e.FP
	if len(fp) > 16 {
		fp = fp[:16]
	}
	return fmt.Sprintf("%s 出示的证书（指纹 %s…）与节点列表不符，可能被劫持", e.Addr, fp)
}

// addrFailure 是一个地址上的失败。
type addrFailure struct {
	Addr string
	Err  error
}

// nodeConn 是 dialNode 建好的连接。
type nodeConn struct {
	Conn      net.Conn
	Addr      string // 实际连上的地址
	Transport string // 实际用的传输
	// Mode 是这个地址的拨号方式（targetTransport 的结果）：auto 表示 TCP/QUIC 竞速。
	Mode string
	// FPs 是这个节点在列表里的证书指纹，Client 接管后重连时照样按它严格比对。
	FPs []string
	// TCPErr 是 auto 模式下 QUIC 胜出时 TCP 一路的失败原因（TCP 还没返回为 nil），见 dialRelayAddr。
	TCPErr error
	// Failed 是更早试过、没连上的地址。调用方据此处理本机专用地址（连不上就清掉）。
	// 只含真的拨过且失败了的地址：被取消的、因为配置不能拨的都不在里面。
	Failed []addrFailure
}

// nodeDialError 是一个节点所有地址都没连上。
type nodeDialError struct {
	// Failures 是拨过且失败的地址，顺序同拨号顺序。
	Failures []addrFailure
	// Skipped 是没拨的地址：用户用 --transport 限定了地址不提供的传输。不是地址的问题，不该算它连不上。
	Skipped []addrFailure
	// Canceled 是调用方取消（比如别的节点已经赢了）：非 nil 时后面的地址没有再拨，被取消的那次也不算地址连不上。
	Canceled error
}

func (e *nodeDialError) Error() string {
	var parts []string
	for _, f := range e.Failures {
		if cm := (*CertMismatchError)(nil); errors.As(f.Err, &cm) && cm.Addr == f.Addr {
			parts = append(parts, f.Err.Error()) // 自己带着地址
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %v", f.Addr, f.Err))
	}
	for _, f := range e.Skipped {
		parts = append(parts, fmt.Sprintf("%s: 跳过（%v）", f.Addr, f.Err))
	}
	if e.Canceled != nil {
		parts = append(parts, e.Canceled.Error())
	}
	if len(parts) == 0 {
		return "没有可拨的地址"
	}
	return strings.Join(parts, "; ")
}

func (e *nodeDialError) Unwrap() []error {
	var errs []error
	for _, f := range e.Failures {
		errs = append(errs, f.Err)
	}
	for _, f := range e.Skipped {
		errs = append(errs, f.Err)
	}
	if e.Canceled != nil {
		errs = append(errs, e.Canceled)
	}
	return errs
}

// targetTransport 算一个地址实际要用的传输：用户的 --transport（空串=auto）与地址声明过的传输取交集。
// 地址两种都声明且用户没限定时仍是 auto（竞速）；交集为空返回错误，不悄悄换成别的传输。
func targetTransport(user string, t nodeTarget) (string, error) {
	if user == "" {
		user = TransportAuto
	}
	if !ValidTransport(user) {
		return "", fmt.Errorf("unknown transport %q (want auto, tcp or quic)", user)
	}
	switch {
	case t.TCP && t.QUIC:
		return user, nil
	case t.TCP:
		if user == TransportQUIC {
			return "", errors.New("地址没有 QUIC，--transport quic 无法使用")
		}
		return TransportTCP, nil
	case t.QUIC:
		if user == TransportTCP {
			return "", errors.New("地址只有 QUIC，--transport tcp 无法使用")
		}
		return TransportQUIC, nil
	}
	return "", errors.New("地址没有声明任何传输")
}

// dialNode 连节点 n：按 nodeDialOrder 逐个地址拨，第一个连上的返回。local 是本机专用地址（可为空）。
// userMode 是用户的 --transport，preferred 是上次成功的传输（见 dialRelayAddr）。ctx 取消时拨号中止。
//
// 证书指纹来自 n.TLSFPs；没有指纹的节点不拨（没有身份校验的连接不能当节点用）。
// 某个地址证书不符、连不上都记进 Failed/Failures 后继续试下一个地址；被 relay 封禁则马上结束
// （同一台 relay 的其余地址一样被拒），错误仍能用 errors.Is(err, proto.ErrBlocked) 认出。
//
// ctx 只用来取消（比如别的节点已经赢了），不要用截止时间限制总时长：截止时间到了也会被当成取消，
// 正在拨的地址不算连不上。每个地址各自受 dialTimeout 限制。
func dialNode(ctx context.Context, n directory.Node, local, userMode, preferred string) (*nodeConn, error) {
	if len(n.TLSFPs) == 0 {
		return nil, errors.New("节点在列表里没有证书指纹，不能连接")
	}
	var de nodeDialError
	for _, t := range nodeDialOrder(n, local) {
		mode, err := targetTransport(userMode, t)
		if err != nil {
			de.Skipped = append(de.Skipped, addrFailure{Addr: t.Addr, Err: err})
			continue
		}
		conn, transport, tcpErr, err := dialStrict(ctx, t.Addr, mode, n.TLSFPs, preferred)
		if err == nil {
			return &nodeConn{
				Conn: conn, Addr: t.Addr, Transport: transport, Mode: mode,
				FPs: append([]string(nil), n.TLSFPs...), TCPErr: tcpErr, Failed: de.Failures,
			}, nil
		}
		if ctx.Err() != nil {
			// 调用方取消了（比如别的节点已经赢了）：这次失败不是地址的问题，不能让调用方据此删掉
			// 一个好好的本机专用地址；后面的地址也不用再拨了。证书不符恰好撞上取消时同样不记，
			// 下次再拨这个节点时照样会发现。
			de.Canceled = ctx.Err()
			break
		}
		de.Failures = append(de.Failures, addrFailure{Addr: t.Addr, Err: err})
		if errors.Is(err, proto.ErrBlocked) {
			break
		}
	}
	return nil, &de
}

// nodePin 是 Client 当前所在的节点地址：Close 之后再 Connect 也回到这里，用同样的指纹和传输。
//
// 指纹是接管那一刻的快照，列表更新（换证书、吊销）后不会跟着变：节点证书因私钥泄露被吊销后，靠 Connect
// 重连的话照样认旧指纹。所以它只是兜底，保证节点模式下的 Connect 不退回 TOFU；长期运行、会断线重连的
// 调用方（share 自动模式）每次重连都要按最新的列表重新 dialNode 再 adoptNode，不能调 Connect。
type nodePin struct {
	fps  []string
	mode string
}

// adoptNode 接管 dialNode 建好的连接，相当于一次成功的 Connect：之后照常注册/加入、心跳、读写。
// Close 后再 Connect 也回到这个地址，用接管时的指纹严格比对（快照，见 nodePin）。
// Client 还连着时先关掉旧连接、叫停旧的心跳：旧码不能一直挂在旧 relay 上，旧心跳也不能往新连接上发。
//
// 调用方要保证没有别的 goroutine 正在这个 Client 上读写（share 在主循环里、Close 之后才接管）：
// 阻塞在旧连接上的读会因为旧连接被关而出错，读的一方会当成断线去重连，把刚接管的连接也关掉。
// config.ServerAddr 也是在这里改的，别的 goroutine 要读它得持有 c.mu。
func (c *Client) adoptNode(r *nodeConn) {
	c.mu.Lock()
	old, oldStop := c.conn, c.hbStop // Close 之后两者都是 nil
	c.closed = false
	c.hbStop = make(chan struct{})
	if r.Mode == TransportAuto {
		// 地址只声明了一种传输时没有竞速，打「TCP 不可达，已改用 QUIC」会误导。
		// 换了地址时，上一次的传输是别的地址上的，不能拿来说「已恢复走 TCP」。
		previous := c.transport
		if !strings.EqualFold(r.Addr, c.config.ServerAddr) {
			previous = ""
		}
		logTransport(r.Addr, r.Transport, previous, r.TCPErr)
	}
	c.config.ServerAddr = r.Addr
	c.node = &nodePin{fps: append([]string(nil), r.FPs...), mode: r.Mode}
	c.transport = r.Transport
	c.installConnLocked(r.Conn)
	c.mu.Unlock()

	if oldStop != nil {
		close(oldStop)
	}
	if old != nil {
		old.Close()
	}
}

// connectNodeLocked 是节点模式下的 Connect：重连 adoptNode 记下的地址。调用方持有 c.mu。
func (c *Client) connectNodeLocked() error {
	conn, transport, tcpErr, err := dialStrict(context.Background(), c.config.ServerAddr, c.node.mode, c.node.fps, c.transport)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	if c.node.mode == TransportAuto {
		logTransport(c.config.ServerAddr, transport, c.transport, tcpErr)
	}
	c.transport = transport
	c.installConnLocked(conn)
	return nil
}

// dialStrict 拨 addr，严格比对证书指纹。证书不符时返回 *CertMismatchError：QUIC 把握手错误转成文本，
// errors.Is 认不出，所以靠 TLS 回调记下出示的指纹，再据此换掉传输层的错误。
func dialStrict(ctx context.Context, addr, mode string, fps []string, preferred string) (conn net.Conn, transport string, tcpErr, err error) {
	if len(fps) == 0 {
		// 没有指纹时 TLS 配置会退回 PKI 校验，那就不是按列表认身份了。
		return nil, "", nil, errors.New("没有证书指纹，不能按节点列表校验身份")
	}
	var mismatched atomic.Pointer[string]
	tlsConfig, err := crypto.NewClientTLSConfig(crypto.ClientTLSOptions{
		AllowedFPs:   fps,
		OnFPMismatch: func(fp string) { mismatched.CompareAndSwap(nil, &fp) },
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to create TLS config: %w", err)
	}
	conn, transport, tcpErr, err = dialRelayAddr(ctx, addr, mode, tlsConfig, preferred)
	if err != nil {
		// 封禁优先：auto 模式下一路出示了别的证书、另一路被封禁时，要让调用方认出封禁、不再拨同一台 relay。
		if fp := mismatched.Load(); fp != nil && !errors.Is(err, proto.ErrBlocked) {
			return nil, "", nil, &CertMismatchError{Addr: addr, FP: *fp}
		}
		return nil, "", nil, err
	}
	if fp := mismatched.Load(); fp != nil {
		// auto 模式下另一路出示了列表之外的证书：连上的这路校验过了，可那一路很可能被劫持，得让用户知道。
		// 另一路要是在这之后才握手（竞速里还没返回），这里看不到，下次拨号时照样会发现。
		log.Printf("[relay] 警告：%v；已改用 %s 连上", &CertMismatchError{Addr: addr, FP: *fp}, transport)
	}
	return conn, transport, tcpErr, nil
}
