package client

import (
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
)

// 自动模式下选节点的纯逻辑（spec 6.3、6.4、6.9）：不碰网络、不读文件，输入是验过签的列表。
// 拨号、粘性、重选这些有状态的流程在调用方（share、help），这里只回答「该试谁、按什么顺序、什么时候起跑」。

// maxCandidates 是同一次选节点最多考虑的节点数（spec 6.9：列表里更多的按权重取前 8）。
const maxCandidates = 8

// routeNodeForCode 按协助码首位找签发它的节点（spec 6.4）。找不到、节点已停用、没有列表时返回 nil，
// 调用方据此决定是同步取一次列表还是直接连默认节点。排空的节点照常返回：它排空前发出的码还要能加入。
//
// 规范化与 relay 的 handleJoin 一致（去分隔符、strings.ToUpper），否则同一个码在两边会落到不同节点
// （例如 ſ 大写后是 S）。--no-auth 的固定码 proto.NoAuthCode 不是任何节点发的，首位碰巧是某个节点的前缀也不能路由。
//
// v 为 nil、首位是多字节字符（code[:1] 只是半个字符）都不用单独判断：NodeByPrefix 对 nil 安全，
// 而节点前缀只会是码字符集里的单个 ASCII 字符，半个字符找不到。
func routeNodeForCode(v *directory.Verified, code string) *directory.Node {
	if !codeRoutable(code) {
		return nil
	}
	code = strings.ToUpper(normalizeCode(code))
	n := v.NodeByPrefix(code[:1])
	if n == nil || n.Status == directory.StatusDisabled {
		return nil
	}
	return n
}

// codeRoutable 报告 code 能不能按首位找节点：规范化后非空，且不是 --no-auth 的固定码。
func codeRoutable(code string) bool {
	code = normalizeCode(code)
	return code != "" && code != proto.NoAuthCode
}

// eligibleForShare 报告节点能不能接收新的 share 注册：启用且权重大于 0。
func eligibleForShare(n directory.Node) bool {
	return n.Status == directory.StatusActive && n.Weight > 0
}

// shareCandidates 返回 share 现在可以拨的节点：合格（eligibleForShare）且 allow 放行（不在失败退避期内），
// 按权重从高到低（同权重按前缀），最多 maxCandidates 个。先过滤再取前 8：退避中的节点不占名额。
//
// prefer-default（含未知策略）下，默认节点只要能拨就一定在内：它要被先连，不能因为权重低被挤出前 8，
// 超过 8 个时顶掉排第 8 的。fastest 下所有候选一视同仁，只按权重取前 8（spec 6.3、6.9）。
func shareCandidates(v *directory.Verified, allow func(prefix string) bool) []directory.Node {
	if v == nil {
		return nil
	}
	var all []directory.Node
	for _, n := range v.Nodes {
		if eligibleForShare(n) && allow(n.Prefix) {
			all = append(all, n)
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Weight != all[j].Weight {
			return all[i].Weight > all[j].Weight
		}
		return all[i].Prefix < all[j].Prefix
	})
	if len(all) <= maxCandidates {
		return all
	}
	top := slices.Clone(all[:maxCandidates])
	isDefault := func(n directory.Node) bool { return n.Prefix == v.DefaultPrefix }
	if v.Policy != directory.PolicyFastest && v.DefaultPrefix != "" && !slices.ContainsFunc(top, isDefault) {
		if i := slices.IndexFunc(all[maxCandidates:], isDefault); i >= 0 {
			top[maxCandidates-1] = all[maxCandidates+i]
		}
	}
	return top
}

// shareRacer 是并行建连的一个节点，Delay 是它比别人晚多久起跑。
type shareRacer struct {
	Node  directory.Node
	Delay time.Duration
}

// sharePlan 是一次选节点的拨号计划。
//
// prefer-default：First 是默认节点（合格且不在退避期时），先单独试它，连上就用；连不上再让 Racers
// 里的其余候选同时起跑，第一个完成握手的直接用。fastest：没有 First，所有候选一起起跑，
// 按权重错开起跑时间。
type sharePlan struct {
	First  *directory.Node
	Racers []shareRacer
}

