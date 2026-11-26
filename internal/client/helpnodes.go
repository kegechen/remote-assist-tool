package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
)

// help 自动模式按协助码找节点（spec 6.4）。
//
//   - 码的首位是签发它的节点的前缀（节点模式的 relay 发码时保证）。列表里有这个节点、没被停用，就连它，
//     证书按列表严格比对（nodedial.go），本机专用地址先试。
//   - 找不到前缀时按 RefreshIfStale 的规则同步向 CF 取一次（最多 helpRefreshWait），还找不到就连默认节点。
//     没有能用的列表时与没有这个功能时一样：连内置默认地址，按配置走 TOFU。
//   - 过渡期回退：按前缀连到的不是默认节点、在那里加入被拒时，去默认节点再试一次。节点模式启用之前发的码首位是随机的，
//     可能碰巧是别的节点的前缀。两个版本之后删掉。
//
// help 不粘节点、不退避：码决定了只能去哪台，每次加入（含 MCP 的自动重连）都按码重新找。

// helpRefreshWait 是找不到前缀时最多为 CF 等多久（spec 6.4：最多 3 秒）。请求本身由 HTTP 客户端的总超时限制，
// 但写缓存要等文件锁，这里另外兜底。变量以便测试调整。
var helpRefreshWait = directoryFetchTimeout

// helpNodes 是 help 自动模式按码找节点用的状态。一个进程一个：MCP 进程里每次 connect 共用（dirManager 的合并请求、
// 重试间隔、提示去重都在里面）。
type helpNodes struct {
	dm        *dirManager
	transport string // --transport
	logf      func(format string, args ...any)
	bg        sync.WaitGroup // 后台刷新列表的 goroutine

	mu        sync.Mutex
	lastRoute string // 上一次找节点的结果那一行（没打的为空串），见 noteRoute
}

func newHelpNodes(dm *dirManager, transport string, logf func(format string, args ...any)) *helpNodes {
	return &helpNodes{dm: dm, transport: transport, logf: logf}
}

// helpRoute 是一次加入要连的地方：node 非 nil 时是列表里的节点，否则是默认地址（TOFU）。
type helpRoute struct {
	node   *directory.Node
	routed bool // 按码的首位找到的节点；false 表示没找到、连的是默认节点或默认地址
	v      *directory.Verified
	cache  *directory.Cache
}

// refreshInBackground 在后台按 RefreshIfStale 的规则刷新列表，不等它（spec 6.2：help 启动时在后台取，不拖慢连接）。
func (hn *helpNodes) refreshInBackground() {
	hn.bg.Add(1)
	go func() {
		defer hn.bg.Done()
		_ = hn.dm.RefreshIfStale(context.Background())
	}()
}

// route 为码 code 找要连的地方，并在后台刷新列表（refreshInBackground）。
func (hn *helpNodes) route(code string) helpRoute {
	hn.refreshInBackground()
	v, cache := hn.dm.Current()
	n := routeNodeForCode(v, code)
	if n == nil && codeRoutable(code) {
		// 找不到前缀：可能是列表旧了、没有新加的节点。和上面的后台刷新是同一个请求（dirManager 合并），只是在这里等它。
		ctx, cancel := context.WithTimeout(context.Background(), helpRefreshWait)
		_ = hn.dm.RefreshIfStale(ctx)
		cancel()
		v, cache = hn.dm.Current()
		n = routeNodeForCode(v, code)
		if n == nil && v != nil {
			// 只写「没有」：日志里不写协助码（spec 6.5），首位也是码的一部分。
			hn.noteRoute("[relay] 节点列表里没有这个协助码所属的节点，改连默认节点")
			return helpRoute{node: defaultNodeForHelp(v), v: v, cache: cache}
		}
	}
	if n != nil {
		hn.noteRoute("[relay] 协助码属于节点 " + nodeLabel(*n))
		return helpRoute{node: n, routed: true, v: v, cache: cache}
	}
	hn.noteRoute("")
	return helpRoute{node: defaultNodeForHelp(v), v: v, cache: cache}
}

// noteRoute 打找节点的结果 line，与上一次相同时不打：MCP 每次 connect（含自动重连）都按码重新找，同一个结果不用
// 反复说。line 为空表示这次没什么可说的（没有列表、码不路由），之后的结果照常打。
func (hn *helpNodes) noteRoute(line string) {
	hn.mu.Lock()
	same := line == hn.lastRoute
	hn.lastRoute = line
	hn.mu.Unlock()
	if line != "" && !same {
		hn.logf("%s", line)
	}
}

