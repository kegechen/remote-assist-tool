package client

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/proto"
)

// writeTimeout 单次写超时兜底：对端慢读 / 半死隧道撑满发送缓冲会让 enc.Encode 无限
// 阻塞并死占 c.mu，进而冻结心跳、SetReadDeadline 与读循环 → 整个 MCP 拖死。超时按写
// 失败返回，触发上层断线处理。对齐 relay 侧同名 writeTimeout。
const writeTimeout = 30 * time.Second

// dialTimeout 建连（含 TLS 握手）超时。
//
// 裸 net.Dial / tls.Dial 不带任何超时，交给内核的 TCP 重传退避——Linux 上是 ~130s，
// 而黑洞 relay（SYN 有回应、TLS 握手不回）根本等不到内核放弃，会永久挂死。这一条在
// MCP 里格外要命：doConnect 入口就持有 connectMu，一个卡住的 connect 会让之后所有
// connect 排队等它，整个 MCP server 只能重启。
//
// dialRelay 把它作为整个拨号（TCP/QUIC 竞速、各自的 TLS 握手）的 context 截止时间，
// 两段都盖住。
var dialTimeout = 20 * time.Second

// joinTimeout 等待 relay 回 JoinResponse / RegisterResponse 的超时。TCP 连上、TLS 也
// 握完，但 relay 迟迟不回应答的情况（进程假死、被中间盒吞掉）同样会让 ReadMessage
// 永久阻塞，光有 dialTimeout 盖不住。
//
// dialTimeout/joinTimeout 用 var 而非 const，仅为让测试能调小到毫秒级；运行期不改。
var joinTimeout = 15 * time.Second

// downlinkProbeTimeout 是 share 注册、help 加入之后确认还收得到 relay 数据（probeDownlink）时最多等多久。relay 收到心跳立刻回，
// 正常是一个往返；留够丢包重传的余量。var 仅为测试可调。
var downlinkProbeTimeout = 10 * time.Second

// relaySilenceTimeout 是连着 relay 时最多多久收不到它的任何消息，超过就当连接已经不通。两端每 30 秒发一次心跳，
// relay 每个都回（最早的版本就是这样），正常时最多隔 30 秒就有一条；这里留出三个心跳周期。
//
// TCP 连得上、relay 发来的数据却被网络丢掉时（下行被干扰），只有这样才发现得了：心跳照常发得出去（写不失败），
// relay 回的不带数据的 ACK 也照常过得来，TCP keepalive 一切正常。var 仅为测试可调。
var relaySilenceTimeout = 90 * time.Second

// relaySilenceWhat 是 relaySilentError 里等的东西：连着 relay 时一条消息都没等到。
const relaySilenceWhat = "任何消息（心跳也没有回应）"

// RelaySilentMarker 是 relaySilentError 文案里稳定的片段（Error 拼出「中转服务 X 在 N 秒内没有发来…」）。
// 错误经 MCP 传到 GUI 时只剩文本，GUI 的错误分类靠它认出这是「中转服务收不到回应」的超时类，
// 不然会落到 join failed 被说成协助码错误。
const RelaySilentMarker = "内没有发来"

// relaySilentError 表示 relay 在时限内一条消息都没发来：请求或心跳发出去了，等的回应没有到。
// TCP 握手成功、请求的应答也到过，不代表之后的数据还过得来：被网络干扰时，relay 发回的数据可能从某一刻起
// 全被丢掉，而发出去的方向一切正常。
type relaySilentError struct {
	addr      string
	transport string // 当时连接用的传输
	what      string // 等的是什么
	wait      time.Duration
	// explicitTCP：用户用 --transport tcp 限定了只走 TCP，提示可以改用 QUIC。
	explicitTCP bool
	// fallback：当时是 auto 模式下的 TCP，已经记下这个地址的 TCP 收不到数据（markTCPStalled），之后拨它先走 QUIC。
	fallback bool
	err      error // 底层的读超时
}

func (e *relaySilentError) Error() string {
	msg := fmt.Sprintf("中转服务 %s 在%s%s%s", e.addr, waitText(e.wait), RelaySilentMarker, e.what)
	if e.transport == TransportTCP {
		msg += "（TCP 连接还在，但收不到它发回的数据，可能被网络干扰）"
		if e.explicitTCP {
			msg += "，可以改用 --transport quic 再试"
		}
	}
	return msg
}

