package client

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/proto"
)

// Config 客户端配置
type Config struct {
	ServerAddr   string
	InsecureSkip bool
	CAFile       string
	UseTLS       bool
	P2PMode      string // "disabled", "auto", "required"
	STUNServer   string // STUN server address for P2P
}

// Client 基础客户端
type Client struct {
	config *Config
	conn   net.Conn
	enc    *json.Encoder
	dec    *json.Decoder
	closed bool
	mu     sync.Mutex
}

// NewClient 创建客户端
func NewClient(cfg *Config) *Client {
	return &Client{
		config: cfg,
	}
}

// Connect 连接服务器
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var conn net.Conn
	var err error

	if c.config.UseTLS {
		var tlsConfig *tls.Config
		tlsConfig, err = crypto.NewTLSClientConfig(c.config.InsecureSkip, c.config.CAFile)
		if err != nil {
			return fmt.Errorf("failed to create TLS config: %w", err)
		}
		conn, err = tls.Dial("tcp", c.config.ServerAddr, tlsConfig)
	} else {
		conn, err = net.Dial("tcp", c.config.ServerAddr)
	}

	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
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
	c.conn = nil
	c.enc = nil
	c.dec = nil
	c.mu.Unlock()

	if conn != nil {
		// Use recover to handle any panics from closing
		defer func() {
			recover()
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

// StartHeartbeatLoop 启动心跳循环
func (c *Client) StartHeartbeatLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for range ticker.C {
			if c.IsClosed() {
				return
			}
			_ = c.SendHeartbeat()
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
