package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/version"
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

	connectMu    sync.Mutex // 串行化 connect，避免并发调用互相拆连接
	mu           sync.Mutex
	help         *HelpMode   // 装载成功后非 nil
	bridge       *mcp.Bridge // 装载成功后非 nil
	activeTarget connectTarget
	activeResult connectResult
	// 记录最近一次成功 connect 的参数，供传输中隧道断时自动重连+续传（抗抖动）
	lastCode   string
	lastServer string
	lastNoAuth bool
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
	case "upload_file", "download_file":
		return b.transferWithReconnect(ctx, name, args)
	}
	return br.CallTool(ctx, name, args)
}

// CallToolStream 实现 mcp.StreamToolCaller：把 share 端的流式输出块透传给 onChunk。
// connect 与 upload_file / download_file 是 host 端本地逻辑、没有流可推（它们的进度走
// stderr），退回普通调用。
func (b *HelpMCPBootstrap) CallToolStream(ctx context.Context, name string, args json.RawMessage, onChunk func(stream string, data []byte)) (json.RawMessage, error) {
	switch name {
	case "connect", "upload_file", "download_file":
		return b.CallTool(ctx, name, args)
	}
	b.mu.Lock()
	br := b.bridge
	b.mu.Unlock()
	if br == nil {
		return nil, fmt.Errorf("not_connected: call 'connect' with the assist code first")
	}
	return br.CallToolStream(ctx, name, args, onChunk)
}

// maxTransferReconnects 传输中隧道断（tunnel_lost）时自动重连+续传的最大次数。
const maxTransferReconnects = 5

// transferBackoffUnit 自动重连前的递增退避基数（第 n 次等 n×unit）。变量以便单测调小。
var transferBackoffUnit = time.Second

// isTunnelDown 判定错误是否为「隧道整体断开 / 未连接」——这类错误单纯重试无用，
// 需重连后续传（区别于瞬时错，后者由 callToolRetry 就地退避重试）。
func isTunnelDown(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "tunnel_lost") || strings.Contains(s, "not_connected")
}