func (e *relaySilentError) Unwrap() error { return e.err }

// waitText 是提示里「在…内」中间的时长：整秒写「 15 秒」，不到一秒（只有测试会这样）照 Duration 的写法、两边带空格。
func waitText(d time.Duration) string {
	if d < time.Second {
		return " " + d.String() + " "
	}
	return fmt.Sprintf(" %d 秒", int(d.Round(time.Second)/time.Second))
}

// quicRetryNote 是重试提示里传输那一段：TCP 收不到数据、这次改走 QUIC 时为「改走 QUIC（UDP）」，否则为空。
func quicRetryNote(err error) string {
	var se *relaySilentError
	if errors.As(err, &se) && se.fallback {
		return "改走 QUIC（UDP）"
	}
	return ""
}

// Config 客户端配置
type Config struct {
	ServerAddr   string
	InsecureSkip bool
	CAFile       string
	UseTLS       bool
	P2PMode      string // "disabled", "auto", "required"
	STUNServer   string // STUN server address for P2P
	BindIP       string // 手动指定绑定 IP，为空则自动检测

	// TrustNewCert 对应 --trust-new-cert：relay 证书指纹与首次连接时不一致也接受，
	// 并覆盖记录。只在 InsecureSkip（默认）路径下有意义，见 crypto/tofu.go。
	TrustNewCert bool

	// MinProto 对应 --min-proto：可接受的最低工具协议版本。空串视作
	// proto.DefaultMinProto，即不降级。放宽到 "1" 才会与 0.0.x 的旧客户端互通。
	MinProto string
	// Transport 对应 --transport：auto（空串同义，TCP 与 QUIC 竞速）、tcp、quic。
	Transport string
	// trustStore 仅供测试注入自定义指纹表位置；零值走 ~/.remote_assist_known_hosts。
	trustStore *crypto.TrustStore
}

// relayDesc 描述实际连上的 relay：地址 + 传输方式。
//
// 值得单独打一行：最终连的是哪台，是「编译期默认值 → REMOTE_RELAY_SERVER →
// --server → NormalizeServerAddr 补默认端口 → --standalone 改写成 loopback」这一串
// 处理的结果，光看命令行看不出来（尤其编译期默认值随构建而变）。传输方式同理：
// TLS/明文 配错的现场表现只是一个没头没尾的 join EOF。
func relayDesc(cfg *Config) string {
	mode := "明文"
	if cfg.UseTLS {
		mode = "TLS"
		if cfg.InsecureSkip {
			mode = "TLS，跳过证书校验"
		}
	}
	return fmt.Sprintf("%s (%s)", cfg.ServerAddr, mode)
}

// logPinResult 把 TOFU 的结果打成一行。
//
// 只有「首次学习」和「显式换证书」值得打——匹配是常态，每次连接刷一行只会让人学会
// 忽略它，真出事那天也照样忽略。指纹只取前 16 个 hex（64 bit），够人肉核对，又不至于
// 让一行日志变成两行。
func logPinResult(addr string) func(crypto.PinResult, string) {
	return func(r crypto.PinResult, fp string) {
		short := fp
		if len(short) > 16 {
			short = short[:16]
		}
		switch r {
		case crypto.PinLearned:
			log.Printf("已记住 %s 的 relay 证书指纹 %s…（首次连接）。此后指纹变化将被拒绝。", addr, short)
		case crypto.PinReplaced:
			log.Printf("已按 --trust-new-cert 更新 %s 的 relay 证书指纹为 %s…", addr, short)
		}
	}
}

