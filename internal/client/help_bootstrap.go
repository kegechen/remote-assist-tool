package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
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
	P2P       bool   `json:"p2p,omitempty"`    // true when tool channel uses P2P direct connection
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
	// --- P2P negotiation (phase 1: start BEFORE tool handshake) ---
	// Both sides negotiate P2P simultaneously:
	//   - Share starts P2P right after SessionReady (in waitAndHandleTunnel)
	//   - Help starts P2P right after Join (here), BEFORE handshakeTool
	// This ensures the timing windows overlap so PeerAddrReady messages
	// arrive while both sides are listening.
	var p2pMgr *p2p.P2PManager
	var p2pResultCh <-chan p2p.P2PResult

	p2pMode := p2p.ParseP2PMode(effectiveCfg.P2PMode)
	if p2pMode != p2p.P2PModeDisabled {
		p2pMgr = p2p.NewP2PManager(p2pMode, effectiveCfg.STUNServer, effectiveCfg.BindIP)
		p2pMgr.SetRelayConn(h.client)
		var startErr error
		p2pResultCh, startErr = p2pMgr.Start(resp.SessionID, false)
		if startErr != nil {
			log.Printf("MCP P2P manager start failed: %v", startErr)
			p2pMgr = nil
		} else {
			fmt.Fprintln(os.Stderr, "MCP: P2P 协商已启动（与工具握手并行）")
		}
	}

	// Tool handshake over relay. During this phase, PeerAddrReady may arrive
	// from the relay — handshakeToolWithP2P feeds it to the P2P manager
	// instead of discarding it.
	key, err := h.handshakeToolWithP2P(p2pMgr)
	if err != nil {
		if p2pMgr != nil {
			p2pMgr.Close()
		}
		h.client.Close()
		return nil, fmt.Errorf("handshake failed: %w", err)
	}

	// --- P2P negotiation (phase 2: collect result after handshake) ---
	var bridge *mcp.Bridge
	var p2pConn *P2PConn // non-nil when P2P active

	if p2pMgr != nil {
		tunnel, p2pErr := h.collectP2PResult(p2pMgr, p2pMode, p2pResultCh)
		if p2pErr != nil {
			if p2pMode == p2p.P2PModeRequired {
				h.client.Close()
				return nil, fmt.Errorf("P2P required but failed: %w", p2pErr)
			}
			log.Printf("MCP P2P negotiation failed, falling back to relay: %v", p2pErr)
		}
		if tunnel != nil {
			pc := NewP2PConn(tunnel)
			// Signal tool-over-P2P mode to share, then redo tool handshake over P2P
			if err := pc.WriteModeHeader(); err != nil {
				log.Printf("MCP P2P mode header send failed, falling back to relay: %v", err)
				tunnel.Close()
			} else if p2pKey, err := h.handshakeToolOverP2P(pc); err != nil {
				log.Printf("MCP P2P tool handshake failed, falling back to relay: %v", err)
				tunnel.Close()
			} else {
				p2pConn = pc
				key = p2pKey // use the P2P-negotiated key
				bridge = mcp.NewBridge(p2pConn, key)
				log.Printf("MCP tool channel switched to P2P direct connection")
			}
		}
	}

	// Fallback: relay mode (original behavior)
	if bridge == nil {
		bridge = mcp.NewBridge(h.client, key)
	}

	// 心跳保活：每 30s 发 Heartbeat，relay 回 echo，避免 ReadMessage 2-min deadline
	// 因为空闲被触发，导致后台 goroutine 退出 → MCP 工具调用全部失效。
	// Heartbeat always goes over relay (the signaling channel stays alive for
	// reconnect / session management even when tools use P2P).
	h.client.StartHeartbeatLoop(30 * time.Second)

	// 后台 ReadMessage 循环，把工具消息投给 bridge。
	// When P2P is active, tool messages arrive via p2pConn; the relay read
	// loop still runs to handle relay-level messages (heartbeat echo, errors,
	// session teardown) and to detect relay death.
	if p2pConn != nil {
		// P2P read loop: tool messages (ToolResp/ToolStream) over UDPTunnel
		go func() {
			for {
				msg, err := p2pConn.ReadMessage()
				if err != nil {
					bridge.Disconnect(fmt.Errorf("tunnel_lost: P2P 隧道已断开（%w），请重新 connect", err))
					b.mu.Lock()
					if b.bridge == bridge {
						b.help = nil
						b.bridge = nil
					}
					b.mu.Unlock()
					return
				}
				dispatchHelpToolMessage(msg, bridge)
			}
		}()
		// Relay read loop (drain-only): keep relay alive, detect session teardown.
		// P2P is active — tool messages are exclusively on the P2P tunnel.
		// This loop only drains relay to prevent buffer buildup; it does NOT
		// dispatch tool messages to avoid duplicate delivery.
		go func() {
			for {
				h.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
				msg, err := h.client.ReadMessage()
				if err != nil {
					log.Printf("MCP relay read loop ended (P2P active): %v", err)
					return
				}
				switch msg.Type {
				case proto.MsgError:
					var errMsg proto.ErrorMessage
					proto.DecodePayload(msg, &errMsg)
					log.Printf("MCP relay error (P2P active): %s", errMsg.Message)
				default:
					// drain: heartbeat echo, stale tool responses — all discarded
				}
			}
		}()
	} else {
		// Relay-only read loop (original behavior)
		go func() {
			for {
				h.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
				msg, err := h.client.ReadMessage()
				if err != nil {
					bridge.Disconnect(fmt.Errorf("tunnel_lost: 隧道已断开（%w），请重新 connect", err))
					b.mu.Lock()
					if b.bridge == bridge {
						b.help = nil
						b.bridge = nil
					}
					b.mu.Unlock()
					return
				}
				dispatchHelpToolMessage(msg, bridge)
			}
		}()
	}

	b.mu.Lock()
	b.help = h
	b.bridge = bridge
	b.mu.Unlock()

	result := connectResult{Connected: true, SessionID: resp.SessionID, Server: effectiveCfg.ServerAddr}
	if p2pConn != nil {
		result.P2P = true
	}
	return json.Marshal(result)
}

