package client

import (
	"crypto/tls"
	"encoding/json"
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
// tls.DialWithDialer 会把这个 timeout 同时用作握手 deadline，两段都盖住。
var dialTimeout = 20 * time.Second

// joinTimeout 等待 relay 回 JoinResponse / RegisterResponse 的超时。TCP 连上、TLS 也
// 握完，但 relay 迟迟不回应答的情况（进程假死、被中间盒吞掉）同样会让 ReadMessage
// 永久阻塞，光有 dialTimeout 盖不住。
//
// dialTimeout/joinTimeout 用 var 而非 const，仅为让测试能调小到毫秒级；运行期不改。
var joinTimeout = 15 * time.Second

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
	mu     sync.Mutex
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

	var conn net.Conn
	var err error

	dialer := &net.Dialer{Timeout: dialTimeout}
	if c.config.UseTLS {
		var tlsConfig *tls.Config
		tlsConfig, err = crypto.NewClientTLSConfig(crypto.ClientTLSOptions{
			SkipVerify:   c.config.InsecureSkip,
			CAFile:       c.config.CAFile,
			PinAddr:      c.config.ServerAddr,
			TrustNewCert: c.config.TrustNewCert,
			TrustStore:   c.config.trustStore,
			OnPin:        logPinResult(c.config.ServerAddr),
		})
		if err != nil {
			return fmt.Errorf("failed to create TLS config: %w", err)
		}
		conn, err = tls.DialWithDialer(dialer, "tcp", c.config.ServerAddr, tlsConfig)
	} else {
		conn, err = dialer.Dial("tcp", c.config.ServerAddr)
	}

	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

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
	c.enc = json.NewEncoder(conn)
	c.dec = json.NewDecoder(conn)
	return nil
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
	c.enc = nil
	c.dec = nil
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

// isNetTimeout 检查错误是否为网络超时
func isNetTimeout(err error) bool {
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout()
	}
	return false
}
