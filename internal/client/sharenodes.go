package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
)

// share 自动模式下的选节点（spec 6.3、6.9）。
//
//   - 没有能用的列表（没有、过期、缓存读不了）时与没有这个功能时完全一样：连内置默认地址，按配置走 TOFU。
//   - 有列表时只连列表里的节点，证书按列表里的指纹严格比对（nodedial.go）。
//   - 粘性：断线重连先回当前节点，连续 shareNodeFailLimit 次连接或注册失败才重新选；健康节点上不主动换
//     （换节点就换码，默认节点恢复后也不迁回）。当前节点被停用、从列表里消失时马上重选；排空时照样回去
//     （relay 接受凭令牌复用原码的重连），等协助端时再迁走（migrate）。
//   - 只在三个时机拨非当前节点：启动选节点、连续失败后重选、排空迁移。同一次选节点每个节点最多拨一次，
//     失败的节点进退避（nodeBackoff）。不定时测速。
//
// 方法都只在 share 的主循环里调用；并行建连的 goroutine 只碰 dm、backoff 和 logf，它们自己加锁。

// shareNodeFailLimit 是当前节点连续连接或注册失败这么多次才重新选节点。
const shareNodeFailLimit = 3

// errNoNodes 表示列表里现在没有能拨的节点：都停用、排空、权重为 0，或都在失败退避期内。
var errNoNodes = errors.New("节点列表里暂时没有可用的节点（都已停用、排空，或在失败退避期内）")

// shareNodes 是 share 自动模式的选节点状态。
type shareNodes struct {
	dm          *dirManager
	backoff     *nodeBackoff
	transport   string // --transport
	defaultAddr string // 没有能用的列表时连的地址
	// hint 是升级前所在节点的地址（--relay-hint）：只在第一次选节点时先试，之后仍是自动模式。
	hint string
	logf func(format string, args ...any)

	current *directory.Node // 当前所在的节点；nil 表示不在列表节点上（没有列表），或要重新选
	fails   int             // 当前节点连续连接或注册失败的次数
	// unlisted 是不在列表节点上时最近一次注册成功的地址：之后拿到了列表、这个地址正是列表里某个节点的，
	// 就把那个节点当作当前节点（粘性），而不是重新选。
	unlisted string
	// offListDraining：不在列表节点上（没有能用的列表、连的是默认地址）时，那里的 relay 说过自己在排空。之后不在
	// 列表节点上时注册都不再声明列表能力，像旧客户端那样被接纳，见 failedOffList。
	offListDraining bool

	pending      *chosenNode // 排空迁移时已经连上的新节点，下一次 connect 直接接管
	migrateNoted bool        // 「暂时迁不走」已经提示过；迁走了、或者不用迁了再复位
}

// chosenNode 是选节点的结果：建好的连接、节点、握手耗时，以及先试过但没连上的默认节点（没有时为 nil）。
type chosenNode struct {
	r             *nodeConn
	n             directory.Node
	took          time.Duration
	defaultFailed *directory.Node
}

func newShareNodes(dm *dirManager, transport, defaultAddr, hint string, logf func(format string, args ...any)) *shareNodes {
	return &shareNodes{dm: dm, backoff: newNodeBackoff(), transport: transport, defaultAddr: defaultAddr, hint: hint, logf: logf}
}

// nodeLabel 是提示里节点的写法：名称（前缀）。名称、前缀来自验过签的列表，已排除控制字符。
func nodeLabel(n directory.Node) string {
	return fmt.Sprintf("%s（%s）", n.Name, n.Prefix)
}

// leaveReason 说明为什么不该留在节点 n 上（n 为 nil 表示它已不在列表里）；可以留下时返回空串。
// 权重为 0 的启用节点不算：权重只管选节点，已经在上面的 share 照常留下。
func leaveReason(n *directory.Node) string {
	switch {
	case n == nil:
		return "已不在节点列表里"
	case n.Status == directory.StatusDraining:
		return "正在排空"
	case n.Status != directory.StatusActive:
		return "已停用"
	}
	return ""
}