// planShare 生成拨号计划。allow 报告某个节点现在能不能拨（失败退避期内的不拨），由 shareCandidates 在取前 8 之前过滤。
func planShare(v *directory.Verified, allow func(prefix string) bool) sharePlan {
	var plan sharePlan
	for _, n := range shareCandidates(v, allow) { // v 为 nil 时没有候选，循环体不会执行
		if v.Policy == directory.PolicyFastest {
			plan.Racers = append(plan.Racers, shareRacer{Node: n, Delay: staggerDelay(n.Weight)})
			continue
		}
		if plan.First == nil && n.Prefix == v.DefaultPrefix {
			first := n
			plan.First = &first
			continue
		}
		plan.Racers = append(plan.Racers, shareRacer{Node: n})
	}
	return plan
}

// staggerDelay 是 fastest 策略下按权重错开的起跑延迟：(100 - weight) × 3ms，最多 300ms。
// 权重高的节点先起跑，RTT 接近时它胜出。
func staggerDelay(weight int) time.Duration {
	weight = min(max(weight, 0), 100)
	return time.Duration(100-weight) * 3 * time.Millisecond
}

// nodeTarget 是节点的一个拨号地址，以及这个地址上允许用哪些传输。
type nodeTarget struct {
	Addr string
	TCP  bool
	QUIC bool
}

// nodeDialOrder 排出节点的拨号顺序（spec 6.3）：本机专用地址最前，然后是列表里的 TCP 地址（按列表顺序），
// 最后是只在 QUIC 里列出的地址。同一个地址只出现一次（主机名不分大小写，写法取第一次出现的），
// 传输取并集（每个地址内部仍走 TCP/QUIC 竞速）。
// 本机专用地址是用户显式连上的地址，TCP 与 QUIC 都竞速过；节点自己没声明 QUIC 时，它只走 TCP。
func nodeDialOrder(n directory.Node, local string) []nodeTarget {
	var out []nodeTarget
	index := map[string]int{}
	add := func(addr string, tcp, quic bool) {
		key := strings.ToLower(addr)
		if i, ok := index[key]; ok {
			out[i].TCP = out[i].TCP || tcp
			out[i].QUIC = out[i].QUIC || quic
			return
		}
		index[key] = len(out)
		out = append(out, nodeTarget{Addr: addr, TCP: tcp, QUIC: quic})
	}
	if local != "" {
		add(local, true, len(n.QUIC) > 0)
	}
	for _, a := range n.TCP {
		add(a, true, false)
	}
	for _, a := range n.QUIC {
		add(a, false, true)
	}
	return out
}

// 失败退避（spec 6.9）：失败的节点 60 秒内不再拨，连续失败翻倍，最长 10 分钟；抖动只拉长（最多 +20%）、
// 总长封顶 10 分钟：既不会让退避短于「60 秒内不再拨」的下限，又能把一批客户端的重试错开。
const (
	backoffBase   = 60 * time.Second
	backoffMax    = 10 * time.Minute
	backoffJitter = 0.2
)

// nodeBackoff 记录各节点（按前缀）的连续失败次数和下次可拨的时间。
type nodeBackoff struct {
	mu     sync.Mutex
	now    func() time.Time
	jitter func() float64 // [0,1)
	state  map[string]backoffState
}

type backoffState struct {
	fails int
	until time.Time
}

func newNodeBackoff() *nodeBackoff {
	return &nodeBackoff{now: time.Now, jitter: rand.Float64, state: make(map[string]backoffState)}
}

// Fail 记一次失败，节点在退避期内不再被拨。
func (b *nodeBackoff) Fail(prefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[prefix]
	st.fails++
	d := backoffBase
	for i := 1; i < st.fails && d < backoffMax; i++ {
		d *= 2
	}
	d = min(time.Duration(float64(d)*(1+backoffJitter*b.jitter())), backoffMax)
	st.until = b.now().Add(d)
	b.state[prefix] = st
}

// Succeed 清掉节点的失败记录。
func (b *nodeBackoff) Succeed(prefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, prefix)
}

// Allowed 报告节点现在能不能拨。剩余等待超过最长退避说明时钟被往回调过（真实时钟带单调读数，
// 一般不会遇到），放行，免得节点被封到时钟追上为止。
func (b *nodeBackoff) Allowed(prefix string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.state[prefix]
	if !ok {
		return true
	}
	remaining := st.until.Sub(b.now())
	return remaining <= 0 || remaining > backoffMax
}