// Client 基础客户端
type Client struct {
	config *Config
	conn   net.Conn
	enc    *json.Encoder
	dec    *json.Decoder
	closed bool
	// hbStop 标识"当前这一代连接"，每次 Connect 换新、Close 时关闭。
	// 心跳循环必须绑定它而不是 closed 标志：share 全程复用同一个 *Client，
	// reconnectWithBackoff 每轮 Close() 后立刻 Connect()，而 Connect 第一行就把
	// closed 复位成 false，两者之间只隔几微秒——30s 周期的 tick 几乎不可能命中
	// 那个窗口，于是旧心跳 goroutine 检查 IsClosed() 得到 false，继续用新连接发
	// 心跳，每次重连再叠一个，永不退出。
	hbStop chan struct{}
	// transport 上一次成功建连用的传输（tcp/quic），auto 模式下次先拨它。只记在进程内：
	// 网络环境变了不该一直沿用旧选择，进程重启即重新从 TCP 开始。
	transport string
	// node 非 nil 表示当前连的是节点列表里的节点（adoptNode 接管的连接），之后的 Connect 照样按接管时的
	// 指纹严格比对、只用这个地址声明过的传输，不退回 TOFU：那等于把身份校验降级，还会把节点地址写进指纹表。
	// 指纹是接管时的快照，见 nodePin：会断线重连的调用方要按最新列表重新 dialNode，不要靠 Connect。
	node *nodePin
	// listed 是 TOFU 连接时直接放行的证书指纹，不查也不写指纹表（crypto.ClientTLSOptions.ListedFPs）。只有自动模式的进程里
	// 指定了某节点本机专用地址的连接（MCP connect 带 server）设它，是那个节点的指纹，见 bindAddr。
	listed []string
	// peerFP 当前连接上 relay 出示的叶子证书指纹，没有 TLS 时为空。share 据此判断重连后连的是不是同一台 relay。
	peerFP string
	// pending 是 probeDownlink 顺带读到的消息（不是心跳回应的那些），之后的 ReadMessage 先返回它们。换连接时清空。
	pending []*proto.Message
	mu      sync.Mutex
	// stunMu/relaySTUN：relay 在注册/加入响应里下发的 STUN 服务器（每次注册/加入覆盖）。
	// 写在注册/加入的 goroutine，读在后台启动 P2P 的 goroutine，单独加锁。
	stunMu    sync.Mutex
	relaySTUN []string
}

// setRelaySTUN 记下 relay 下发的 STUN 服务器；nil 表示 relay 未下发。
func (c *Client) setRelaySTUN(servers []string) {
	c.stunMu.Lock()
	c.relaySTUN = append([]string(nil), servers...)
	c.stunMu.Unlock()
}

// serverAddr 返回当前连的 relay 地址。自动模式下 adoptNode 会在持锁时改它，别的 goroutine 要经这里读。
func (c *Client) serverAddr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config.ServerAddr
}

// peerFingerprint 返回当前连接上 relay 出示的证书指纹（见 peerFP），没连着时为空。
func (c *Client) peerFingerprint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peerFP
}

// connPeerFP 取连接上对端出示的叶子证书指纹。*tls.Conn 和 QUIC 连接（quictransport.Conn）都有 ConnectionState；
// 明文连接没有，返回空串。
func connPeerFP(conn net.Conn) string {
	cs, ok := conn.(interface{ ConnectionState() tls.ConnectionState })
	if !ok {
		return ""
	}
	if certs := cs.ConnectionState().PeerCertificates; len(certs) > 0 {
		return crypto.CertFingerprint(certs[0].Raw)
	}
	return ""
}

// relaySTUNServers 返回 relay 下发的 STUN 服务器副本。
func (c *Client) relaySTUNServers() []string {
	c.stunMu.Lock()
	defer c.stunMu.Unlock()
	return append([]string(nil), c.relaySTUN...)
}

// trustListed 设之后 TOFU 连接时按列表放行的证书指纹（见 listed）；nil 表示不按列表放行。
func (c *Client) trustListed(fps []string) {
	c.mu.Lock()
	c.listed = fps
	c.mu.Unlock()
}

// NewClient 创建客户端
func NewClient(cfg *Config) *Client {
	return &Client{
		config: cfg,
	}
}