// currentNode 返回当前所在的列表节点，不在列表节点上时为 nil。对 nil 安全（显式模式）。
func (sn *shareNodes) currentNode() *directory.Node {
	if sn == nil {
		return nil
	}
	return sn.current
}

// currentPrefix 返回当前节点的前缀，不在列表节点上时为空。对 nil 安全（显式模式）。
func (sn *shareNodes) currentPrefix() string {
	if n := sn.currentNode(); n != nil {
		return n.Prefix
	}
	return ""
}

// declaresList 报告注册时要不要带上列表位置（也是能力声明）。不在列表节点上、默认地址上的 relay 说过自己在排空时
// 不带：带了只会再被拒一次（见 failedOffList）。
func (sn *shareNodes) declaresList() bool {
	return sn.current != nil || !sn.offListDraining
}

func (sn *shareNodes) setCurrent(n *directory.Node) {
	if n == nil || sn.current == nil || sn.current.Prefix != n.Prefix {
		sn.fails = 0
	}
	sn.current = n
}

// connect 让 c 连上一个节点（c 应处于已 Close 的状态）。失败时调用方报 failed、退避后再调。
func (sn *shareNodes) connect(c *Client) error {
	if p := sn.pending; p != nil {
		sn.pending = nil
		sn.adopt(c, p.n, p.r)
		return nil
	}
	v, cache := sn.dm.Current()
	if v == nil {
		sn.setCurrent(nil)
		c.leaveNode(sn.defaultAddr)
		return c.Connect()
	}
	if sn.current == nil && sn.unlisted != "" {
		if n := nodeByAddr(v, cache, sn.unlisted); n != nil {
			sn.setCurrent(n)
		}
		sn.unlisted = ""
	}
	if cur := sn.current; cur != nil {
		n := v.NodeByPrefix(cur.Prefix)
		if n != nil && (n.Status == directory.StatusActive || n.Status == directory.StatusDraining) {
			// 用这份列表里的节点信息：地址、指纹可能已经更新。
			r, err := sn.dial(context.Background(), *n, "", cache, v)
			if err != nil {
				return err
			}
			sn.adopt(c, *n, r)
			return nil
		}
		sn.logf("[relay] 当前节点 %s %s，重新选择节点", nodeLabel(*cur), leaveReason(n))
		sn.setCurrent(nil)
	}
	return sn.pick(c, v, cache)
}

// failed 记一次连接或注册失败。当前节点拒绝新注册（排空、停用），或连续失败 shareNodeFailLimit 次时，
// 它进退避，下一次 connect 重新选节点。ID 冲突不算：那是本机另一个 share 占着，换节点只会让两个都跑起来。
func (sn *shareNodes) failed(err error) {
	cur := sn.current
	if cur == nil {
		sn.failedOffList(err)
		return
	}
	var rej *registerRejected
	errors.As(err, &rej)
	switch {
	case rej != nil && rej.Code == proto.ErrCodeClientIDConflict:
		return
	case rej != nil && (rej.Code == proto.ErrCodeNodeDraining || rej.Code == proto.ErrCodeNodeDisabled):
		state := "正在排空"
		if rej.Code == proto.ErrCodeNodeDisabled {
			state = "已停用"
		}
		sn.logf("[relay] 节点 %s %s，不接受新注册，改选其他节点", nodeLabel(*cur), state)
	default:
		sn.fails++
		if sn.fails < shareNodeFailLimit {
			return
		}
		sn.logf("[relay] 节点 %s 连续 %d 次连接或注册失败，重新选择节点", nodeLabel(*cur), sn.fails)
	}
	sn.backoff.Fail(cur.Prefix)
	sn.setCurrent(nil)
}