// defaultNodeForHelp 返回 help 能连的默认节点：列表里的默认节点没被停用（排空的照样接受它发过的码）；
// 没有列表、默认节点被停用或丢弃时为 nil，连默认地址。
func defaultNodeForHelp(v *directory.Verified) *directory.Node {
	d := v.DefaultNode()
	if d == nil || d.Status == directory.StatusDisabled {
		return nil
	}
	return d
}

// connect 让 c 连上 n（nil 表示默认地址 defaultAddr，按配置走 TOFU）。c 应处于已 Close 的状态。
func (hn *helpNodes) connect(c *Client, n *directory.Node, r helpRoute, defaultAddr string) error {
	if n == nil {
		c.leaveNode(defaultAddr)
		return c.Connect()
	}
	nc, err := dialListedNode(context.Background(), hn.dm, hn.transport, hn.logf, *n, "", r.cache, r.v)
	if err != nil {
		return nodeError(*n, err)
	}
	c.adoptNode(nc)
	return nil
}

// relayConnectError 表示还没到加入那一步、连 relay 就失败了。MCP connect 据此报 "relay connect failed"，与加入失败区分。
type relayConnectError struct{ err error }

func (e *relayConnectError) Error() string { return e.err.Error() }
func (e *relayConnectError) Unwrap() error { return e.err }

// UseNodeList 进入自动模式（没指定 relay）：按协助码首位找节点（spec 6.4），P2P 的 STUN 跟着实际连上的节点。
// home 是列表缓存所在的家目录（空串表示当前用户的）。本程序没有内置根公钥、拿不到家目录时不用列表，只连配置的地址，
// STUN 照样跟着它（调用方没按默认地址推导 STUN）。
func (h *HelpMode) UseNodeList(home string) {
	h.stunFollowsRelay = true
	if hn := newHelpNodesAt(home, h.client.config.Transport); hn != nil {
		h.useNodes(hn)
	}
}

// UseNodeList 同 HelpMode.UseNodeList：之后不带 server 的 connect 按协助码找节点，带 server 的照旧只连它。
// 两种 connect 的 STUN 都跟着实际连上的 relay。
func (b *HelpMCPBootstrap) UseNodeList(home string) {
	b.stunFollowsRelay = true
	b.nodes = newHelpNodesAt(home, b.cfg.Transport)
}

// LearnNodeList 用于显式模式（指定了 relay）：照旧只连指定的地址、按配置走 TOFU，展示不变；加入时带上列表位置、
// 收下 relay 转发的列表，记下本机专用地址（spec 6.1、6.5）。不从 CF 取。home 同 UseNodeList。
// 本程序没有内置根公钥、拿不到家目录时什么都不学。
func (h *HelpMode) LearnNodeList(home string) {
	h.learn = newOfflineDirManager(home, helpLogf)
}

// LearnNodeList 同 HelpMode.LearnNodeList：每次 connect（带不带 server）都照旧只连指定的地址，只学列表。
func (b *HelpMCPBootstrap) LearnNodeList(home string) {
	b.learn = newOfflineDirManager(home, helpLogf)
}

// helpLogf 是 help 列表提示的去处：stderr（MCP 的 stdout 是协议通道），有日志文件时再写一份进去（spec 6.8）。
func helpLogf(format string, args ...any) { fmt.Fprintf(stderrLog(), format+"\n", args...) }

// newHelpNodesAt 建 help 用的列表状态，提示打到 stderr。
func newHelpNodesAt(home, transport string) *helpNodes {
	dm := newDirManager(home, helpLogf)
	if dm == nil {
		return nil
	}
	return newHelpNodes(dm, transport, helpLogf)
}

// useNodes 让 h 按 hn 选节点；没有能用的列表时连现在配置的地址。
func (h *HelpMode) useNodes(hn *helpNodes) {
	h.nodes = hn
	h.defaultAddr = h.client.config.ServerAddr
	h.stunFollowsRelay = true
}

