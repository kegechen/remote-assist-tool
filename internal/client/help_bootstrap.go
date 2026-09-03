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

// peerAddrRelay 把 PeerAddrReady 稳妥地送到 P2P manager 手里，哪怕消息比 manager
// 先到——先到就暂存，manager 一 attach 立刻补投。
//
// 为什么需要它：relay 对每次 advertise 只推送一次、且只推给对端。help 这边这条消息
// 会落在工具握手窗口内（share 在收到 ToolHello 之前就 advertise 完了），而 manager
// 要等握手结束、在后台 goroutine 里才启动（启动含同步 STUN + NAT 探测，放主路径会
// 拖慢 connect）。中间这段空窗如果把消息丢了，help 就永远没有对端地址，
// startHolePunching 直接 return，P2P 静默失效。
type peerAddrRelay struct {
	mu      sync.Mutex
	mgr     *p2p.P2PManager
	pending *proto.PeerAddrReady
}

// deliver 投递一条对端地址：manager 就绪就直接喂，否则暂存待 attach 补投。
func (r *peerAddrRelay) deliver(ready *proto.PeerAddrReady) {
	r.mu.Lock()
	mgr := r.mgr
	if mgr == nil {
		r.pending = ready
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	mgr.HandlePeerAddrReady(ready)
}

// attach 登记 manager 并补投暂存的地址。登记与取暂存在同一临界区内完成，
// 因此不存在「刚判断完 mgr==nil 就被 attach 抢先、消息永远没人消费」的窗口。
func (r *peerAddrRelay) attach(mgr *p2p.P2PManager) {
	r.mu.Lock()
	r.mgr = mgr
	pending := r.pending
	r.pending = nil
	r.mu.Unlock()
	if pending != nil {
		mgr.HandlePeerAddrReady(pending)
	}
}

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
	// --- 工具握手锚定在 relay ---
	// relay 是可靠通道，且 join 刚在同一条连接上成功往返过，握手必成。P2P 一律降级为
	// 事后热升级（见下方 upgradeToP2P）：打洞失败、单向可达都只意味着「留在中转」，
	// 不再影响 connect 的成败。
	//
	// 为什么这样改：UDP 打洞的成功判定天然是单向的（onP2PConnected 收到对方一个包就
	// 宣告成功、不等回应），两端因此可能得出相反结论。旧实现让 P2P 协商结果决定握手
	// 走哪条通道，于是出现——share 认为通了，去 P2P 隧道上死等 mode header 且不再读
	// relay；help 认为没通，在 relay 上发 ToolHello 无人应答 → 15s 后
	// "handshake failed: i/o timeout"。而且无法自愈：先建成隧道的一端此后发的是隧道
	// 二进制包，对端仍在用 json.Unmarshal 解 P2PTestPacket，解不出就丢。
	// 把握手锚定到 relay，这个竞态就整个消失了。
	// 握手窗口内到达的 PeerAddrReady 必须接住（见 peerAddrRelay 注释）：share 在收到
	// ToolHello 之前就已 advertise，relay 只推这一次，丢了 P2P 就彻底起不来。
	addrRelay := &peerAddrRelay{}
	key, hsErr := h.handshakeToolCapturing(addrRelay.deliver)
	if hsErr != nil {
		h.client.Close()
		return nil, fmt.Errorf("handshake failed: %w", hsErr)
	}
	bridge := mcp.NewBridge(h.client, key)

	// 心跳保活：每 30s 发 Heartbeat，relay 回 echo，避免 ReadMessage 2-min deadline
	// 因为空闲被触发，导致后台 goroutine 退出 → MCP 工具调用全部失效。
	// 心跳始终走 relay：即便工具流量已升级到 P2P，relay 信令通道仍需保活，它承担
	// 会话管理与 P2P 断开后的降级回退。
	h.client.StartHeartbeatLoop(30 * time.Second)

	// p2pConn 由后台升级 goroutine 填入，teardown 也要能关它 → 原子指针。
	var p2pConnPtr atomic.Pointer[P2PConn]
	// helloAckCh 把 relay 上的 ToolHelloAck 交给等待方。P2P 断开后要重新握手才能让
	// share 把 daemon 出口切回 relay，而此时 relay 已被读循环独占，只能这样转交。
	helloAckCh := make(chan proto.HelloAck, 1)
	// p2pMgrPtr 供 teardown 回收 P2P manager（它持有绑定的 UDP socket 与接收循环）。
	// 不回收的话，每次 connect/重连都会漏一个——transferWithReconnect 一次传输就可能
	// 重连 5 次。赋值发生在下方 manager 启动处。
	var p2pMgrPtr atomic.Pointer[p2p.P2PManager]
	// sessionDone 由 teardown 关闭，唤醒还阻塞在 P2P 协商结果上的升级 goroutine。
	// 只关 manager 是不够的：P2PManager.Close 仅 close(stopChan)，其 p2pTimeout 收到后
	// 直接 return 而不往 resultCh 推结果，干等 resultCh 就是永久泄漏。
	sessionDone := make(chan struct{})

	// teardown 拆除当前会话，sync.Once 保证只跑一次：关闭 relay 控制连接
	// （→ 心跳循环经 IsClosed() 退出，并让 relay 端读循环 EOF → DisconnectClient
	// 立即清 Help 槽）、唤醒在途工具调用、清 bootstrap 状态。任一读循环（P2P / relay）
	// 检测到断开都调它。修复点：旧实现 P2P 隧道断开后只 bridge.Disconnect + 清 b.help，
	// 不关 h.client，心跳不停 → relay 端 Help 槽永驻 → 新 connect 永久 already-has-helper。
	var teardownOnce sync.Once
	teardown := func(err error) {
		teardownOnce.Do(func() {
			close(sessionDone)
			bridge.Disconnect(err)
			h.client.Close()
			if pc := p2pConnPtr.Load(); pc != nil {
				pc.Close()
			}
			if mgr := p2pMgrPtr.Load(); mgr != nil {
				mgr.Close()
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
	// P2P 起始为 false：连接先在 relay 上可用，升级成功后由 upgradeToP2P 回填
	// b.activeResult.P2P，重复 connect 走幂等复用路径即可读到最新状态。
	result := connectResult{
		Connected:   true,
		SessionID:   resp.SessionID,
		Server:      effectiveCfg.ServerAddr,
		PeerVersion: resp.PeerVersion,
		PeerHost:    resp.PeerHost,
		HelpVersion: version.Info(),
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

	// --- P2P：后台协商并热升级，不阻塞 connect 返回 ---
	p2pMode := p2p.ParseP2PMode(effectiveCfg.P2PMode)
	// 私网/loopback relay（standalone / 同 LAN）下 relay 已直连、P2P 多余，且 standalone
	// 不启 STUN 必然打洞超时；auto 模式自动跳过，省掉后台徒劳重试。
	if p2pMode == p2p.P2PModeAuto && IsLANServer(effectiveCfg.ServerAddr) {
		fmt.Fprintf(os.Stderr, "MCP: server %s 为私网/loopback，relay 已 LAN 直连，跳过 P2P（如需强制用 --p2p required）\n", effectiveCfg.ServerAddr)
		p2pMode = p2p.P2PModeDisabled
	}
	// upgradeDone 报告后台升级的最终结果，仅 --p2p required 会去等它。
	upgradeDone := make(chan bool, 1)

	// relay 读循环：单点消费 relay 连接，兼做 P2P 信令的转发。协商不再由主线程抢读
	// relay，两者争抢同一条连接的问题（以及由此需要的 ResetDecoder）就此消失。
	go func() {
		for {
			h.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
			msg, err := h.client.ReadMessage()
			if err != nil {
				// relay 信令通道断开：即便 P2P 工具隧道还在，也必须拆除会话，
				// 否则 relay 端 Help 槽泄漏，新 connect 撞 already-has-helper。
				teardown(fmt.Errorf("tunnel_lost: 隧道已断开（%w），请重新 connect", err))
				return
			}
			switch msg.Type {
			case proto.MsgPeerAddrReady:
				var ready proto.PeerAddrReady
				if err := proto.DecodePayload(msg, &ready); err == nil {
					// 经 addrRelay 投递：manager 可能还没起来（它在后台 goroutine 里
					// 启动，含同步 STUN），暂存后由 attach 补投，绝不丢。
					addrRelay.deliver(&ready)
				}
			case proto.MsgToolHelloAck:
				// 交给 downgradeToRelay；无人等待时丢弃（初次握手在读循环启动前已完成）。
				var ack proto.HelloAck
				if err := proto.DecodePayload(msg, &ack); err == nil {
					select {
					case helloAckCh <- ack:
					default:
					}
				}
			case proto.MsgError:
				var errMsg proto.ErrorMessage
				proto.DecodePayload(msg, &errMsg)
				log.Printf("MCP relay error: %s", errMsg.Message)
			default:
				// 始终投递，即使工具流量已经升级到 P2P。
				//
				// 不能按「P2P 活跃就只 drain」来做：P2P 断开时 share 会先把 daemon
				// 出口切回 relay 并可能立刻发出响应，而 help 这边尚未复位标志，那条
				// 响应就被当垃圾丢掉、调用方只能干等 deadline。
				// 重复投递本身是安全的——HandleInbound 的 pending channel 是
				// buffered 1 且用非阻塞 select 投递，重复的那份自然被丢弃；何况
				// share 端同一时刻只往一条通道发，实际也不会重复。
				dispatchHelpToolMessage(msg, bridge)
			}
		}
	}()

	if p2pMode != p2p.P2PModeDisabled {
		go b.upgradeToP2P(h, bridge, key, &effectiveCfg, resp.SessionID, p2pMode, addrRelay,
			&p2pConnPtr, &p2pMgrPtr, helloAckCh, teardown, upgradeDone, sessionDone)
	}

	// --p2p required 明确表示「不接受中转」，因此这里必须同步等升级结果：连接虽已在
	// relay 上可用，但不满足用户的硬要求，宁可拆掉并如实报错，也不能悄悄留在中转。
	// auto / disabled 不走这里，connect 立即返回，P2P 纯后台升级。
	if p2pMode == p2p.P2PModeRequired {
		select {
		case ok := <-upgradeDone:
			if !ok {
				teardown(errors.New("P2P required but not established"))
				return nil, fmt.Errorf("P2P required but failed: 打洞或双向探活未通过（--p2p required 不接受中转回退）")
			}
			result.P2P = true
		case <-time.After(p2pRequiredWaitTimeout):
			teardown(errors.New("P2P required but negotiation timed out"))
			return nil, fmt.Errorf("P2P required but failed: 协商超时")
		}
	}

	return json.Marshal(result)
}

// p2pRequiredWaitTimeout --p2p required 下 connect 同步等待 P2P 升级的上限。
// 取值须覆盖 manager 自身的打洞超时（required 模式 30s）加上 mode header 与双向
// 探活的往返余量。
const p2pRequiredWaitTimeout = 60 * time.Second

// upgradeToP2P 在 relay 工具通道已经可用之后，后台把工具流量热升级到 P2P 直连。
//
// 全程「失败即静默留在 relay」：P2P 只是加速，任何一步不成都不影响已经可用的会话。
// 关键是 ProbeBidirectional —— 隧道建成不等于双向可达。UDP 打洞的成功判定是单向的，
// 若不做双向证实就切换，可能把好端端的 relay 会话换成一条只能单向发包的死隧道。
func (b *HelpMCPBootstrap) upgradeToP2P(
	h *HelpMode,
	bridge *mcp.Bridge,
	key [32]byte,
	cfg *Config,
	sessionID string,
	mode p2p.P2PMode,
	addrRelay *peerAddrRelay,
	p2pConnPtr *atomic.Pointer[P2PConn],
	p2pMgrPtr *atomic.Pointer[p2p.P2PManager],
	helloAckCh <-chan proto.HelloAck,
	teardown func(error),
	done chan<- bool,
	sessionDone <-chan struct{},
) {
	// notify 只在首次生效（done 带缓冲，满了就丢），供 --p2p required 的同步等待。
	notify := func(ok bool) {
		select {
		case done <- ok:
		default:
		}
	}

	fmt.Fprintln(os.Stderr, "MCP: 后台尝试 P2P 直连...")

	// manager 在这里启动而不是在 connect 主路径上：Start 内含同步 STUN 发现 + NAT 类型
	// 探测（UDP 被封时要把三台外部 STUN 依次试完），放主路径会让 connect 干等十几秒，
	// 与「连接立即可用、P2P 后台升级」的设计相悖。
	mgr := p2p.NewP2PManager(mode, cfg.STUNServer, cfg.BindIP)
	mgr.SetRelayConn(h.client)
	p2pMgrPtr.Store(mgr) // 登记给 teardown 回收
	resultCh, startErr := mgr.Start(sessionID, false)
	if startErr != nil {
		log.Printf("MCP P2P manager start failed, staying on relay: %v", startErr)
		mgr.Close()
		notify(false)
		return
	}
	// 登记 manager 并补投握手窗口里暂存的对端地址。没有这一步，help 永远没有
	// peerInfo，startHolePunching 直接 return，P2P 静默失效。
	addrRelay.attach(mgr)

	var result p2p.P2PResult
	var ok bool
	select {
	case result, ok = <-resultCh:
	case <-sessionDone:
		// 会话已拆（relay 断 / --p2p required 等待超时）。干等 resultCh 永远等不到，
		// manager 被 Close 时不会往它推任何东西。
		mgr.Close()
		notify(false)
		return
	}
	if !ok || result.Tunnel == nil {
		if result.Err != nil {
			log.Printf("MCP P2P negotiation failed, staying on relay: %v", result.Err)
		}
		fmt.Fprintln(os.Stderr, "MCP: P2P 未建立，继续使用中转（不影响使用）")
		mgr.Close()
		notify(false)
		return
	}

	pc := NewP2PConn(result.Tunnel)
	// 先发 mode header 告诉 share 这条隧道走工具模式，再做双向探活。
	if err := pc.WriteModeHeader(); err != nil {
		log.Printf("MCP P2P mode header send failed, staying on relay: %v", err)
		result.Tunnel.Close()
		mgr.Close()
		notify(false)
		return
	}
	if err := pc.ProbeBidirectional(p2pProbeTimeout); err != nil {
		// 最典型的场景：打洞只单向通（我收得到对方，对方收不到我）。隧道看似建成，
		// 实际发出去的包对端永远收不到 —— 必须原地放弃，留在 relay。
		fmt.Fprintf(os.Stderr, "MCP: P2P 双向探活未通过（%v），继续使用中转\n", err)
		result.Tunnel.Close()
		mgr.Close()
		notify(false)
		return
	}

	// 会话可能已经没了（relay 断开，或 --p2p required 等待超时后 teardown）。此时切换
	// 毫无意义，还会往 stderr 打出「已切换到 P2P 直连」，与 connect 已报的失败自相矛盾。
	if !b.sessionAlive(bridge) {
		result.Tunnel.Close()
		mgr.Close()
		notify(false)
		return
	}

	// 双向证实通过：把工具流量切到隧道。复用 relay 握手协商出的同一把会话密钥
	// （key 由 code+双方 nonce 派生，与传输通道无关），不必在 P2P 上再握一次手，
	// 也就不存在新旧 key 的解密竞态。
	p2pConnPtr.Store(pc)
	bridge.SwapConn(pc, key)
	fmt.Fprintln(os.Stderr, "MCP: 已切换到 P2P 直连")

	b.mu.Lock()
	if b.bridge == bridge {
		b.activeResult.P2P = true
	}
	b.mu.Unlock()
	notify(true)

	// P2P 读循环：工具响应此后从隧道来。
	for {
		msg, err := pc.ReadMessage()
		if err != nil {
			if mode == p2p.P2PModeRequired {
				// required 明确表示不接受中转，隧道没了就拆会话，不能偷偷降级。
				p2pConnPtr.Store(nil)
				pc.Close()
				mgr.Close()
				teardown(fmt.Errorf("tunnel_lost: P2P 隧道断开且 --p2p required 不接受中转回退（%w），请重新 connect", err))
				return
			}
			b.downgradeToRelay(h, bridge, pc, p2pConnPtr, helloAckCh, teardown, err)
			return
		}
		dispatchHelpToolMessage(msg, bridge)
	}
}

// sessionAlive 报告 bridge 是否仍是当前活动会话。后台升级 goroutine 在动 bridge 之前
// 必须过这一关：teardown（relay 断 / --p2p required 超时）会把它换掉或清空。
func (b *HelpMCPBootstrap) sessionAlive(bridge *mcp.Bridge) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bridge == bridge
}

// downgradeToRelay P2P 隧道中途断掉时把工具流量切回 relay，而不是判死整个会话——
// relay 信令通道此刻通常还活着（心跳一直在跑），没理由让用户重连。
//
// 切回必须重新握手：share 端 daemon 的响应出口仍指向已死的隧道，得让它 SwapConn
// 回 relay。share.handleRelayToolHello 正是为此设计的（见其注释），重发 ToolHello
// 即可让对端切回并给出新的会话密钥。
//
// 已知取舍：降级窗口内在途的 CallTool 收不到响应，会等到各自 ctx deadline 才失败
// （不主动 Disconnect，否则等于放弃整条会话）。P2P 中途断本就少见，比起原先直接
// 判死会话仍是净改善。
func (b *HelpMCPBootstrap) downgradeToRelay(
	h *HelpMode,
	bridge *mcp.Bridge,
	pc *P2PConn,
	p2pConnPtr *atomic.Pointer[P2PConn],
	helloAckCh <-chan proto.HelloAck,
	teardown func(error),
	cause error,
) {
	p2pConnPtr.Store(nil)
	pc.Close()

	if h.client.IsClosed() {
		// relay 也没了，会话确实结束。
		teardown(fmt.Errorf("tunnel_lost: 隧道已断开（%w），请重新 connect", cause))
		return
	}
	fmt.Fprintf(os.Stderr, "MCP: P2P 隧道中断（%v），正在切回中转...\n", cause)

	// 排空可能残留的陈旧 Ack，否则会被当成本次握手的应答，派生出错误的 key。
	select {
	case <-helloAckCh:
	default:
	}

	hello := proto.NewHello()
	if err := h.client.SendMessage(proto.MsgToolHello, &hello); err != nil {
		teardown(fmt.Errorf("tunnel_lost: P2P 中断后切回中转失败（%w），请重新 connect", err))
		return
	}
	select {
	case ack := <-helloAckCh:
		if !ack.Accept {
			teardown(fmt.Errorf("tunnel_lost: P2P 中断后对端拒绝重新握手（%s），请重新 connect", ack.ErrorMsg))
			return
		}
		bridge.SwapConn(h.client, proto.DeriveSessionKey(h.code, ack.NonceB64, hello.NonceB64))
		b.mu.Lock()
		if b.bridge == bridge {
			b.activeResult.P2P = false
		}
		b.mu.Unlock()
		fmt.Fprintln(os.Stderr, "MCP: 已切回中转模式，连接继续可用")
	case <-time.After(15 * time.Second):
		teardown(fmt.Errorf("tunnel_lost: P2P 中断后切回中转超时，请重新 connect"))
	}
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