// failedOffList 处理不在列表节点上（没有能用的列表，连的是默认地址）时的失败。那里的 relay 说自己排空、停用而拒绝
// 注册，是要新客户端改选别的节点，可本机没有列表、无从选起：先同步向 CF 取一次列表（受 1 分钟重试间隔限制），取到了
// 下次 connect 就按列表选。排空的话，之后不在列表节点上时注册不再声明列表能力，像旧客户端那样被接纳——没有能用的
// 列表时要与没有这个功能时一样（spec 6.3），不能因为声明了能力反而一直拿不到码。停用的节点新旧客户端一律拒绝，不用改。
func (sn *shareNodes) failedOffList(err error) {
	var rej *registerRejected
	if !errors.As(err, &rej) || (rej.Code != proto.ErrCodeNodeDraining && rej.Code != proto.ErrCodeNodeDisabled) {
		return
	}
	if rej.Code == proto.ErrCodeNodeDraining && !sn.offListDraining {
		sn.offListDraining = true
		sn.logf("[relay] 中转服务 %s 正在排空，本机还没有节点列表、无从改选，先按不带节点列表的方式在它上面注册", sn.defaultAddr)
	}
	_ = sn.dm.RefreshIfStale(context.Background())
}

// registered 在 addr 上注册成功：当前节点的失败记录清零。
func (sn *shareNodes) registered(addr string) {
	sn.fails = 0
	sn.unlisted = ""
	if sn.current == nil {
		sn.unlisted = addr
		return
	}
	sn.backoff.Succeed(sn.current.Prefix)
}

// pick 重新选节点：先试升级前所在的节点（只在第一次），然后按 planShare 的计划。
func (sn *shareNodes) pick(c *Client, v *directory.Verified, cache *directory.Cache) error {
	var errs nodeErrors
	if hint := sn.hint; hint != "" {
		sn.hint = ""
		if n := nodeByAddr(v, cache, hint); n != nil && n.Status == directory.StatusActive && sn.backoff.Allowed(n.Prefix) {
			r, err := sn.dial(context.Background(), *n, hint, cache, v)
			if err == nil {
				sn.adopt(c, *n, r)
				sn.logf("[relay] 已回到升级前所在的节点 %s", nodeLabel(*n))
				return nil
			}
			sn.backoff.Fail(n.Prefix)
			errs = append(errs, nodeError(*n, err))
		}
	}
	ch, err := sn.choose(v, cache, sn.backoff.Allowed)
	if err != nil {
		if len(errs) == 0 {
			return err
		}
		return append(errs, err)
	}
	sn.adopt(c, ch.n, ch.r)
	switch {
	case ch.defaultFailed != nil:
		sn.logf("[relay] 默认节点 %s 连接失败，已改用节点 %s（握手 %dms）", nodeLabel(*ch.defaultFailed), nodeLabel(ch.n), ch.took.Milliseconds())
	case ch.n.Prefix == v.DefaultPrefix:
		sn.logf("[relay] 已连接默认节点 %s（握手 %dms）", nodeLabel(ch.n), ch.took.Milliseconds())
	default:
		sn.logf("[relay] 已选择节点 %s（握手 %dms）", nodeLabel(ch.n), ch.took.Milliseconds())
	}
	return nil
}

// choose 按 planShare 的计划拨号（allow 决定哪些节点能拨），返回第一个连上的节点，不交给 Client。
// prefer-default 先单独试默认节点，连不上再让其余候选同时起跑；fastest 所有候选按权重错开起跑。
// 连不上的节点进退避。
func (sn *shareNodes) choose(v *directory.Verified, cache *directory.Cache, allow func(prefix string) bool) (*chosenNode, error) {
	plan := planShare(v, allow)
	var errs nodeErrors
	var defaultFailed *directory.Node
	if f := plan.First; f != nil {
		start := time.Now()
		r, err := sn.dial(context.Background(), *f, "", cache, v)
		if err == nil {
			return &chosenNode{r: r, n: *f, took: time.Since(start)}, nil
		}
		sn.backoff.Fail(f.Prefix)
		errs = append(errs, nodeError(*f, err))
		defaultFailed = f
	}
	if len(plan.Racers) > 0 {
		ch, raceErrs := sn.race(plan.Racers, cache, v)
		if ch != nil {
			ch.defaultFailed = defaultFailed
			return ch, nil
		}
		errs = append(errs, raceErrs...)
	}
	if len(errs) == 0 {
		return nil, errNoNodes
	}
	return nil, errs
}