// p2pSTUN 是 P2P 用的 STUN 地址（也是 UDP 中继的地址）：指定了就用它；自动模式下跟着实际连上的 relay（relay 主机的 3478），
// 不按默认地址推导，同 ShareMode.p2pSTUN。
func (h *HelpMode) p2pSTUN() string {
	if stun := h.client.config.STUNServer; stun != "" || !h.stunFollowsRelay {
		return stun
	}
	return RelaySTUNAddr(h.client.serverAddr())
}

// connectAndJoin 连上 relay 并加入会话。自动模式下按协助码选节点，并做过渡期回退（spec 6.4）。
// 连 relay 失败返回 *relayConnectError；加入失败时已经 Close。
func (h *HelpMode) connectAndJoin() (*proto.JoinResponse, error) {
	hn := h.nodes
	if hn == nil {
		return h.joinVia(h.client.Connect)
	}
	r := hn.route(h.code)
	resp, err := h.joinVia(func() error { return hn.connect(h.client, r.node, r, h.defaultAddr) })
	if err == nil {
		h.node = r.node
		return resp, nil
	}
	d := defaultNodeForHelp(r.v)
	// 过渡期回退只接 relay 不说原因的拒绝（ErrJoinFailed：码不对、已过期、节点停用等）。连不上、读写失败、封禁不回退；
	// 「同源已有协助端」（ErrSessionBusySameAddr）说明会话就在这台上，去默认节点只会多记一次失败。
	if !r.routed || !errors.Is(err, ErrJoinFailed) || (d != nil && d.Prefix == r.node.Prefix) {
		return nil, err
	}
	hn.logf("[relay] 在节点 %s上加入失败（%v），到默认节点再试一次（节点列表启用之前发的协助码在默认节点上）", nodeLabel(*r.node), err)
	resp, ferr := h.joinVia(func() error { return hn.connect(h.client, d, r, h.defaultAddr) })
	if ferr != nil {
		hn.logf("[relay] 默认节点上也没能加入（%v）", ferr)
		// 报按前缀找到的节点上的错误：码的首位说它属于那里。
		return nil, err
	}
	h.node = d
	return resp, nil
}

// stalledRetryDelay 是 TCP 上等不到加入应答、关掉连接之后，隔多久再改走 QUIC 加入。var 仅为测试可调。
var stalledRetryDelay = time.Second

// joinVia 用 connect 让 h.client 连上 relay（c 应处于已 Close 的状态），然后加入会话。连 relay 失败返回
// *relayConnectError；加入失败时已经 Close。
//
// auto 模式下 TCP 连上了、加入请求也发出去了，应答却等不到（relay 发回的数据被网络丢掉，见 relaySilentError），
// 按同样的方式再连一次，只再试这一次：这时这个地址先拨 QUIC（tcpStalls）。握手成功不代表数据通，竞速只比握手，
// 不这样的话每次都会选中这条 TCP。
func (h *HelpMode) joinVia(connect func() error) (*proto.JoinResponse, error) {
	if err := connect(); err != nil {
		return nil, &relayConnectError{err}
	}
	resp, err := h.joinOrClose()
	var silent *relaySilentError
	if err == nil || !errors.As(err, &silent) || !silent.fallback {
		return resp, err
	}
	fmt.Fprintf(stderrLog(), "[relay] %v；改走 QUIC（UDP）重试\n", err)
	// 发出去的方向是通的：relay 多半收下了这次加入，协助端的位置由刚才那条 TCP 连接占着。joinOrClose 已经关掉它，
	// relay 读到关闭才把位置腾出来（之后 5 秒内新来的协助端可以顶替）；QUIC 上的加入抢在前面到，会被当成「已有
	// 协助端」拒掉。
	time.Sleep(stalledRetryDelay)
	if cerr := connect(); cerr != nil {
		return nil, &relayConnectError{fmt.Errorf("%v；改走 QUIC 重连失败：%w", err, cerr)}
	}
	resp, rerr := h.joinOrClose()
	switch {
	case rerr == nil:
		return resp, nil
	case isJoinRejected(rerr) || errors.Is(rerr, proto.ErrBlocked):
		return nil, rerr // relay 这次答复了，按它的答复报
	}
	return nil, fmt.Errorf("%v；改走 QUIC 重试后：%w", err, rerr)
}

// joinOrClose 加入会话，失败时关掉连接。
func (h *HelpMode) joinOrClose() (*proto.JoinResponse, error) {
	resp, err := h.join()
	if err != nil {
		h.client.Close()
		return nil, err
	}
	return resp, nil
}
