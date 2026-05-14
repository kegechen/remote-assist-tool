package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
)

// HelpMCPBootstrap 是 --mcp-stdio 不带 --code 时的入口。
// MCP server 立刻启动，但 9 个真实工具未连接前会返回 not_connected。
// Claude 调用 connect(code) 后，内部完成 client.Connect + Join + 握手，
// 装载 bridge，之后所有调用透传到真实工具。
type HelpMCPBootstrap struct {
	cfg *Config

	mu     sync.Mutex
	help   *HelpMode   // 装载成功后非 nil
	bridge *mcp.Bridge // 装载成功后非 nil
}

func NewHelpMCPBootstrap(cfg *Config) *HelpMCPBootstrap {
	return &HelpMCPBootstrap{cfg: cfg}
}

// Run 阻塞跑 MCP stdio server，直到 stdin EOF
func (b *HelpMCPBootstrap) Run(ctx context.Context) error {
	fmt.Fprintln(os.Stderr, "MCP stdio 模式（bootstrap）：等待 Claude 通过 connect 工具提供协助码")
	srv := mcp.NewServer(b)
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}

// CallTool 实现 mcp.ToolCaller。connect 内部处理；其他工具委托到 bridge（如已连接）。
func (b *HelpMCPBootstrap) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if name == "connect" {
		return b.doConnect(ctx, args)
	}
	b.mu.Lock()
	br := b.bridge
	b.mu.Unlock()
	if br == nil {
		return nil, fmt.Errorf("not_connected: call 'connect' with the assist code first")
	}
	return br.CallTool(ctx, name, args)
}

type connectArgs struct {
	Code string `json:"code"`
}

type connectResult struct {
	Connected bool   `json:"connected"`
	SessionID string `json:"session_id,omitempty"`
}

func (b *HelpMCPBootstrap) doConnect(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a connectArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if a.Code == "" {
		return nil, fmt.Errorf("code required")
	}

	b.mu.Lock()
	// 关闭老连接（reconnect 支持）
	if b.help != nil {
		b.help.client.Close()
		b.help = nil
		b.bridge = nil
	}
	b.mu.Unlock()

	h := NewHelpModeMCP(b.cfg, a.Code)
	if err := h.client.Connect(); err != nil {
		return nil, fmt.Errorf("relay connect failed: %w", err)
	}
	resp, err := h.join()
	if err != nil {
		h.client.Close()
		return nil, fmt.Errorf("join failed: %w", err)
	}
	key, err := h.handshakeTool()
	if err != nil {
		h.client.Close()
		return nil, fmt.Errorf("handshake failed: %w", err)
	}
	bridge := mcp.NewBridge(h.client, key)

	// 心跳保活：每 30s 发 Heartbeat，relay 回 echo，避免 ReadMessage 2-min deadline
	// 因为空闲被触发，导致后台 goroutine 退出 → MCP 工具调用全部失效。
	h.client.StartHeartbeatLoop(30 * time.Second)
	// 后台 ReadMessage 循环，把工具消息投给 bridge
	go func() {
		for {
			h.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
			msg, err := h.client.ReadMessage()
			if err != nil {
				return
			}
			dispatchHelpToolMessage(msg, bridge)
		}
	}()

	b.mu.Lock()
	b.help = h
	b.bridge = bridge
	b.mu.Unlock()

	return json.Marshal(connectResult{Connected: true, SessionID: resp.SessionID})
}