// Connect 连接服务器（支持 Close 后重新连接）
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = false               // 允许 Close 后重新连接
	c.hbStop = make(chan struct{}) // 新一代连接：上一代的 hbStop 已在 Close 里关闭

	if c.node != nil {
		return c.connectNodeLocked()
	}

	var tlsConfig *tls.Config
	if c.config.UseTLS {
		var err error
		tlsConfig, err = crypto.NewClientTLSConfig(crypto.ClientTLSOptions{
			SkipVerify:   c.config.InsecureSkip,
			CAFile:       c.config.CAFile,
			PinAddr:      c.config.ServerAddr,
			TrustNewCert: c.config.TrustNewCert,
			TrustStore:   c.config.trustStore,
			OnPin:        logPinResult(c.config.ServerAddr),
			ListedFPs:    c.listed,
		})
		if err != nil {
			return fmt.Errorf("failed to create TLS config: %w", err)
		}
	}

	conn, transport, tcpErr, err := c.dialRelay(tlsConfig, c.transport)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	if c.config.Transport == "" || c.config.Transport == TransportAuto {
		logTransport(c.config.ServerAddr, transport, c.transport, tcpErr)
	}
	c.transport = transport
	c.installConnLocked(conn)
	return nil
}

// installConnLocked 把建好的连接装进 Client。调用方持有 c.mu。
func (c *Client) installConnLocked(conn net.Conn) {
	// Enable TCP KeepAlive to detect dead connections
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	} else if tlsConn, ok := conn.(*tls.Conn); ok {
		if tcpConn, ok := tlsConn.NetConn().(*net.TCPConn); ok {
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(30 * time.Second)
		}
	}

	c.conn = conn
	c.peerFP = connPeerFP(conn)
	c.enc = json.NewEncoder(conn)
	c.dec = json.NewDecoder(conn)
	c.pending = nil
}

// Close 关闭连接
func (c *Client) Close() {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	conn := c.conn
	hbStop := c.hbStop
	c.conn = nil
	c.peerFP = ""
	c.enc = nil
	c.dec = nil
	c.pending = nil
	c.hbStop = nil
	c.mu.Unlock()

	// 关掉这一代的心跳。置 nil 后本 goroutine 独占该 channel，锁外 close 是安全的；
	// closeOnce 语义由上面的 c.closed 短路提供。
	if hbStop != nil {
		close(hbStop)
	}

	if conn != nil {
		// Use recover to handle any panics from closing
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Recovered panic during connection close: %v", r)
			}
		}()
		conn.Close()
	}
}

// SendMessage 发送消息
func (c *Client) SendMessage(msgType proto.MessageType, payload interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.enc == nil || c.conn == nil {
		return fmt.Errorf("not connected")
	}
	if c.closed {
		return fmt.Errorf("connection closed")
	}

	msg, err := proto.NewMessage(msgType, payload)
	if err != nil {
		return err
	}
	// 写超时兜底：避免半死隧道下 Encode 无限阻塞、死占 c.mu 冻结读循环与心跳。
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	defer c.conn.SetWriteDeadline(time.Time{})
	return c.enc.Encode(msg)
}

// ReadMessage 读取消息
func (c *Client) ReadMessage() (*proto.Message, error) {
	c.mu.Lock()
	if c.dec == nil || c.conn == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("not connected")
	}
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("connection closed")
	}
	if len(c.pending) > 0 {
		msg := c.pending[0]
		c.pending = c.pending[1:]
		c.mu.Unlock()
		return msg, nil
	}
	dec := c.dec
	c.mu.Unlock()

	var msg proto.Message
	if err := dec.Decode(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// SendHeartbeat 发送心跳
func (c *Client) SendHeartbeat() error {
	return c.SendMessage(proto.MsgHeartbeat, &proto.Heartbeat{Timestamp: time.Now().Unix()})
}

// readReply 读 relay 对注册、加入请求的应答（what 是提示里应答的叫法），最多等 joinTimeout。relay 收下请求却
// 不应答（进程假死、被中间盒吞掉、发回的数据被网络丢掉）时，裸 ReadMessage 会永久阻塞。超时返回 *relaySilentError。
func (c *Client) readReply(what string) (*proto.Message, error) {
	wait := joinTimeout
	c.SetReadDeadline(time.Now().Add(wait))
	msg, err := c.ReadMessage()
	c.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, c.silentErr(err, what, wait)
	}
	return msg, nil
}