// raceResult 是并行建连里一路的结果。
type raceResult struct {
	r    *nodeConn
	n    directory.Node
	took time.Duration
	err  error
}

// race 让 racers 按各自的延迟起跑、并行建连，第一个连上的胜出，其余取消；取消前刚好连上的由 closeLateNodes 关掉
// （relay 那边是握手后零消息断开，不计分，spec 6.9）。失败的节点进退避。返回胜者，或者全部失败时各节点的错误。
func (sn *shareNodes) race(racers []shareRacer, cache *directory.Cache, v *directory.Verified) (*chosenNode, []error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan raceResult, len(racers))
	for _, rc := range racers {
		go func() {
			if rc.Delay > 0 {
				t := time.NewTimer(rc.Delay)
				defer t.Stop()
				select {
				case <-t.C:
				case <-ctx.Done():
					results <- raceResult{n: rc.Node, err: ctx.Err()}
					return
				}
			}
			start := time.Now()
			r, err := sn.dial(ctx, rc.Node, "", cache, v)
			results <- raceResult{r: r, n: rc.Node, took: time.Since(start), err: err}
		}()
	}
	var errs []error
	for i := range racers {
		res := <-results
		if res.err == nil {
			cancel()
			go closeLateNodes(results, len(racers)-i-1)
			return &chosenNode{r: res.r, n: res.n, took: res.took}, nil
		}
		sn.backoff.Fail(res.n.Prefix)
		errs = append(errs, nodeError(res.n, res.err))
	}
	return nil, errs
}

// closeLateNodes 收齐竞速里还没返回的 pending 路，连上了的关掉：胜者出来之后、取消生效之前刚好握完手的。
func closeLateNodes(results <-chan raceResult, pending int) {
	for range pending {
		if res := <-results; res.r != nil {
			abortConn(res.r.Conn)
		}
	}
}

// dial 拨节点 n（first 非空时最先试它，用于 --relay-hint），见 dialListedNode。可以在多个 goroutine 里同时调用。
func (sn *shareNodes) dial(ctx context.Context, n directory.Node, first string, cache *directory.Cache, v *directory.Verified) (*nodeConn, error) {
	return dialListedNode(ctx, sn.dm, sn.transport, sn.logf, n, first, cache, v)
}

// dialListedNode 拨列表里的节点 n（first 非空时最先试它），并处理途中失败的地址：连不上的本机专用地址
// 从缓存删掉（spec 6.5），证书与列表不符的列表地址打警告（spec 6.3）。被 relay 封禁不算连不上：地址没坏，
// 被封的是本机，删了的话解封后还得再用 --server 引导一次。share 与 help 共用。
func dialListedNode(ctx context.Context, dm *dirManager, transport string, logf func(format string, args ...any),
	n directory.Node, first string, cache *directory.Cache, v *directory.Verified) (*nodeConn, error) {
	local := cache.LocalAddr(v, n.Prefix)
	dialFirst := local
	if first != "" {
		dialFirst = first
	}
	r, err := dialNode(ctx, n, dialFirst, transport, "")
	var failed []addrFailure
	var de *nodeDialError
	if err == nil {
		failed = r.Failed
	} else if errors.As(err, &de) {
		failed = de.Failures
	}
	for _, f := range failed {
		var mm *CertMismatchError
		switch {
		case errors.Is(f.Err, proto.ErrBlocked):
			// 封禁由调用方报给用户（「访问受限」），这里什么都不删。
		case local != "" && f.Addr == local:
			dm.DropLocalAddr(n.Prefix, local, f.Err)
		case errors.As(f.Err, &mm):
			logf("[relay] 警告：节点 %s 的地址 %s 出示的证书与节点列表不符（指纹 %s…），可能被劫持，已跳过", nodeLabel(n), f.Addr, shortFP(mm.FP))
		}
	}
	return r, err
}

// shortFP 是提示里指纹的写法：前 16 个 hex，够人工核对。
func shortFP(fp string) string {
	if len(fp) > 16 {
		return fp[:16]
	}
	return fp
}