type fileTransferArgs struct {
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
	// Offset 断点续传起点（字节）：upload seek 本地 + 远端 Append；
	// download seek 本地 + 远端 read_file offset。默认 0 = 从头。
	Offset int64 `json:"offset,omitempty"`
}

type fileTransferResult struct {
	Bytes  int64 `json:"bytes"`
	Chunks int   `json:"chunks"`
}

// chunkRetries 单 chunk 传输失败的重试次数（抗瞬时抖动）。隧道整体断开
// （tunnel_lost）时重试无用，会快速耗尽后返回，由调用方 reconnect + offset 续传。
const chunkRetries = 3

// toolCaller 抽象 bridge.CallTool，便于 upload/download 续传逻辑单测（*mcp.Bridge 满足之）。
type toolCaller interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

// callToolRetry 对单 chunk 的 CallTool 做有限次递增退避（1s/2s/3s）重试。
func callToolRetry(ctx context.Context, br toolCaller, name string, args json.RawMessage) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt <= chunkRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		res, err := br.CallTool(ctx, name, args)
		if err == nil {
			return res, nil
		}
		lastErr = err
		// 隧道整体断开 / 未连接属不可恢复错误，重试无用——立即返回，
		// 让调用方 reconnect 后用 offset 续传，省掉 ~6s 空转。
		if e := err.Error(); strings.Contains(e, "tunnel_lost") || strings.Contains(e, "not_connected") {
			return nil, err
		}
	}
	return nil, lastErr
}

