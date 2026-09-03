package agent

import "sync"

// replayWindowBits 抗重放滑动窗口宽度（能记住的最近调用数）。
//
// Bridge 的调用 ID 是 atomic 自增的，正常只在 relay⇄P2P 热切换那一瞬会有少量乱序，
// 几个的量级。1024 给了三个数量级的余量：宁可多花 128 字节，也不要因为一次异常的
// 乱序就把合法调用误判成重放。
const replayWindowBits = 1024

const replayWindowWords = replayWindowBits / 64

// replayGuard 按调用 ID 做抗重放判定，语义与 IPsec 的滑动窗口一致。
//
// 为什么需要：ToolReq 的 ID 是明文，同一条密文改个 ID 重发即可让远端再执行一次
// （比如一条 exec）。AAD 把密文和 ID 绑死之后，改 ID 会解密失败，但**原样重放**
// 仍然成立——nonce 是发送方给的，接收方不做去重就无从识别。
//
// 窗口是「每把 key 一份」：重新握手会换 key（RotateKey），此时必须 reset，否则新
// 会话的 ID 会撞上旧会话残留的位。key 不变的 P2P 热升级不能 reset，那会让升级瞬间
// 在途的请求被当成重放打掉。
type replayGuard struct {
	mu      sync.Mutex
	started bool
	highest uint64
	bits    [replayWindowWords]uint64
}

// reset 清空窗口。换 key 时调用。
func (g *replayGuard) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.started = false
	g.highest = 0
	g.bits = [replayWindowWords]uint64{}
}

// accept 判定 id 是否是首次出现。返回 false 表示重放或已滑出窗口，调用方须拒绝该请求。
func (g *replayGuard) accept(id uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.started {
		g.started = true
		g.highest = id
		g.set(id)
		return true
	}

	switch {
	case id > g.highest:
		// 窗口前移：把跨过的槽位清干净，免得上一轮的位被当成"见过"。
		if id-g.highest >= replayWindowBits {
			g.bits = [replayWindowWords]uint64{}
		} else {
			for v := g.highest + 1; v <= id; v++ {
				g.clear(v)
			}
		}
		g.highest = id
		g.set(id)
		return true
	case g.highest-id >= replayWindowBits:
		return false // 太旧，已经滑出窗口，无从判断是否重放 —— 一律拒绝
	case g.isSet(id):
		return false // 见过
	default:
		g.set(id)
		return true // 迟到的乱序帧
	}
}

func (g *replayGuard) slot(id uint64) (word int, mask uint64) {
	pos := id % replayWindowBits
	return int(pos / 64), 1 << (pos % 64)
}

func (g *replayGuard) set(id uint64) {
	w, m := g.slot(id)
	g.bits[w] |= m
}

func (g *replayGuard) clear(id uint64) {
	w, m := g.slot(id)
	g.bits[w] &^= m
}

func (g *replayGuard) isSet(id uint64) bool {
	w, m := g.slot(id)
	return g.bits[w]&m != 0
}