// adopt 把连上的节点交给 c，记为当前节点。
func (sn *shareNodes) adopt(c *Client, n directory.Node, r *nodeConn) {
	c.adoptNode(r)
	sn.setCurrent(&n)
}

// migrate 在等协助端时调用（spec 6.3 排空迁移）：当前节点被列表标成排空、停用、已不在列表里，或者 relayDraining
// （最近一次注册响应说它在排空）时，拨别的节点，连上了就记为 pending，由下一次 connect 接管，返回 true。
// 连不上别的节点就留在原节点（它照样服务已注册的 share），返回 false，下次再试；这期间只提示一次。
//
// peerFP 是当前连接上 relay 出示的证书指纹。不在列表节点上时（第一次运行时在默认地址上注册、列表随注册响应才下发，
// 或者列表失效时回到了默认地址）靠它认出所在节点；不按地址认，理由同 sameCodeNode。认不出（这台 relay 不在列表里）
// 就不迁移，与没有这个功能时一样。
func (sn *shareNodes) migrate(relayDraining bool, peerFP string) bool {
	v, cache := sn.dm.Current()
	if v == nil {
		return false // 没有列表就没有别的节点可选
	}
	cur := sn.current
	if cur == nil {
		if cur = v.NodeByFingerprint(peerFP); cur == nil {
			return false
		}
	}
	reason := leaveReason(v.NodeByPrefix(cur.Prefix))
	if reason == "" && relayDraining {
		reason = "正在排空"
	}
	if reason == "" {
		sn.migrateNoted = false
		return false
	}
	allow := func(prefix string) bool { return prefix != cur.Prefix && sn.backoff.Allowed(prefix) }
	ch, err := sn.choose(v, cache, allow)
	if err != nil {
		if !sn.migrateNoted {
			sn.migrateNoted = true
			sn.logf("[relay] 当前节点 %s %s，但暂时连不上其他节点（%v），先留在当前节点", nodeLabel(*cur), reason, err)
		}
		return false
	}
	sn.migrateNoted = false
	sn.logf("[relay] 当前节点 %s %s，已改用节点 %s（握手 %dms），协助码将会更换", nodeLabel(*cur), reason, nodeLabel(ch.n), ch.took.Milliseconds())
	sn.pending = ch
	return true
}

// nodeByAddr 找地址是 addr 的节点：列表里的地址，或缓存里记的本机专用地址（主机名不分大小写）。
// 本机专用地址恰好是别的节点的列表地址时以列表为准：列表签过名，本机专用地址只是本机记下的。
func nodeByAddr(v *directory.Verified, cache *directory.Cache, addr string) *directory.Node {
	if n, _, ok := listedTarget(v, addr); ok {
		return n
	}
	return localAddrNode(v, cache, addr)
}

// listedTarget 找 addr 是哪个节点在列表里的地址（TCP、QUIC，主机名不分大小写），t 是这个地址声明过的传输。
func listedTarget(v *directory.Verified, addr string) (n *directory.Node, t nodeTarget, ok bool) {
	for i := range v.Nodes {
		for _, t := range nodeDialOrder(v.Nodes[i], "") {
			if strings.EqualFold(t.Addr, addr) {
				return &v.Nodes[i], t, true
			}
		}
	}
	return nil, nodeTarget{}, false
}

// localAddrNode 找 addr 是哪个节点记下的本机专用地址（主机名不分大小写）。
func localAddrNode(v *directory.Verified, cache *directory.Cache, addr string) *directory.Node {
	for i := range v.Nodes {
		if local := cache.LocalAddr(v, v.Nodes[i].Prefix); local != "" && strings.EqualFold(local, addr) {
			return &v.Nodes[i]
		}
	}
	return nil
}

// nodeError 给拨节点的错误加上节点名。
func nodeError(n directory.Node, err error) error {
	return fmt.Errorf("节点 %s: %w", nodeLabel(n), err)
}

// nodeErrors 是一次选节点里各节点的失败，一行写完（重连失败的提示是一行）。errors.Is 能认出其中任意一个。
type nodeErrors []error

func (e nodeErrors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "；")
}

func (e nodeErrors) Unwrap() []error { return e }