// reconnect 用最近一次成功 connect 的参数重建隧道 + bridge（供传输中自动重连）。
func (b *HelpMCPBootstrap) reconnect(ctx context.Context) error {
	b.mu.Lock()
	a := connectArgs{Code: b.lastCode, Server: b.lastServer, NoAuth: b.lastNoAuth}
	b.mu.Unlock()
	if a.Code == "" && !a.NoAuth {
		return fmt.Errorf("no cached assist code to auto-reconnect")
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = b.doConnect(ctx, raw)
	return err
}

// transferWithReconnect 包裹 upload/download：隧道断时自动重连并从断点续传，不再要求
// 外部人工重连重调（抗网络抖动）。上传走顺序模式（uploadConcurrency=1）保证无空洞、续传安全。
// caveat：只在协助码仍有效时管用；码过期/轮换救不了（那是另一层问题）。
func (b *HelpMCPBootstrap) transferWithReconnect(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	run := func(a json.RawMessage) (json.RawMessage, error) {
		b.mu.Lock()
		br := b.bridge
		b.mu.Unlock()
		if br == nil {
			return nil, fmt.Errorf("not_connected: call 'connect' with the assist code first")
		}
		if name == "upload_file" {
			return b.doUploadFile(ctx, br, a)
		}
		return b.doDownloadFile(ctx, br, a)
	}
	return transferLoop(ctx, name, args, run, func() error { return b.reconnect(ctx) })
}

// transferLoop 对 run 做自动重连+续传：run 返回 tunnel_lost/not_connected 类错误时，
// 递增退避后调 reconnect，再以续传参数重跑，直至成功 / 非隧道错 / 次数耗尽 / ctx 取消。
// 抽成自由函数便于对 run/reconnect 注入 mock 做单测。
func transferLoop(ctx context.Context, name string, args json.RawMessage,
	run func(json.RawMessage) (json.RawMessage, error), reconnect func() error) (json.RawMessage, error) {
	cur := args
	for attempt := 0; ; attempt++ {
		res, err := run(cur)
		if err == nil {
			return res, nil
		}
		if attempt >= maxTransferReconnects || !isTunnelDown(err) || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-time.After(time.Duration(attempt+1) * transferBackoffUnit):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if rerr := reconnect(); rerr != nil {
			return nil, fmt.Errorf("传输中隧道断开，自动重连失败: %w（原传输错误: %v）", rerr, err)
		}
		next, aerr := resumeTransferArgs(name, cur)
		if aerr != nil {
			return nil, aerr
		}
		cur = next
		fmt.Fprintf(os.Stderr, "[%s] 隧道断开→已自动重连，第 %d 次续传...\n", name, attempt+1)
	}
}

// resumeTransferArgs 生成续传参数：download 以本地已存大小为续传点；upload 置 Offset>0
// 触发 doUploadFile 的「按远端 stat 真实大小续传」（顺序上传 → 远端大小即连续前缀，安全）。
func resumeTransferArgs(name string, raw json.RawMessage) (json.RawMessage, error) {
	var a fileTransferArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if name == "download_file" {
		var size int64
		if fi, err := os.Stat(a.LocalPath); err == nil {
			size = fi.Size()
		}
		a.Offset = size
	} else {
		a.Offset = 1 // 置正即可，doUploadFile 会以远端 stat 真实大小为准
	}
	return json.Marshal(a)
}

type connectArgs struct {
	Code   string `json:"code"`
	Server string `json:"server,omitempty"`  // 可选：覆盖 cfg.ServerAddr，用于 share --standalone LAN 直连
	NoAuth bool   `json:"no_auth,omitempty"` // true: 使用固定 NoAuthCode，无需用户提供 code
}

type connectResult struct {
	Connected   bool   `json:"connected"`
	SessionID   string `json:"session_id,omitempty"`
	Server      string `json:"server,omitempty"`       // 实际连接的 relay 地址（debug 用）
	P2P         bool   `json:"p2p,omitempty"`          // true when tool channel uses P2P direct connection
	PeerVersion string `json:"peer_version,omitempty"` // share 版本，来自 relay join 响应
	PeerHost    string `json:"peer_host,omitempty"`
	HelpVersion string `json:"help_version,omitempty"` // 当前 help CLI 版本，供 GUI 做升级提示
}

type connectTarget struct {
	Code   string
	Server string
	NoAuth bool
}

func (b *HelpMCPBootstrap) doConnect(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	b.connectMu.Lock()
	defer b.connectMu.Unlock()

	var a connectArgs
	// 拒绝未知字段：静默忽略会让写错名字的参数悄悄失效，connect 转而连上内置默认
	// relay，报出的超时错误完全指不到根因（调用方臆造 relay_url 时就这么栽过）。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("bad args: %w (connect accepts only: code, server, no_auth; for the relay address use server=\"host:port\", no http:// prefix)", err)
	}
	// schema 里明说了 "http://host:port is rejected"——那就真得拒绝。不挡的话
	// NormalizeServerAddr 会把它揉成 "[http://1.2.3.4:8443]:8443" 再去拨号，报错指不到
	// 根因，正好重蹈上面 DisallowUnknownFields 想根治的覆辙（承诺与实现不符比没承诺更坏）。
	if a.Server != "" {
		if err := ValidateServerAddr(a.Server); err != nil {
			return nil, fmt.Errorf("bad args: %w", err)
		}
	}
	if a.NoAuth {
		if a.Code == "" {
			a.Code = proto.NoAuthCode
		}
		fmt.Fprintln(os.Stderr, "WARNING: NO-AUTH mode — connecting without authentication. Only safe on trusted private LANs.")
	}
	if a.Code == "" {
		return nil, fmt.Errorf("code required (or set no_auth=true for trusted LANs)")
	}

	// 先算出实际连接目标。相同目标的重复 connect 是常见操作（新一轮 agent 可能不
	// 知道 MCP 子进程仍保持着连接），必须幂等复用，不能拆掉健康的 relay/P2P 会话。
	effectiveCfg := *b.cfg
	if a.Server != "" {
		effectiveCfg.ServerAddr = a.Server
	}
	effectiveCfg.ServerAddr = NormalizeServerAddr(effectiveCfg.ServerAddr)
	target := connectTarget{
		Code:   normalizeCode(a.Code),
		Server: effectiveCfg.ServerAddr,
		NoAuth: a.NoAuth,
	}

	b.mu.Lock()
	if b.help != nil && b.bridge != nil && b.activeTarget == target {
		result := b.activeResult
		b.mu.Unlock()
		return json.Marshal(result)
	}
	// 关闭老连接（reconnect 支持）
	if b.help != nil {
		b.help.client.Close()
		b.help = nil
		b.bridge = nil
	}
	b.activeTarget = connectTarget{}
	b.activeResult = connectResult{}
	b.mu.Unlock()

	h := NewHelpModeMCP(&effectiveCfg, a.Code)
	if err := h.client.Connect(); err != nil {
		return nil, fmt.Errorf("relay connect failed: %w", err)
	}
	resp, err := h.join()
	if err != nil {
		h.client.Close()
		return nil, fmt.Errorf("join failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "MCP: 已连接中转服务 %s\n", relayDesc(&effectiveCfg))
	if resp.PeerHost != "" {
		fmt.Fprintf(os.Stderr, "MCP: 对端标识 %s\n", resp.PeerHost)
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
	// 私网/loopback relay（standalone / 同 LAN）下 relay 已直连、P2P 多余，且 standalone
	// 不启 STUN 必然打洞超时；auto 模式自动跳过，省掉 8s 超时 + 后台徒劳重试。
	if p2pMode == p2p.P2PModeAuto && IsLANServer(effectiveCfg.ServerAddr) {
		fmt.Fprintf(os.Stderr, "MCP: server %s 为私网/loopback，relay 已 LAN 直连，跳过 P2P（如需强制用 --p2p required）\n", effectiveCfg.ServerAddr)
		p2pMode = p2p.P2PModeDisabled
	}
	if p2pMode != p2p.P2PModeDisabled {
		p2pMgr = p2p.NewP2PManager(p2pMode, effectiveCfg.STUNServer, effectiveCfg.BindIP)
		p2pMgr.SetRelayConn(h.client)
		var startErr error
		p2pResultCh, startErr = p2pMgr.Start(resp.SessionID, false)
		if startErr != nil {
			log.Printf("MCP P2P manager start failed: %v", startErr)
			p2pMgr = nil
		}
	}

	var bridge *mcp.Bridge
	var p2pConn *P2PConn // non-nil when P2P active
	var key [32]byte

	// --- P2P 协商优先（不发 ToolHello）---
	// 先纯协商拿 p2p 结果：成功则在 p2p 隧道上做工具握手，失败再回退 relay 工具握手。
	// 两端都「先 P2P 协商、结果决定握手通道」，消除 help 先 relay 握手与 share 端
	// ReadModeHeader 互等的死锁（详见 negotiateP2POnly 注释）。
	if p2pMgr != nil {
		tunnel, p2pErr := h.negotiateP2POnly(p2pMgr, p2pMode, p2pResultCh)
		if p2pErr != nil && p2pMode == p2p.P2PModeRequired {
			h.client.Close()
			return nil, fmt.Errorf("P2P required but failed: %w", p2pErr)
		}
		if tunnel != nil {
			pc := NewP2PConn(tunnel)
			// 先发 mode header 告诉 share 走工具模式，再在 p2p 上做工具握手
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

	// 回退：relay 工具握手（p2p 失败 / disabled）。此时 share 端也已回退到 handleTunnel
	// 读 relay，会应答这里发的 ToolHello。p2p 协商可能设过 read deadline 致 decoder 缓存
	// 错误，回退前先重建 decoder。
	//
	// 已知限制：若 p2p tunnel 已建立、但其上的工具握手（WriteModeHeader /
	// handshakeToolOverP2P）失败才回退到这里，share 端已离开 handleTunnel 进入
	// waitSessionReady（那里对 ToolHello 只 Inject 不 buildHelloAck），此处 relay 握手
	// 会 timeout。属罕见边缘（可靠 p2p 流上工具握手极少失败）；根治需 share 端在 p2p
	// 工具握手失败后回退 relay 模式应答，留作后续。
	if bridge == nil {
		h.client.ResetDecoder()
		k, hsErr := h.handshakeToolWithP2P(nil) // mgr=nil：纯 relay 工具握手，不再喂 PeerAddrReady
		if hsErr != nil {
			h.client.Close()
			return nil, fmt.Errorf("handshake failed: %w", hsErr)
		}
		key = k
		bridge = mcp.NewBridge(h.client, key)
	}

	// 心跳保活：每 30s 发 Heartbeat，relay 回 echo，避免 ReadMessage 2-min deadline
	// 因为空闲被触发，导致后台 goroutine 退出 → MCP 工具调用全部失效。
	// Heartbeat always goes over relay (the signaling channel stays alive for
	// reconnect / session management even when tools use P2P).
	h.client.StartHeartbeatLoop(30 * time.Second)

	// teardown 拆除当前会话，sync.Once 保证只跑一次：关闭 relay 控制连接
	// （→ 心跳循环经 IsClosed() 退出，并让 relay 端读循环 EOF → DisconnectClient
	// 立即清 Help 槽）、唤醒在途工具调用、清 bootstrap 状态。任一读循环（P2P / relay）
	// 检测到断开都调它。修复点：旧实现 P2P 隧道断开后只 bridge.Disconnect + 清 b.help，
	// 不关 h.client，心跳不停 → relay 端 Help 槽永驻 → 新 connect 永久 already-has-helper。
	var teardownOnce sync.Once
	teardown := func(err error) {
		teardownOnce.Do(func() {
			bridge.Disconnect(err)
			h.client.Close()
			if p2pConn != nil {
				p2pConn.Close()
			}
			b.mu.Lock()
			if b.bridge == bridge {
				b.help = nil
				b.bridge = nil
				b.activeTarget = connectTarget{}
				b.activeResult = connectResult{}
			}
			b.mu.Unlock()
		})
	}
	result := connectResult{
		Connected:   true,
		SessionID:   resp.SessionID,
		Server:      effectiveCfg.ServerAddr,
		PeerVersion: resp.PeerVersion,
		PeerHost:    resp.PeerHost,
		HelpVersion: version.Info(),
	}
	if p2pConn != nil {
		result.P2P = true
	}

	// 必须在启动读循环前发布活动状态。否则连接若立即断开，teardown 可能先运行却
	// 因 b.bridge 尚未登记而清理不到，随后这里反而缓存一个已经死亡的会话。
	b.mu.Lock()
	b.help = h
	b.bridge = bridge
	b.activeTarget = target
	b.activeResult = result
	// 缓存本次 connect 参数，供传输中隧道断时自动重连（用同一码/同一 relay 续传）
	b.lastCode = a.Code
	b.lastServer = a.Server
	b.lastNoAuth = a.NoAuth
	b.mu.Unlock()

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
					teardown(fmt.Errorf("tunnel_lost: P2P 隧道已断开（%w），请重新 connect", err))
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
					// relay 信令通道断开：即便 P2P 工具隧道还在，也必须拆除会话，
					// 否则 relay 端 Help 槽泄漏，新 connect 撞 already-has-helper。
					log.Printf("MCP relay read loop ended (P2P active): %v", err)
					teardown(fmt.Errorf("tunnel_lost: relay 信令通道已断开（%w），请重新 connect", err))
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
					teardown(fmt.Errorf("tunnel_lost: 隧道已断开（%w），请重新 connect", err))
					return
				}
				dispatchHelpToolMessage(msg, bridge)
			}
		}()
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
// uploadConcurrency 上传滑动窗口大小（同时在途的 write_file 数）。
// 取 1（顺序上传）：并发 WriteAt 失败时会在远端留空洞（高 offset 写了、低 offset 没写），
// 使「按远端 stat 大小续传」不安全（跳过空洞 → 文件损坏）。顺序上传保证远端大小恒等于
// 连续前缀，配合 transferLoop 的自动重连+续传能安全扛住网络抖动。代价是高 RTT 链路吞吐降，
// 但对「偶尔推二进制过跨境链路」的场景，正确性 + 带进度续传 >> 吞吐。
const uploadConcurrency = 1

// progressLoop 周期性把传输进度打到 stderr，直到 stop 关闭。done 由各 chunk goroutine
// 原子累加，total 为文件总大小（<=0 时只报已传字节）。
func progressLoop(op, path string, done *atomic.Int64, total int64, stop <-chan struct{}) {
	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			logTransferProgress(op, path, done.Load(), total)
		}
	}
}

// logTransferProgress 输出一行传输进度到 stderr。
func logTransferProgress(op, path string, done, total int64) {
	const mib = 1 << 20
	if total > 0 {
		fmt.Fprintf(os.Stderr, "[%s] %s %.1f%% (%.1f/%.1f MiB)\n",
			op, path, float64(done)/float64(total)*100, float64(done)/float64(mib), float64(total)/float64(mib))
	} else {
		fmt.Fprintf(os.Stderr, "[%s] %s %.1f MiB\n", op, path, float64(done)/float64(mib))
	}
}

// upload_file: 本机 → 远端。流水线并发上传：串行读本地（保证 offset 顺序分配）+
// 并发 write_file(绝对 offset，pwrite 幂等可乱序)，滑动窗口限并发。
//   - 从头传（offset=0）：先同步 truncate 远端为空，再并发填充（offset 写不 truncate）。
//   - 续传（offset>0）：以远端实际大小（stat）为续传点。
//   - 失败：建议从头重传（offset=0）——并发 WriteAt 不保证连续，失败可能留洞，
//     从头 truncate 重传才保证正确（隧道修复后单次成功率高，从头重传成本可接受）。
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
		// 续传以「远端文件实际大小」为准，而非信任调用方 offset：消除"写成功但响应丢致
		// 重复"的膨胀（详见 TestUploadResumeDedupsByRemoteSize）。
		statArgs, _ := json.Marshal(struct {
			Path string `json:"path"`
		}{Path: a.RemotePath})
		if res, serr := br.CallTool(ctx, "stat", statArgs); serr == nil {
			var st struct {
				Size int64 `json:"size"`
			}
			if json.Unmarshal(res, &st) == nil && st.Size >= 0 {
				a.Offset = st.Size
			}
		} else {
			a.Offset = 0 // 远端文件不存在 / stat 失败：从头传
		}
		if _, err := f.Seek(a.Offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek local %q to offset %d: %w", a.LocalPath, a.Offset, err)
		}
	}

	// 从头传：先同步 truncate 远端为空（offset 写模式不 truncate，需先清空避免旧内容残留）。
	if a.Offset == 0 {
		truncArgs, _ := json.Marshal(struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
			Create  bool   `json:"create"`
		}{Path: a.RemotePath, Content: []byte{}, Create: true})
		if _, err := callToolRetry(ctx, br, "write_file", truncArgs); err != nil {
			return nil, fmt.Errorf("truncate remote %q: %w", a.RemotePath, err)
		}
	}

	// 流水线并发上传
	var totalSize int64
	if fi, e := f.Stat(); e == nil {
		totalSize = fi.Size()
	}
	var doneBytes atomic.Int64
	doneBytes.Store(a.Offset)
	progStop := make(chan struct{})
	go progressLoop("upload", a.RemotePath, &doneBytes, totalSize, progStop)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, uploadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	setErr := func(e error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel() // 唤醒在途 goroutine 与派发循环快速收尾
		}
		mu.Unlock()
	}
	hasErr := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}

	total := a.Offset
	var chunks int
	offset := a.Offset
	buf := make([]byte, fileTransferChunk)
	for {
		if hasErr() {
			break
		}
		n, rerr := io.ReadFull(f, buf)
		// ReadFull 在最后一块（短读）返回 ErrUnexpectedEOF + n>0；EOF 表示 n==0。
		eof := errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
		if !eof && rerr != nil {
			setErr(fmt.Errorf("read local %q: %w", a.LocalPath, rerr))
			break
		}
		if n == 0 {
			break // 空文件已由上面的 truncate 建好；续传到末尾亦在此结束
		}
		data := make([]byte, n) // 拷贝（buf 下轮复用）
		copy(data, buf[:n])
		at := offset
		offset += int64(n)
		total += int64(n)
		chunks++

		select {
		case sem <- struct{}{}:
		case <-cctx.Done():
		}
		if hasErr() {
			break
		}
		wg.Add(1)
		go func(data []byte, at int64) {
			defer wg.Done()
			defer func() { <-sem }()
			wargs, _ := json.Marshal(struct {
				Path    string `json:"path"`
				Content []byte `json:"content"`
				At      int64  `json:"at"`
			}{Path: a.RemotePath, Content: data, At: at})
			if _, err := callToolRetry(cctx, br, "write_file", wargs); err != nil {
				setErr(fmt.Errorf("upload to remote %q failed near offset %d (reconnect and re-call upload_file with offset=0 to resume from scratch): %w", a.RemotePath, at, err))
				return
			}
			doneBytes.Add(int64(len(data)))
		}(data, at)

		if eof {
			break
		}
	}
	wg.Wait()
	close(progStop)
	if firstErr != nil {
		return nil, firstErr
	}
	logTransferProgress("upload", a.RemotePath, total, totalSize) // 收尾打印 100%
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
	// 进度显示：先 stat 远端拿总大小（拿不到则只报已传字节）
	var totalSize int64
	dstatArgs, _ := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: a.RemotePath})
	if res, serr := br.CallTool(ctx, "stat", dstatArgs); serr == nil {
		var st struct {
			Size int64 `json:"size"`
		}
		if json.Unmarshal(res, &st) == nil {
			totalSize = st.Size
		}
	}
	var doneBytes atomic.Int64
	doneBytes.Store(a.Offset)
	progStop := make(chan struct{})
	go progressLoop("download", a.LocalPath, &doneBytes, totalSize, progStop)
	defer close(progStop)
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
			doneBytes.Store(total)
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
	logTransferProgress("download", a.LocalPath, total, totalSize) // 收尾打印 100%
	return json.Marshal(fileTransferResult{Bytes: total, Chunks: chunks})
}