// probeDownlink 确认 relay 发来的数据真的收得到：发一个心跳，等 relay 发来任意一条消息（relay 对每个心跳都回一个，
// 最早的版本就是这样；这时已经注册或加入，旧 relay 也不会把它当成连接前的未知消息计分）。收到的不是心跳回应
// （比如 relay 保留了配对、协助端的消息先到了）就留给之后的 ReadMessage。超时返回 *relaySilentError。
func (c *Client) probeDownlink(timeout time.Duration) error {
	if err := c.SendHeartbeat(); err != nil {
		return err
	}
	c.SetReadDeadline(time.Now().Add(timeout))
	msg, err := c.ReadMessage()
	c.SetReadDeadline(time.Time{})
	if err != nil {
		return c.silentErr(err, "心跳回应", timeout)
	}
	if msg.Type != proto.MsgHeartbeat {
		c.mu.Lock()
		c.pending = append(c.pending, msg)
		c.mu.Unlock()
	}
	return nil
}

// silentErr 把读超时换成 *relaySilentError（what、wait 是等的东西和等了多久），别的错误原样返回。当前连接是 auto
// 模式下的 TCP 时，记下这个地址的 TCP 收不到数据（markTCPStalled），之后拨它先走 QUIC。
func (c *Client) silentErr(err error, what string, wait time.Duration) error {
	if !isNetTimeout(err) {
		return err
	}
	c.mu.Lock()
	e := &relaySilentError{what: what, wait: wait, transport: c.transport, err: err}
	if c.config != nil {
		e.addr = c.config.ServerAddr
		e.explicitTCP = c.config.Transport == TransportTCP
	}
	auto := c.autoTransportLocked()
	c.mu.Unlock()
	if e.transport == TransportTCP && auto && e.addr != "" {
		if markTCPStalled(e.addr) {
			log.Printf("relay %s 的 TCP 连接收不到它发回的数据，之后连它先走 QUIC（UDP）", e.addr)
		}
		e.fallback = true
	}
	return e
}

// autoTransportLocked 报告当前连接能不能改走 QUIC：没用 --transport 限定（auto），而且是 TLS（QUIC 强制 TLS）。
// 节点模式看接管时这个地址的拨号方式：地址只声明了一种传输时不是 auto。调用方持有 c.mu。
func (c *Client) autoTransportLocked() bool {
	if c.node != nil {
		return c.node.mode == TransportAuto
	}
	if c.config == nil || !c.config.UseTLS {
		return false
	}
	return c.config.Transport == "" || c.config.Transport == TransportAuto
}

// IsClosed 是否已关闭
func (c *Client) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// SetReadDeadline 设置读取截止时间
func (c *Client) SetReadDeadline(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.SetReadDeadline(t)
	}
}

// ResetDecoder 重建 JSON 解码器
// json.Decoder 在遇到超时等临时错误时会缓存错误，后续所有 Decode 调用
// 都会直接返回该缓存错误而不再尝试读取。P2P 协商超时后必须调用此方法。
func (c *Client) ResetDecoder() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.dec != nil {
		buffered := c.dec.Buffered()
		c.dec = json.NewDecoder(io.MultiReader(buffered, c.conn))
	}
}

// StartHeartbeatLoop 启动心跳循环，绑定调用时的那一代连接。
//
// 循环在该代连接 Close 时立即退出，因此 share 的 reconnectWithBackoff 每轮重连
// 起一个新循环不会叠加：旧的那个在 Close() 里就被 hbStop 叫停了，不会跟着新连接
// 继续发心跳（IsClosed() 兜不住这一点，见 Client.hbStop 注释）。
func (c *Client) StartHeartbeatLoop(interval time.Duration) {
	c.mu.Lock()
	stop := c.hbStop
	c.mu.Unlock()
	if stop == nil {
		return // 未 Connect 或已 Close：没有可绑定的连接代次
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if c.IsClosed() {
					return
				}
				_ = c.SendHeartbeat()
			}
		}
	}()
}

// tunnelCopy 隧道拷贝
func tunnelCopy(dst io.Writer, src io.Reader, done chan<- error) {
	_, err := io.Copy(dst, src)
	done <- err
}

// pipeConn 连接两个net.Conn
func pipeConn(conn1, conn2 net.Conn) {
	done := make(chan error, 2)
	go tunnelCopy(conn1, conn2, done)
	go tunnelCopy(conn2, conn1, done)
	<-done
	conn1.Close()
	conn2.Close()
}

// isNetTimeout 检查错误是否为网络超时（包了几层也认得出，比如 *relaySilentError）
func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