// upload_file: 本机 → 远端。复用 share 端 write_file 协议。
// 全新上传：第一块 Create=true（truncate），后续块 Append=true。
// 断点续传（offset>0）：seek 本地到 offset，全程 Append（不 truncate）。
// 每个 chunk 走 callToolRetry 抗瞬时抖动；彻底失败时 error 带已传偏移，
// 调用方 reconnect 后用 offset=<已传> 续传。
func (b *HelpMCPBootstrap) doUploadFile(ctx context.Context, br toolCaller, raw json.RawMessage) (json.RawMessage, error) {
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

	if a.Offset > 0 {
		if _, err := f.Seek(a.Offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek local %q to offset %d: %w", a.LocalPath, a.Offset, err)
		}
	}

	// 中途失败不回滚，远端留半截文件；调用方可 reconnect 后 offset 续传（接 Append）。
	total := a.Offset
	var chunks int
	buf := make([]byte, fileTransferChunk)
	for {
		n, rerr := io.ReadFull(f, buf)
		// ReadFull 在最后一块（短读）返回 ErrUnexpectedEOF + n>0；EOF 表示 n==0。
		eof := errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
		if !eof && rerr != nil {
			return nil, fmt.Errorf("read local %q: %w", a.LocalPath, rerr)
		}
		if n == 0 && chunks == 0 && a.Offset == 0 {
			// 空文件：明确建一个空文件
			wargs, _ := json.Marshal(struct {
				Path    string `json:"path"`
				Content []byte `json:"content"`
				Create  bool   `json:"create"`
			}{Path: a.RemotePath, Content: []byte{}, Create: true})
			if _, err := callToolRetry(ctx, br, "write_file", wargs); err != nil {
				return nil, fmt.Errorf("write empty file to remote %q: %w", a.RemotePath, err)
			}
			chunks = 1
			break
		}
		if n == 0 {
			break
		}
		// 首块且非续传时 Create（truncate）；续传或后续块一律 Append。
		fresh := chunks == 0 && a.Offset == 0
		wargs, _ := json.Marshal(struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
			Create  bool   `json:"create"`
			Append  bool   `json:"append"`
		}{
			Path:    a.RemotePath,
			Content: buf[:n],
			Create:  fresh,
			Append:  !fresh,
		})
		if _, err := callToolRetry(ctx, br, "write_file", wargs); err != nil {
			return nil, fmt.Errorf("upload to remote %q failed at offset %d (reconnect and re-call upload_file with offset=%d to resume): %w", a.RemotePath, total, total, err)
		}
		chunks++
		total += int64(n)
		if eof {
			break
		}
	}
	return json.Marshal(fileTransferResult{Bytes: total, Chunks: chunks})
}

