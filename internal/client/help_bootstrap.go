package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
)

// fileTransferChunk 单次 read_file/write_file 走的块大小。
// 512 KiB 给 base64 膨胀（×4/3 ≈ 683 KiB）和 JSON-RPC frame overhead 留余量，
// 整体单条消息可控在 ~720 KiB，远低于 MCP stdio 1 MiB 的常见软上限。
const fileTransferChunk = 512 * 1024

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

// CallTool 实现 mcp.ToolCaller。
//   - connect：本地处理（启动隧道）
//   - upload_file / download_file：host 端复合工具，循环调用 bridge 的
//     write_file / read_file，share 端零改动
//   - 其他：透传到 bridge
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
	switch name {
	case "upload_file":
		return b.doUploadFile(ctx, br, args)
	case "download_file":
		return b.doDownloadFile(ctx, br, args)
	}
	return br.CallTool(ctx, name, args)
}

type connectArgs struct {
	Code   string `json:"code"`
	Server string `json:"server,omitempty"` // 可选：覆盖 cfg.ServerAddr，用于 share --standalone LAN 直连
}

type connectResult struct {
	Connected bool   `json:"connected"`
	SessionID string `json:"session_id,omitempty"`
	Server    string `json:"server,omitempty"` // 实际连接的 relay 地址（debug 用）
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

	// 拷贝一份 cfg，避免修改影响后续 connect。若调用方传了 server 就覆盖 ServerAddr
	// （典型场景：share --standalone 跑在 LAN 上，由用户告知地址；help 这边无需重启）
	effectiveCfg := *b.cfg
	if a.Server != "" {
		effectiveCfg.ServerAddr = a.Server
	}
	h := NewHelpModeMCP(&effectiveCfg, a.Code)
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

	return json.Marshal(connectResult{Connected: true, SessionID: resp.SessionID, Server: effectiveCfg.ServerAddr})
}

type fileTransferArgs struct {
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
}

type fileTransferResult struct {
	Bytes  int64 `json:"bytes"`
	Chunks int   `json:"chunks"`
}

// upload_file: 本机 → 远端。复用 share 端 write_file 协议。
// 第一块用 Create=true / Append=false（truncate-create），后续块用 Append=true。
func (b *HelpMCPBootstrap) doUploadFile(ctx context.Context, br *mcp.Bridge, raw json.RawMessage) (json.RawMessage, error) {
	var a fileTransferArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if a.LocalPath == "" || a.RemotePath == "" {
		return nil, fmt.Errorf("local_path and remote_path required")
	}
	f, err := os.Open(a.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("open local %q: %w", a.LocalPath, err)
	}
	defer f.Close()

	// 中途失败不回滚，远端会留下半截文件（fail loud 留证据，方便 debug）。
	// 调用方拿到 error 后需要自行清理 RemotePath。
	var total int64
	var chunks int
	buf := make([]byte, fileTransferChunk)
	for {
		n, rerr := io.ReadFull(f, buf)
		// ReadFull 在最后一块（短读）返回 ErrUnexpectedEOF + n>0；EOF 表示 n==0。
		eof := errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
		if !eof && rerr != nil {
			return nil, fmt.Errorf("read local %q: %w", a.LocalPath, rerr)
		}
		if n == 0 && chunks == 0 {
			// 空文件：明确建一个空文件
			wargs, _ := json.Marshal(struct {
				Path    string `json:"path"`
				Content []byte `json:"content"`
				Create  bool   `json:"create"`
			}{Path: a.RemotePath, Content: []byte{}, Create: true})
			if _, err := br.CallTool(ctx, "write_file", wargs); err != nil {
				return nil, fmt.Errorf("write empty file to remote %q: %w", a.RemotePath, err)
			}
			chunks = 1
			break
		}
		if n == 0 {
			break
		}
		wargs, _ := json.Marshal(struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
			Create  bool   `json:"create"`
			Append  bool   `json:"append"`
		}{
			Path:    a.RemotePath,
			Content: buf[:n],
			Create:  chunks == 0,
			Append:  chunks > 0,
		})
		if _, err := br.CallTool(ctx, "write_file", wargs); err != nil {
			return nil, fmt.Errorf("write chunk %d to remote %q: %w", chunks, a.RemotePath, err)
		}
		chunks++
		total += int64(n)
		if eof {
			break
		}
	}
	return json.Marshal(fileTransferResult{Bytes: total, Chunks: chunks})
}

// download_file: 远端 → 本机。复用 share 端 read_file 协议，循环到 EOF。
func (b *HelpMCPBootstrap) doDownloadFile(ctx context.Context, br *mcp.Bridge, raw json.RawMessage) (json.RawMessage, error) {
	var a fileTransferArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if a.LocalPath == "" || a.RemotePath == "" {
		return nil, fmt.Errorf("local_path and remote_path required")
	}
	f, err := os.Create(a.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("create local %q: %w", a.LocalPath, err)
	}
	defer f.Close()

	// 中途失败不回滚，本地会留下半截文件（fail loud 留证据，方便 debug）。
	// 调用方拿到 error 后需要自行清理 LocalPath。
	var total int64
	var chunks int
	for {
		rargs, _ := json.Marshal(struct {
			Path   string `json:"path"`
			Offset int64  `json:"offset"`
			Length int64  `json:"length"`
		}{
			Path:   a.RemotePath,
			Offset: total,
			Length: fileTransferChunk,
		})
		resraw, err := br.CallTool(ctx, "read_file", rargs)
		if err != nil {
			return nil, fmt.Errorf("read chunk %d from remote %q: %w", chunks, a.RemotePath, err)
		}
		var rres struct {
			Bytes []byte `json:"bytes"`
			EOF   bool   `json:"eof"`
		}
		if err := json.Unmarshal(resraw, &rres); err != nil {
			return nil, fmt.Errorf("decode chunk %d from remote %q: %w", chunks, a.RemotePath, err)
		}
		if len(rres.Bytes) > 0 {
			if _, err := f.Write(rres.Bytes); err != nil {
				return nil, fmt.Errorf("write chunk %d to local %q: %w", chunks, a.LocalPath, err)
			}
			total += int64(len(rres.Bytes))
		}
		chunks++
		if rres.EOF {
			break
		}
		if len(rres.Bytes) == 0 {
			// 防御：share 端不返 EOF 也不返 bytes 时退出，避免死循环
			return nil, fmt.Errorf("empty chunk without EOF from remote %q at offset %d", a.RemotePath, total)
		}
	}
	return json.Marshal(fileTransferResult{Bytes: total, Chunks: chunks})
}