// negotiateP2POnly 只做 P2P 协商：读 relay 收集 PeerAddrReady 喂给 P2P manager、
// 等打洞结果，**不发 ToolHello**（与 share 端 negotiateP2P 对称）。返回 p2p tunnel
// （成功）或 nil（失败/超时，调用方回退 relay）。
//
// 修复 MCP-over-p2p 握手死锁：旧流程 help 先在 relay 上 handshakeToolWithP2P 等
// ToolHelloAck，但 share p2p 建立后进 handleTunnelP2P→ReadModeHeader 阻塞等 help 在
// p2p 隧道上发 mode header；help 卡在等 relay Ack、share 卡在等 p2p header → 互相
// 死等、connect 15s timeout（p2p 越快建立越必现）。改为两端都「先纯 P2P 协商，结果
// 决定走 p2p 握手 or relay 握手」即消除——p2p 成功时 help 直接 WriteModeHeader，
// share 的 ReadModeHeader 立即读到，不再死锁。
func (h *HelpMode) negotiateP2POnly(mgr *p2p.P2PManager, mode p2p.P2PMode, resultCh <-chan p2p.P2PResult) (*p2p.UDPTunnel, error) {
	select {
	case result := <-resultCh:
		return result.Tunnel, result.Err
	default:
	}
	fmt.Fprintln(os.Stderr, "MCP: 正在尝试 P2P 直连...")
	timeout := 12 * time.Second
	if mode == p2p.P2PModeRequired {
		timeout = 32 * time.Second
	}
	h.client.SetReadDeadline(time.Now().Add(timeout))
	peerReady := false
	for !peerReady {
		msg, err := h.client.ReadMessage()
		if err != nil {
			h.client.SetReadDeadline(time.Time{})
			if isNetTimeout(err) {
				mgr.Close()
				if mode == p2p.P2PModeRequired {
					return nil, fmt.Errorf("P2P 协商超时：对端未响应")
				}
				return nil, nil
			}
			mgr.Close()
			return nil, err
		}
		switch msg.Type {
		case proto.MsgPeerAddrReady:
			var ready proto.PeerAddrReady
			if err := proto.DecodePayload(msg, &ready); err == nil {
				mgr.HandlePeerAddrReady(&ready)
			}
			peerReady = true
		case proto.MsgHeartbeat:
			continue
		case proto.MsgError:
			// 对端断 / relay 错误：提前返回以回退 relay，不干等满 timeout（对称 share 的 negotiateP2P）
			h.client.SetReadDeadline(time.Time{})
			var errMsg proto.ErrorMessage
			proto.DecodePayload(msg, &errMsg)
			mgr.Close()
			return nil, fmt.Errorf("relay error during P2P negotiation: %s", errMsg.Message)
		default:
			continue
		}
	}
	h.client.SetReadDeadline(time.Time{})
	result := <-resultCh
	if result.Tunnel != nil {
		fmt.Fprintln(os.Stderr, "MCP: P2P 直连已建立！")
	} else if result.Err == nil {
		fmt.Fprintln(os.Stderr, "MCP: P2P 打洞超时，回退到中转模式")
		mgr.Close()
	} else {
		mgr.Close()
	}
	return result.Tunnel, result.Err
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