// download_file: 远端 → 本机。复用 share 端 read_file（已支持 offset）协议，循环到 EOF。
// 断点续传（offset>0）：打开已有本地文件 seek 到 offset，从远端 offset 续读，不 truncate。
// 每个 chunk 走 callToolRetry；彻底失败时 error 带已传偏移供 reconnect 后续传。
func (b *HelpMCPBootstrap) doDownloadFile(ctx context.Context, br toolCaller, raw json.RawMessage) (json.RawMessage, error) {
	var a fileTransferArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if a.LocalPath == "" || a.RemotePath == "" {
		return nil, fmt.Errorf("local_path and remote_path required")
	}
	var f *os.File
	var err error
	if a.Offset > 0 {
		// 续传：打开已有文件并 seek，不 truncate
		f, err = os.OpenFile(a.LocalPath, os.O_WRONLY, 0644)
		if err == nil {
			_, err = f.Seek(a.Offset, io.SeekStart)
		}
	} else {
		f, err = os.Create(a.LocalPath)
	}
	if err != nil {
		return nil, fmt.Errorf("open local %q: %w", a.LocalPath, err)
	}
	defer f.Close()

	// 中途失败不回滚，本地留半截文件；调用方可 reconnect 后 offset 续传。
	total := a.Offset
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
		resraw, err := callToolRetry(ctx, br, "read_file", rargs)
		if err != nil {
			return nil, fmt.Errorf("download from remote %q failed at offset %d (reconnect and re-call download_file with offset=%d to resume): %w", a.RemotePath, total, total, err)
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

// handshakeToolWithP2P performs tool handshake over relay, while also feeding
// PeerAddrReady messages to the P2P manager (if active). This is a variant of
// handshakeTool that doesn't discard P2P signaling messages.
func (h *HelpMode) handshakeToolWithP2P(mgr *p2p.P2PManager) ([32]byte, error) {
	hello := proto.NewHello()
	if err := h.client.SendMessage(proto.MsgToolHello, &hello); err != nil {
		return [32]byte{}, err
	}
	h.client.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer h.client.SetReadDeadline(time.Time{})
	for {
		msg, err := h.client.ReadMessage()
		if err != nil {
			return [32]byte{}, err
		}
		switch msg.Type {
		case proto.MsgToolHelloAck:
			var ack proto.HelloAck
			proto.DecodePayload(msg, &ack)
			if !ack.Accept {
				return [32]byte{}, fmt.Errorf("share rejected tool channel: %s", ack.ErrorMsg)
			}
			return proto.DeriveSessionKey(h.code, ack.NonceB64, hello.NonceB64), nil
		case proto.MsgPeerAddrReady:
			// Feed to P2P manager instead of discarding
			if mgr != nil {
				var ready proto.PeerAddrReady
				if err := proto.DecodePayload(msg, &ready); err == nil {
					mgr.HandlePeerAddrReady(&ready)
				}
			}
		case proto.MsgHeartbeat, proto.MsgError:
			continue
		default:
			continue
		}
	}
}

// collectP2PResult waits for the P2P negotiation result with a bounded timeout.
// If the P2P manager already produced a result (e.g. LAN fast path or hole punch
// completed during handshake), returns immediately. Otherwise waits up to the
// remaining negotiation budget.
func (h *HelpMode) collectP2PResult(mgr *p2p.P2PManager, mode p2p.P2PMode, resultCh <-chan p2p.P2PResult) (*p2p.UDPTunnel, error) {
	// Check for immediate result
	select {
	case result := <-resultCh:
		if result.Tunnel != nil {
			fmt.Fprintln(os.Stderr, "MCP: P2P 直连已建立！")
		}
		return result.Tunnel, result.Err
	default:
	}

	// Wait with timeout for hole punching to complete
	timeout := 8 * time.Second
	if mode == p2p.P2PModeRequired {
		timeout = 20 * time.Second
	}

	fmt.Fprintln(os.Stderr, "MCP: 等待 P2P 打洞完成...")
	select {
	case result := <-resultCh:
		if result.Tunnel != nil {
			fmt.Fprintln(os.Stderr, "MCP: P2P 直连已建立！")
		} else if result.Err == nil {
			fmt.Fprintln(os.Stderr, "MCP: P2P 打洞超时，回退到中转模式")
			mgr.Close()
		} else {
			mgr.Close()
		}
		return result.Tunnel, result.Err
	case <-time.After(timeout):
		mgr.Close()
		if mode == p2p.P2PModeRequired {
			return nil, fmt.Errorf("P2P 协商超时：打洞未完成")
		}
		fmt.Fprintln(os.Stderr, "MCP: P2P 打洞超时，回退到中转模式")
		return nil, nil
	}
}

// handshakeToolOverP2P performs tool handshake over the P2P tunnel (after the
// relay handshake and P2P connection succeeded). This establishes a fresh
// session key for P2P-encrypted tool messages, independent of the relay key.
func (h *HelpMode) handshakeToolOverP2P(pc *P2PConn) ([32]byte, error) {
	hello := proto.NewHello()
	if err := pc.SendMessage(proto.MsgToolHello, &hello); err != nil {
		return [32]byte{}, fmt.Errorf("p2p tool hello: %w", err)
	}
	// Read ToolHelloAck with timeout (share should respond quickly over P2P)
	type readResult struct {
		msg *proto.Message
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		msg, err := pc.ReadMessage()
		ch <- readResult{msg, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return [32]byte{}, fmt.Errorf("p2p tool handshake read: %w", r.err)
		}
		if r.msg.Type != proto.MsgToolHelloAck {
			return [32]byte{}, fmt.Errorf("p2p tool handshake: expected HelloAck, got %s", r.msg.Type)
		}
		var ack proto.HelloAck
		proto.DecodePayload(r.msg, &ack)
		if !ack.Accept {
			return [32]byte{}, fmt.Errorf("share rejected P2P tool channel: %s", ack.ErrorMsg)
		}
		return proto.DeriveSessionKey(h.code, ack.NonceB64, hello.NonceB64), nil
	case <-time.After(10 * time.Second):
		return [32]byte{}, fmt.Errorf("p2p tool handshake timeout")
	}
}
