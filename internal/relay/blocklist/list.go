// Package blocklist 实现 relay 内置的 IP 黑名单：按异常行为计分自动封禁、
// 递增封禁时长、白名单、解禁宽限期、每 IP 识别信息，以及原子 JSON 持久化。
//
// 计分事件见 Kind 与 kindWeight，持久化格式见 store.go。
// 本包不知道 relay 的连接/协议细节：调用方在合适的位置上报事件（RecordViolation /
// ObserveConnect / ObserveClient），并在 accept 之后用 CheckAccept 做准入。
//
// 内存结构（全部有界）：
//   - entries：主表，键为 IPv4 地址或 IPv6 /64 前缀。
//   - 可淘汰条目挂在两条 LRU 上：idle（窗口内无分值）与 scoring（正在计分）。总数 ≤ MaxTracked，
//     超出时 O(1) 淘汰 idle 最旧，其次 scoring 最旧。
//   - 钉住条目（生效封禁 / 白名单 / 宽限期）不在 LRU 上；自动封禁另有上限：IPv4 与 IPv6 各自最多
//     MaxAutoBans 条，用按到期时间的小顶堆淘汰最早到期者；同一 IPv6 /48 下的自动封禁另有子配额，
//     超出时只在该 /48 内部淘汰，挤不掉别人的封禁。
//   - mem：独立的有界 LRU（IPv4 / IPv6 各一份），只存每个键的递增次数、最近一次封禁到期时间、
//     最近一次成功加入时间。"只剩递增记忆"的到期封禁不再占主表。
package blocklist

import (
	"container/heap"
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/remote-assist/tool/internal/logger"
)

// Kind 是计分事件类型。只有异常行为才有对应 Kind：正常连接、重连、ClientID 冲突、
// 限流拒绝一律不上报。
type Kind string

const (
	// KindTLSGarbage 对端发来的不是 TLS（RecordHeaderError）或是畸形 TLS 记录。
	KindTLSGarbage Kind = "tls_garbage"
	// KindTLSAborted 对端主动中止握手：TLS alert（证书校验失败、时钟错）、EOF、超时、重置。
	// 正常客户端换证书/TOFU 不匹配、TCP 健康检查都落在这里，只记事件不计分。
	KindTLSAborted           Kind = "tls_aborted"
	KindProtoDecode          Kind = "proto_decode"
	KindProtoState           Kind = "proto_state"
	KindUnknownMessage       Kind = "unknown_message"
	KindJoinFailed           Kind = "join_failed"
	KindJoinFailuresExceeded Kind = "join_failures_exceeded"
)

// kindWeight 各类事件的分值；0 分只记事件。
var kindWeight = map[Kind]int{
	KindTLSGarbage:           1,
	KindTLSAborted:           0,
	KindProtoDecode:          2,
	KindProtoState:           2,
	KindUnknownMessage:       1,
	KindJoinFailed:           1,
	KindJoinFailuresExceeded: 5,
}

// exemptIfRecentGood 近 goodClientWindow 内在该键上有过成功注册/加入时不计分的事件：
// 握手类与未知消息最容易被版本差异、证书更换误触发。
var exemptIfRecentGood = map[Kind]bool{
	KindTLSGarbage:     true,
	KindTLSAborted:     true,
	KindUnknownMessage: true,
}

const (
	maxClientsPerIP    = 10
	maxClientField     = 64 // 客户端自报的版本/身份串保存时截断到这么多字节
	persistAutoClient  = 1  // 持久化时自动封禁条目只留最近 1 个客户端
	maxEventsPerIP     = 20
	maxHistoryPerIP    = 50
	maxHitsPerEntry    = 256
	historyRetention   = 30 * 24 * time.Hour
	goodClientWindow   = 7 * 24 * time.Hour
	maxNoteLen         = 200
	autoActor          = "auto"
	persistAutoEvents  = 1    // 持久化时自动封禁条目只留最近 1 条事件
	persistAutoHistory = 1    // 持久化时自动封禁条目只留最近 1 条处置历史
	maxPersistHistory  = 1000 // 非钉住、仅有人工处置历史的条目最多持久化这么多条
)

var (
	ErrInvalidIP       = errors.New("invalid ip")
	ErrInvalidDuration = errors.New("invalid duration")
	ErrNotBanned       = errors.New("ip is not banned")
	ErrNotAllowlisted  = errors.New("ip is not allowlisted")
	ErrCapacity        = errors.New("blocklist capacity exhausted")
)

// Config 黑名单配置。零值字段使用 DefaultConfig 的取值。
type Config struct {
	Path               string        // 持久化文件；空 = 纯内存
	Threshold          int           // 窗口内封禁分值阈值
	Window             time.Duration // 计分滑动窗口
	BanBase            time.Duration // 首次自动封禁时长
	BanMax             time.Duration // 自动封禁时长上限
	OffenseDecay       time.Duration // 最近一次自动封禁结束后多久无再犯即清零递增次数
	UnbanGrace         time.Duration // 手动解禁后的自动封禁宽限期
	MaxTracked         int           // 可淘汰（观察/计分）条目上限
	MaxAutoBans        int           // 每个地址族（IPv4 / IPv6）同时生效的自动封禁上限，默认 = MaxTracked
	MaxMemory          int           // 每个地址族的递增/成功客户端记忆上限，默认 = 4×MaxTracked
	SaveInterval       time.Duration // 普通变更最小写盘间隔，默认 5s
	UrgentSaveInterval time.Duration // 人工处置最小写盘间隔，默认 1s
	Now                func() time.Time
	// Records 封禁记录（可选，可由多个名单共用）。Open 之前打开；名单不负责关闭它，要在名单关闭之后再关。
	Records *Records
	// StaticAllow 启动参数 --blocklist-allow 的放行范围（见 static_allow.go）：命中的来源不计分、不被拒绝、不能手动封禁。
	StaticAllow []netip.Prefix
	// retryBackoff 写盘失败后的重试间隔（逐次递增，最后一个封顶），空 = defaultRetryBackoff。
	// 只能在 Open 之前设置（测试用）：saver 协程在 Open 里启动，之后再改会与它的读取竞争。
	retryBackoff []time.Duration
}

// MaxTrackedLimit MaxTracked 的上限。按此上限、两族自动封禁全满的正常数据估算持久化文件约 22 MB，
// 在写盘预算（24 MiB）与加载上限（32 MiB）之内；更大的值会让预算裁剪成为常态。
const MaxTrackedLimit = 8192

// DefaultConfig 返回生产默认值。
func DefaultConfig() Config {
	return Config{
		Threshold:          20,
		Window:             10 * time.Minute,
		BanBase:            time.Hour,
		BanMax:             24 * time.Hour,
		OffenseDecay:       7 * 24 * time.Hour,
		UnbanGrace:         24 * time.Hour,
		MaxTracked:         4096,
		SaveInterval:       5 * time.Second,
		UrgentSaveInterval: time.Second,
		Now:                time.Now,
	}
}

func normalizeConfig(cfg Config) Config {
	d := DefaultConfig()
	if cfg.Threshold <= 0 {
		cfg.Threshold = d.Threshold
	}
	if cfg.Window <= 0 {
		cfg.Window = d.Window
	}
	if cfg.BanBase <= 0 {
		cfg.BanBase = d.BanBase
	}
	if cfg.BanMax <= 0 {
		cfg.BanMax = d.BanMax
	}
	if cfg.BanMax < cfg.BanBase {
		cfg.BanMax = cfg.BanBase
	}
	if cfg.OffenseDecay <= 0 {
		cfg.OffenseDecay = d.OffenseDecay
	}
	if cfg.UnbanGrace <= 0 {
		cfg.UnbanGrace = d.UnbanGrace
	}
	if cfg.MaxTracked <= 0 {
		cfg.MaxTracked = d.MaxTracked
	}
	if cfg.MaxTracked > MaxTrackedLimit {
		cfg.MaxTracked = MaxTrackedLimit
	}
	if cfg.MaxAutoBans <= 0 {
		cfg.MaxAutoBans = cfg.MaxTracked
	}
	if cfg.MaxMemory <= 0 {
		cfg.MaxMemory = 4 * cfg.MaxTracked
	}
	if cfg.SaveInterval <= 0 {
		cfg.SaveInterval = d.SaveInterval
	}
	if cfg.UrgentSaveInterval <= 0 {
		cfg.UrgentSaveInterval = d.UrgentSaveInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg
}

// ---------- 内部状态（同时也是持久化格式） ----------

type hitRec struct {
	ts     int64
	weight int
}

type banRec struct {
	Since     int64  `json:"since"`
	Until     int64  `json:"until"` // Permanent 时为 0
	Permanent bool   `json:"permanent"`
	Reason    string `json:"reason"` // auto | manual
	Actor     string `json:"actor"`
	Note      string `json:"note"`
	Kind      string `json:"kind,omitempty"` // 自动封禁时触发的最后一类事件
}

func (b *banRec) active(now int64) bool {
	return b != nil && (b.Permanent || b.Until > now)
}

type allowRec struct {
	Since     int64  `json:"since"`
	Until     int64  `json:"until"`
	Permanent bool   `json:"permanent"`
	Actor     string `json:"actor"`
	Note      string `json:"note"`
}

func (a *allowRec) active(now int64) bool {
	return a != nil && (a.Permanent || a.Until > now)
}

type clientRec struct {
	Role      string `json:"role"`
	Version   string `json:"version"`
	Host      string `json:"host"`
	ClientFP  string `json:"client_fp"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	Count     int64  `json:"count"`
	Verified  bool   `json:"verified,omitempty"` // 成功加入（持有效协助码）或注册后完成过配对
}

type eventRec struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Points int    `json:"points"` // 本次实际计入的分值；0 = 只记录（豁免、白名单、宽限期或已封禁）
}

type historyRec struct {
	TS     int64  `json:"ts"`
	Action string `json:"action"` // ban | unban | allow | disallow | ban_expired | allow_expired | ban_evicted
	Actor  string `json:"actor"`
	Note   string `json:"note"`
	Detail string `json:"detail"`
}

type entry struct {
	IP          string       `json:"ip"` // 键：IPv4 地址或 IPv6 /64 前缀
	FirstSeen   int64        `json:"first_seen"`
	LastSeen    int64        `json:"last_seen"`
	Connections int64        `json:"connections"`
	Rejected    int64        `json:"rejected"` // 封禁期间在 accept 处被拒的连接数；在读锁下原子递增
	Clients     []clientRec  `json:"clients"`
	Events      []eventRec   `json:"events"`
	History     []historyRec `json:"history"`
	Ban         *banRec      `json:"ban,omitempty"`
	Allow       *allowRec    `json:"allow,omitempty"`
	GraceUntil  int64        `json:"grace_until"`
	BanCount    int          `json:"ban_count"` // 累计被封次数（含手动）

	hits    []hitRec      // 窗口内命中，不持久化
	lru     *list.List    // 所在 LRU（钉住时为 nil）
	elem    *list.Element // 在 lru 中的位置
	heapIdx int           // 在自动封禁堆中的下标，-1 = 不在堆中
}

func (e *entry) pinned(now int64) bool {
	return e.Ban.active(now) || e.Allow.active(now) || e.GraceUntil > now
}

func (e *entry) points(now int64, window time.Duration) int {
	cutoff := now - int64(window/time.Second)
	sum := 0
	for _, h := range e.hits {
		if h.ts > cutoff {
			sum += h.weight
		}
	}
	return sum
}

func (e *entry) pruneHits(now int64, window time.Duration) {
	cutoff := now - int64(window/time.Second)
	kept := e.hits[:0]
	for _, h := range e.hits {
		if h.ts > cutoff {
			kept = append(kept, h)
		}
	}
	if len(kept) == 0 {
		kept = nil
	}
	e.hits = kept
}

// addHistory 追加一条处置历史；满 maxHistoryPerIP 时就地左移丢掉最旧的，不再每次复制整切片。
func (e *entry) addHistory(h historyRec) {
	if n := len(e.History); n >= maxHistoryPerIP {
		copy(e.History, e.History[n-maxHistoryPerIP+1:])
		e.History = e.History[:maxHistoryPerIP-1]
	}
	e.History = append(e.History, h)
}

// addEvent 追加一条违规事件，满 maxEventsPerIP 时就地左移（不分配）。
func (e *entry) addEvent(ev eventRec) {
	if n := len(e.Events); n >= maxEventsPerIP {
		copy(e.Events, e.Events[n-maxEventsPerIP+1:])
		e.Events = e.Events[:maxEventsPerIP-1]
	}
	e.Events = append(e.Events, ev)
}

// addHit 追加一次计分命中，满 maxHitsPerEntry 时就地左移（不分配）。
func (e *entry) addHit(h hitRec) {
	if n := len(e.hits); n >= maxHitsPerEntry {
		copy(e.hits, e.hits[n-maxHitsPerEntry+1:])
		e.hits = e.hits[:maxHitsPerEntry-1]
	}
	e.hits = append(e.hits, h)
}

// lastManualTS 最近一次人工处置的时间（0 = 没有）。
func (e *entry) lastManualTS() int64 {
	for i := len(e.History) - 1; i >= 0; i-- {
		if e.History[i].Actor != autoActor {
			return e.History[i].TS
		}
	}
	return 0
}

// ---------- 自动封禁小顶堆（按到期时间） ----------

type autoBanHeap []*entry

func (h autoBanHeap) Len() int           { return len(h) }
func (h autoBanHeap) Less(i, j int) bool { return h[i].Ban.Until < h[j].Ban.Until }
func (h autoBanHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}
func (h *autoBanHeap) Push(x any) {
	e := x.(*entry)
	e.heapIdx = len(*h)
	*h = append(*h, e)
}
func (h *autoBanHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	e.heapIdx = -1
	return e
}

// ---------- 递增 / 成功客户端记忆（有界 LRU） ----------

type memRec struct {
	Key        string `json:"key"`
	Offenses   int    `json:"offenses"`
	LastBanEnd int64  `json:"last_ban_end"`
	LastGood   int64  `json:"last_good"`
}

type memLRU struct {
	cap int
	ll  *list.List
	m   map[string]*list.Element
}

func newMemLRU(capacity int) *memLRU {
	return &memLRU{cap: capacity, ll: list.New(), m: make(map[string]*list.Element)}
}

func (c *memLRU) get(key string) *memRec {
	if el := c.m[key]; el != nil {
		return el.Value.(*memRec)
	}
	return nil
}

// touch 取得或创建记录并标为最近使用；超出容量淘汰最久未用的。
func (c *memLRU) touch(key string) *memRec {
	if el := c.m[key]; el != nil {
		c.ll.MoveToBack(el)
		return el.Value.(*memRec)
	}
	r := &memRec{Key: key}
	c.m[key] = c.ll.PushBack(r)
	for c.ll.Len() > c.cap {
		front := c.ll.Front()
		delete(c.m, front.Value.(*memRec).Key)
		c.ll.Remove(front)
	}
	return r
}

func (c *memLRU) del(key string) {
	if el := c.m[key]; el != nil {
		delete(c.m, key)
		c.ll.Remove(el)
	}
}

// 地址族：IPv4 与 IPv6 的自动封禁配额、记忆配额分开计，IPv6 一侧的海量前缀挤不掉 IPv4 的状态。
const (
	famV4 = 0
	famV6 = 1
)

func familyOf(key string) int {
	if strings.Contains(key, ":") {
		return famV6
	}
	return famV4
}

// ---------- List ----------

type auditRec struct {
	level   logger.AuditLevel
	event   string
	message string
	details map[string]interface{}
}

// List 是并发安全的黑名单。accept 热路径（CheckAccept）只取读锁。
type List struct {
	cfg  Config
	salt []byte

	mu      sync.RWMutex
	entries map[string]*entry
	idle    *list.List                     // 可淘汰、窗口内无分值
	scoring *list.List                     // 可淘汰、正在计分
	bans    [2]autoBanHeap                 // 按地址族
	mem     [2]*memLRU                     // 按地址族
	per48   map[string]map[*entry]struct{} // IPv6 /48 -> 该 /48 下生效的自动封禁

	untracked atomic.Uint64 // 主表满（全是钉住条目）时未能跟踪的事件数

	onManualBan atomic.Value // func(key string)

	// dirty 标记"已持久化状态上有新的观察数据"（连接、客户端、事件、记忆）。这类变化频繁且不紧急，
	// 不逐次写盘，由每分钟的 Sweep 合并成一次异步写。
	dirty atomic.Bool

	// 持久化：path 非空且打开成功时 persistent=true。写盘由后台 saver 合并执行。
	persistent bool
	path       string
	lock       *fileLock
	saveMu     sync.Mutex
	saveCh     chan struct{}
	urgent     atomic.Bool
	stopCh     chan struct{}
	doneCh     chan struct{}
	closeOnce  sync.Once
	saveCount  atomic.Int64

	saveBudget     int                          // 写盘字节预算；0 = maxSaveBytes（测试可调小）
	lastSaveReport atomic.Pointer[budgetReport] // 最近一次写盘的预算裁剪情况

	saveFailures atomic.Int64 // 累计写盘失败次数
	snapshotHold atomic.Int64 // 最近一次写盘快照单次持读锁的最长时长（纳秒，观测与测试用）

	autoBanEvictedHook func(key string) // 测试钩子：自动封禁被 ban_evicted 时调用（持锁）
}

// Open 创建黑名单。cfg.Path 非空时从文件加载并持久化。
//
// 打开失败（锁被占用、文件损坏、过大、权限过宽）时返回一个**可用的纯内存 List** 和错误：
// 调用方记录错误后照常使用，拒绝与自动封禁仍然生效，原文件不会被覆盖。
func Open(cfg Config) (*List, error) {
	cfg = normalizeConfig(cfg)
	l := &List{
		cfg:     cfg,
		entries: make(map[string]*entry),
		idle:    list.New(),
		scoring: list.New(),
		mem:     [2]*memLRU{newMemLRU(cfg.MaxMemory), newMemLRU(cfg.MaxMemory)},
		per48:   make(map[string]map[*entry]struct{}),
	}
	if cfg.Path == "" {
		l.salt = newSalt()
		l.reconcileRecords()
		return l, nil
	}
	lock, data, err := openStore(cfg.Path)
	if err != nil {
		l.salt = newSalt()
		// 名单没加载成功时不对账：否则记录里仍生效的封禁会被当成丢失结束掉，而修好名单文件重启后它们会回来。
		return l, err
	}
	l.lock = lock
	l.path = cfg.Path
	l.persistent = true
	if data == nil {
		l.salt = newSalt()
	} else {
		l.salt = data.salt
		l.loadLocked(data)
	}
	l.saveCh = make(chan struct{}, 1)
	l.stopCh = make(chan struct{})
	l.doneCh = make(chan struct{})
	go l.saver()
	// 加载后先清扫一遍（停机期间到期的封禁/白名单，到期记录随之结束），再与封禁记录对账，并确保文件存在。
	l.Sweep()
	l.releaseStaticAllowed() // 在对账之前：放行解除的封禁，记录要以「人工解禁」结束，而不是被当成重启丢失
	l.reconcileRecords()
	l.requestSave(true)
	return l, nil
}

// loadLocked 装入文件内容，并按**当前**上限裁剪：以大上限运行过再改小重启时，超出部分的
// 自动封禁（保留最晚到期的）被提前解除、记忆按 LRU 截断、可淘汰条目按 LRU 淘汰。手动封禁、
// 白名单、宽限期一律不裁。
func (l *List) loadLocked(data *loadedData) {
	now := l.now()
	for _, r := range data.mem {
		*l.memOf(r.Key).touch(r.Key) = *r
	}
	var autos []*entry
	for _, e := range data.entries {
		e.heapIdx = -1
		e.lru, e.elem = nil, nil
		e.hits = nil
		for i := range e.Clients {
			e.Clients[i].Host = cleanField(e.Clients[i].Host)
			e.Clients[i].Version = cleanField(e.Clients[i].Version)
		}
		l.entries[e.IP] = e
		if e.Ban.active(now) && e.Ban.Reason == "auto" {
			autos = append(autos, e)
		}
	}
	// 最晚到期的优先保留：按到期时间倒序逐个入堆，超出族配额或 /48 子配额的直接解除。
	sort.Slice(autos, func(i, j int) bool { return autos[i].Ban.Until > autos[j].Ban.Until })
	trimmed := 0
	for _, e := range autos {
		fam := familyOf(e.IP)
		if l.bans[fam].Len() >= l.autoBanCap() || (fam == famV6 && len(l.per48[prefix48(e.IP)]) >= l.per48Cap()) {
			l.recordBanEnd(e, EndEvicted, now, autoActor)
			e.Ban = nil
			// 与运行时 dropAutoBan 一致：被提前解除的封禁清零递增次数，免得下次直接跳到长时长。
			if r := l.memOf(e.IP).get(e.IP); r != nil {
				r.Offenses, r.LastBanEnd = 0, 0
			}
			e.addHistory(historyRec{TS: now, Action: "ban_evicted", Actor: autoActor, Detail: "加载时超出当前自动封禁上限，被提前解除"})
			trimmed++
			continue
		}
		l.pushAutoBan(e)
	}
	for _, e := range data.entries {
		l.place(e, now, false)
	}
	if trimmed > 0 {
		log.Printf("blocklist: %d auto ban(s) exceeded the current cap on load and were released early", trimmed)
	}
	if n := len(l.entries); n > l.hardCap() {
		log.Printf("blocklist: %d tracked keys exceed the hard cap %d after load (manual bans/allowlist are never trimmed); new keys will not be tracked until some expire or are removed", n, l.hardCap())
	}
}

func newSalt() []byte {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return salt
}

// Persistent 报告是否在持久化模式。
func (l *List) Persistent() bool { return l != nil && l.persistent }

// Config 返回生效配置（只读副本）。
func (l *List) Config() Config { return l.cfg }

// SaveCount 返回累计写盘次数（观测与测试用）。
func (l *List) SaveCount() int64 { return l.saveCount.Load() }

// Close 停止后台 saver，执行最后一次写盘并释放文件锁。
func (l *List) Close() error {
	if l == nil {
		return nil
	}
	var err error
	l.closeOnce.Do(func() {
		if !l.persistent {
			return
		}
		close(l.stopCh)
		<-l.doneCh
		err = l.saveNow()
		if l.lock != nil {
			_ = l.lock.Close()
		}
	})
	return err
}

// SetOnManualBan 注册手动封禁回调（relay 用来断开该键下的现有连接）。回调在锁外调用，参数是键。
func (l *List) SetOnManualBan(fn func(key string)) {
	l.onManualBan.Store(fn)
}

func (l *List) now() int64 { return l.cfg.Now().Unix() }

// fingerprint 用随文件持久化的 salt 计算 ClientID 的不可逆指纹，跨重启稳定。
func (l *List) fingerprint(v string) string {
	if v == "" {
		return ""
	}
	h := hmac.New(sha256.New, l.salt)
	_, _ = h.Write([]byte(v))
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func (l *List) emit(audits []auditRec) {
	for _, a := range audits {
		logger.Log(a.level, a.event, a.message, a.details)
	}
}

// hardCap 主表条目硬上限：可淘汰条目 + 两族自动封禁 + 人工钉住（白名单/手动封禁/宽限期）。
func (l *List) hardCap() int {
	return 2*l.cfg.MaxTracked + 2*l.cfg.MaxAutoBans
}

// autoBanCap 每个地址族同时生效的自动封禁上限。
func (l *List) autoBanCap() int { return l.cfg.MaxAutoBans }

// per48Cap 同一 IPv6 /48 下同时生效的自动封禁上限：持有整个 /48（65536 个 /64）的来源最多占这么多，
// 超出时只在它自己的 /48 内淘汰，挤不掉其他来源的封禁。
func (l *List) per48Cap() int {
	if n := l.cfg.MaxAutoBans / 16; n > 4 {
		return n
	}
	return 4
}

func (l *List) memOf(key string) *memLRU { return l.mem[familyOf(key)] }

func (l *List) banHeaps() map[int]*autoBanHeap {
	return map[int]*autoBanHeap{famV4: &l.bans[famV4], famV6: &l.bans[famV6]}
}

func (l *List) mems() []*memLRU { return l.mem[:] }

// pushAutoBan 把一条生效的自动封禁放入所属族的堆（以及 /48 索引）。
func (l *List) pushAutoBan(e *entry) {
	heap.Push(&l.bans[familyOf(e.IP)], e)
	if familyOf(e.IP) == famV6 {
		p := prefix48(e.IP)
		set := l.per48[p]
		if set == nil {
			set = make(map[*entry]struct{})
			l.per48[p] = set
		}
		set[e] = struct{}{}
	}
}

// removeAutoBan 把条目移出堆与 /48 索引（不改 e.Ban）。
func (l *List) removeAutoBan(e *entry) {
	if e.heapIdx < 0 {
		return
	}
	heap.Remove(&l.bans[familyOf(e.IP)], e.heapIdx)
	if familyOf(e.IP) == famV6 {
		p := prefix48(e.IP)
		if set := l.per48[p]; set != nil {
			delete(set, e)
			if len(set) == 0 {
				delete(l.per48, p)
			}
		}
	}
}

// touch 取得或创建条目。主表已到硬上限（只可能是人工钉住条目过多）时返回 nil，
// 不再为新键建条目——也不做任何扫描。调用方必须持有写锁，并在修改后调用 place。
func (l *List) touch(key string, now int64) *entry {
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= l.hardCap() {
			// 先尝试腾出一个可淘汰位置（O(1)），仍不行就放弃跟踪。
			if !l.evictOne(nil, now) {
				return nil
			}
		}
		e = &entry{IP: key, FirstSeen: now, LastSeen: now, heapIdx: -1}
		l.entries[key] = e
	}
	return e
}

// place 按当前状态把条目放进正确位置：钉住条目移出 LRU；其他条目放进 idle/scoring，
// recent=true 时视为刚被访问（移到队尾）。可淘汰条目超出 MaxTracked 时 O(1) 淘汰。
func (l *List) place(e *entry, now int64, recent bool) {
	if e.pinned(now) {
		if e.elem != nil {
			e.lru.Remove(e.elem)
			e.elem, e.lru = nil, nil
		}
		return
	}
	target := l.idle
	if len(e.hits) > 0 {
		target = l.scoring
	}
	switch {
	case e.elem != nil && e.lru == target:
		if recent {
			target.MoveToBack(e.elem)
		}
		return
	case e.elem != nil:
		e.lru.Remove(e.elem)
	}
	e.elem, e.lru = target.PushBack(e), target
	for l.idle.Len()+l.scoring.Len() > l.cfg.MaxTracked {
		if !l.evictOne(e, now) {
			break
		}
	}
}

// evictOne 淘汰一个可淘汰条目：先 idle 最旧，再 scoring 最旧；永不淘汰 keep。O(1)。
//
// 防御：LRU 上本不该出现钉住条目或仍在自动封禁堆里的条目（那会让主表删掉一条生效封禁、
// 在堆里留下孤儿）。万一出现，只把它摘下 LRU，绝不从主表删除。
func (l *List) evictOne(keep *entry, now int64) bool {
	for _, ll := range []*list.List{l.idle, l.scoring} {
		for el := ll.Front(); el != nil; {
			victim := el.Value.(*entry)
			next := el.Next()
			if victim == keep {
				el = next
				continue
			}
			ll.Remove(el)
			victim.elem, victim.lru = nil, nil
			if victim.heapIdx >= 0 || victim.pinned(now) {
				logInvariantOnce("evictOne: pinned entry found on an LRU; detached instead of deleting")
				el = next
				continue
			}
			delete(l.entries, victim.IP)
			return true
		}
	}
	return false
}

var invariantLogged atomic.Bool

func logInvariantOnce(msg string) {
	if invariantLogged.CompareAndSwap(false, true) {
		log.Printf("blocklist: internal invariant violated (%s)", msg)
	}
}

// refresh 处理已到期的封禁/白名单/宽限期并裁剪窗口外命中，返回审计记录与是否有状态变化。
// 调用方持有写锁，随后需 place。
func (l *List) refresh(e *entry, now int64) ([]auditRec, bool) {
	var audits []auditRec
	changed := false
	if e.Ban != nil && !e.Ban.active(now) {
		l.removeAutoBan(e)
		e.addHistory(historyRec{TS: now, Action: "ban_expired", Actor: autoActor, Detail: reasonText(e.Ban.Reason) + "封禁到期自动解除"})
		audits = append(audits, auditRec{logger.AuditLevelInfo, "ip_ban_expired", fmt.Sprintf("IP %s %s封禁到期解除", e.IP, reasonText(e.Ban.Reason)), map[string]interface{}{"ip": e.IP, "actor": autoActor, "reason": e.Ban.Reason}})
		// 记录的结束时间取计划到期时间，而不是被惰性发现的这一刻。
		l.recordBanEnd(e, EndExpired, e.Ban.Until, autoActor)
		e.Ban = nil
		changed = true
	}
	if e.Allow != nil && !e.Allow.active(now) {
		e.addHistory(historyRec{TS: now, Action: "allow_expired", Actor: autoActor, Detail: "白名单到期自动移除"})
		audits = append(audits, auditRec{logger.AuditLevelInfo, "ip_allow_expired", fmt.Sprintf("IP %s 白名单到期移除", e.IP), map[string]interface{}{"ip": e.IP, "actor": autoActor}})
		e.Allow = nil
		changed = true
	}
	if e.GraceUntil != 0 && e.GraceUntil <= now {
		e.GraceUntil = 0
		changed = true
	}
	e.pruneHits(now, l.cfg.Window)
	return audits, changed
}

func (l *List) recentGood(key string, now int64) bool {
	r := l.memOf(key).get(key)
	return r != nil && r.LastGood > now-int64(goodClientWindow/time.Second)
}

// CheckAccept 是 accept 热路径：被封返回 false 并计一次被拒。只取读锁，O(1)。
func (l *List) CheckAccept(ip string) bool {
	if l == nil {
		return true
	}
	if l.staticAllowed(ip) {
		return true
	}
	key, err := KeyOf(ip)
	if err != nil {
		return true
	}
	now := l.now()
	l.mu.RLock()
	defer l.mu.RUnlock()
	e := l.entries[key]
	if e != nil && e.Ban.active(now) {
		atomic.AddInt64(&e.Rejected, 1)
		return false
	}
	return true
}

// IsBanned 报告 ip（或键）当前是否被封（不计数）。
func (l *List) IsBanned(ip string) bool {
	if l == nil {
		return false
	}
	key, err := NormalizeKey(ip)
	if err != nil {
		return false
	}
	now := l.now()
	l.mu.RLock()
	defer l.mu.RUnlock()
	e := l.entries[key]
	return e != nil && e.Ban.active(now)
}

// ObserveConnect 记录一次通过准入的连接。
func (l *List) ObserveConnect(ip string) {
	key, err := KeyOf(ip)
	if err != nil {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.touch(key, now)
	if e == nil {
		l.untracked.Add(1)
		return
	}
	audits, _ := l.refresh(e, now)
	e.Connections++
	e.LastSeen = now
	if e.pinned(now) {
		l.dirty.Store(true)
	}
	l.place(e, now, true)
	if len(audits) > 0 {
		go l.emit(audits) // 持锁中，审计写日志放到锁外的 goroutine
	}
}

// ClientInfo 是一次成功注册/加入的客户端信息。ClientID 只用于计算指纹，不保存。
//
// Verified：help 持有效协助码成功加入，或 share 注册后被成功加入（完成配对）。注册本身不需要
// 任何凭据，只有 Verified 的观察才授予"近期有成功客户端"的计分豁免；仅注册的客户端只做识别展示。
type ClientInfo struct {
	Role     string // share | help
	Version  string
	Host     string
	ClientID string
	Verified bool
}

// ObserveClient 记录一个在该 IP 上成功完成注册/加入的客户端。
func (l *List) ObserveClient(ip string, c ClientInfo) {
	key, err := KeyOf(ip)
	if err != nil {
		return
	}
	now := l.now()
	fp := l.fingerprint(c.ClientID)
	host, version := cleanField(c.Host), cleanField(c.Version)
	l.mu.Lock()
	defer l.mu.Unlock()
	if c.Verified {
		l.memOf(key).touch(key).LastGood = now
	}
	l.dirty.Store(true)
	e := l.touch(key, now)
	if e == nil {
		l.untracked.Add(1)
		return
	}
	audits, _ := l.refresh(e, now)
	e.LastSeen = now
	defer l.place(e, now, true)
	if len(audits) > 0 {
		go l.emit(audits)
	}
	for i := range e.Clients {
		cr := &e.Clients[i]
		same := cr.Role == c.Role && cr.ClientFP == fp
		if fp == "" {
			same = same && cr.Host == host && cr.Version == version
		}
		if same {
			cr.LastSeen = now
			cr.Count++
			cr.Version = version
			cr.Host = host
			cr.Verified = cr.Verified || c.Verified
			return
		}
	}
	e.Clients = append(e.Clients, clientRec{Role: c.Role, Version: version, Host: host, ClientFP: fp, FirstSeen: now, LastSeen: now, Count: 1, Verified: c.Verified})
	if len(e.Clients) > maxClientsPerIP {
		oldest := 0
		for i := range e.Clients {
			if e.Clients[i].LastSeen < e.Clients[oldest].LastSeen {
				oldest = i
			}
		}
		e.Clients = append(e.Clients[:oldest], e.Clients[oldest+1:]...)
	}
}

// RecordViolation 记录一次异常行为并在达到阈值时自动封禁。返回本次是否触发了封禁。
// 白名单、宽限期、已被封的键，以及豁免类事件只记录不计分。
func (l *List) RecordViolation(ip string, kind Kind, detail string) bool {
	if l == nil {
		return false
	}
	if l.staticAllowed(ip) {
		return false // 启动参数放行：不计分、不建条目
	}
	key, err := KeyOf(ip)
	if err != nil {
		return false
	}
	weight, known := kindWeight[kind]
	if !known {
		weight = 1
	}
	now := l.now()
	banned := false

	l.mu.Lock()
	e := l.touch(key, now)
	if e == nil {
		l.mu.Unlock()
		l.untracked.Add(1)
		return false
	}
	audits, _ := l.refresh(e, now)
	e.LastSeen = now
	if exemptIfRecentGood[kind] && l.recentGood(key, now) {
		weight = 0
	}
	if e.pinned(now) {
		weight = 0
		l.dirty.Store(true)
	}
	if weight > 0 {
		e.addHit(hitRec{ts: now, weight: weight})
	}
	e.addEvent(eventRec{TS: now, Kind: string(kind), Detail: truncateBytes(cleanText(detail), 64), Points: weight})
	if weight > 0 {
		if pts := e.points(now, l.cfg.Window); pts >= l.cfg.Threshold {
			audits = append(audits, l.autoBanLocked(e, now, kind, pts)...)
			banned = true
		}
	}
	l.place(e, now, true)
	l.mu.Unlock()

	l.emit(audits)
	if banned {
		l.requestSave(false)
	}
	return banned
}

func (l *List) autoBanLocked(e *entry, now int64, kind Kind, points int) []auditRec {
	m := l.memOf(e.IP).touch(e.IP)
	if m.Offenses > 0 && m.LastBanEnd > 0 && now-m.LastBanEnd > int64(l.cfg.OffenseDecay/time.Second) {
		m.Offenses = 0
	}
	dur := l.cfg.BanBase
	for i := 0; i < m.Offenses && dur < l.cfg.BanMax; i++ {
		dur *= 2
	}
	if dur > l.cfg.BanMax {
		dur = l.cfg.BanMax
	}
	until := now + int64(dur/time.Second)
	m.Offenses++
	m.LastBanEnd = until
	e.BanCount++
	e.hits = nil
	note := fmt.Sprintf("%s 内累计 %d 分（阈值 %d），最后一次：%s", fmtDuration(l.cfg.Window), points, l.cfg.Threshold, kind)
	e.Ban = &banRec{Since: now, Until: until, Reason: "auto", Actor: autoActor, Note: note, Kind: string(kind)}
	trigger, partial := banTrigger(e.Events, now, l.cfg.Window, points)
	l.recordBanStart(e, m.Offenses, trigger, partial)
	e.addHistory(historyRec{TS: now, Action: "ban", Actor: autoActor, Note: note, Detail: fmt.Sprintf("自动封禁 %s（第 %d 次）", fmtDuration(dur), m.Offenses)})
	audits := []auditRec{{logger.AuditLevelWarn, "ip_banned", fmt.Sprintf("IP %s 已被自动封禁 %s，第 %d 次，%s", e.IP, fmtDuration(dur), m.Offenses, note), map[string]interface{}{
		"ip": e.IP, "actor": autoActor, "reason": string(kind), "points": points,
		"duration": dur.String(), "until": until, "offense": m.Offenses,
	}}}
	// 先入堆并立即钉住（移出 LRU），再处理上限淘汰。顺序不能反：被淘汰的条目放回 LRU 时会触发
	// LRU 淘汰，若 e 还挂在 LRU 上（且恰好最旧），会被从主表删掉——封禁当场失效，还在堆里留下孤儿，
	// 孤儿日后被弹出时会删掉同 IP 后来新建的条目（连带别人的生效封禁）。
	l.pushAutoBan(e)
	l.place(e, now, false)
	fam := familyOf(e.IP)
	if fam == famV6 {
		// /48 子配额：只在同一 /48 内部淘汰，持有整个 /48 的来源挤不掉别人的封禁。
		p := prefix48(e.IP)
		for len(l.per48[p]) > l.per48Cap() {
			var victim *entry
			for x := range l.per48[p] {
				if x != e && (victim == nil || x.Ban.Until < victim.Ban.Until) {
					victim = x
				}
			}
			audits = append(audits, l.dropAutoBan(victim, now, "同一 IPv6 /48 下的自动封禁超出子配额，最早到期的被提前解除")...)
		}
	}
	// 族配额：淘汰最早到期的（O(log n)）。刚封的 e 恰好最早到期时淘汰次早的。
	for l.bans[fam].Len() > l.autoBanCap() {
		h := l.bans[fam]
		victim := h[0]
		if victim == e {
			victim = h[1]
			if len(h) > 2 && h[2].Ban.Until < victim.Ban.Until {
				victim = h[2]
			}
		}
		audits = append(audits, l.dropAutoBan(victim, now, "自动封禁条目超出上限，最早到期的被提前解除")...)
	}
	return audits
}

// dropAutoBan 因配额把一条自动封禁移出。若它其实已到期、只是还没被 Sweep，按到期处理
// （记 ban_expired、保留递增记忆）；否则记 ban_evicted 并清零递增次数，免得它下次被封直接跳到长时长。
func (l *List) dropAutoBan(victim *entry, now int64, detail string) []auditRec {
	var audits []auditRec
	if !victim.Ban.active(now) {
		audits, _ = l.refresh(victim, now)
	} else {
		l.removeAutoBan(victim)
		l.recordBanEnd(victim, EndEvicted, now, autoActor)
		victim.Ban = nil
		if l.autoBanEvictedHook != nil {
			l.autoBanEvictedHook(victim.IP)
		}
		if r := l.memOf(victim.IP).get(victim.IP); r != nil {
			r.Offenses, r.LastBanEnd = 0, 0
		}
		victim.addHistory(historyRec{TS: now, Action: "ban_evicted", Actor: autoActor, Detail: detail})
		audits = append(audits, auditRec{logger.AuditLevelWarn, "ip_ban_evicted", fmt.Sprintf("IP %s 的自动封禁因条目超限被提前解除", victim.IP), map[string]interface{}{"ip": victim.IP, "actor": autoActor}})
	}
	l.place(victim, now, false)
	return audits
}

// BanResult 处置结果。Warning 非空表示需要提醒操作人的情况。
type BanResult struct {
	Warning string `json:"warning,omitempty"`
}

// Ban 手动封禁 ip（IPv4 地址、IPv6 地址或 /64 前缀）。permanent=false 时 d 必须为正。
// 会回调 OnManualBan 断开现有连接。
func (l *List) Ban(ip string, d time.Duration, permanent bool, actor, note string) (BanResult, error) {
	key, err := NormalizeKey(ip)
	if err != nil {
		return BanResult{}, err
	}
	if !permanent && d <= 0 {
		return BanResult{}, ErrInvalidDuration
	}
	if l.staticCovers(key) {
		return BanResult{}, ErrStaticAllowed
	}
	note = cleanNote(note)
	now := l.now()
	var res BanResult
	l.mu.Lock()
	e := l.touch(key, now)
	if e == nil {
		l.mu.Unlock()
		return BanResult{}, ErrCapacity
	}
	audits, _ := l.refresh(e, now)
	if e.Allow.active(now) {
		res.Warning = "该 IP 在白名单中，手动封禁已覆盖白名单（白名单条目保留，解禁后继续生效）"
	}
	l.removeAutoBan(e)
	b := &banRec{Since: now, Permanent: permanent, Reason: "manual", Actor: actor, Note: note}
	durText := "永久"
	if !permanent {
		b.Until = now + int64(d/time.Second)
		durText = fmtDuration(d)
	}
	if e.Ban.active(now) {
		l.recordBanEnd(e, EndReplaced, now, actor)
	}
	e.Ban = b
	l.recordBanStart(e, 0, nil, false)
	e.GraceUntil = 0
	e.hits = nil
	e.BanCount++
	e.addHistory(historyRec{TS: now, Action: "ban", Actor: actor, Note: note, Detail: "手动封禁 " + durText})
	l.place(e, now, true)
	l.mu.Unlock()

	l.emit(append(audits, auditRec{logger.AuditLevelWarn, "ip_banned", fmt.Sprintf("IP %s 已被手动封禁 %s（操作人 %s）", key, durText, actor), map[string]interface{}{
		"ip": key, "actor": actor, "reason": "manual", "note": note, "duration": durText, "until": b.Until, "overrides_allowlist": res.Warning != "",
	}}))
	l.requestSave(true)
	if fn, ok := l.onManualBan.Load().(func(string)); ok && fn != nil {
		fn(key)
	}
	return res, nil
}

// AllowSpec 加入白名单的有效期：Permanent 或正的 Duration。
type AllowSpec struct {
	Duration  time.Duration
	Permanent bool
}

// Unban 解禁：移除封禁、清零分值与递增次数并进入宽限期；allow 非 nil 时同时加白。
func (l *List) Unban(ip, actor, note string, allow *AllowSpec) error {
	key, err := NormalizeKey(ip)
	if err != nil {
		return err
	}
	if allow != nil && !allow.Permanent && allow.Duration <= 0 {
		return ErrInvalidDuration
	}
	note = cleanNote(note)
	now := l.now()
	l.mu.Lock()
	e := l.entries[key]
	var audits []auditRec
	if e != nil {
		audits, _ = l.refresh(e, now)
	}
	if e == nil || !e.Ban.active(now) {
		if e != nil {
			l.place(e, now, false)
		}
		l.mu.Unlock()
		l.emit(audits)
		return ErrNotBanned
	}
	audits = append(audits, l.unbanLocked(e, now, actor, note))
	if allow != nil {
		audits = append(audits, l.allowLocked(e, now, *allow, actor, note))
	}
	l.place(e, now, true)
	l.mu.Unlock()
	l.emit(audits)
	l.requestSave(true)
	return nil
}

func (l *List) unbanLocked(e *entry, now int64, actor, note string) auditRec {
	prev := e.Ban
	l.removeAutoBan(e)
	l.recordBanEnd(e, EndUnban, now, actor)
	e.Ban = nil
	e.hits = nil
	if r := l.memOf(e.IP).get(e.IP); r != nil {
		r.Offenses, r.LastBanEnd = 0, 0
	}
	e.GraceUntil = now + int64(l.cfg.UnbanGrace/time.Second)
	e.addHistory(historyRec{TS: now, Action: "unban", Actor: actor, Note: note,
		Detail: fmt.Sprintf("手动解禁（原%s封禁），宽限期 %s 内不自动封禁", reasonText(prev.Reason), fmtDuration(l.cfg.UnbanGrace))})
	return auditRec{logger.AuditLevelInfo, "ip_unbanned", fmt.Sprintf("IP %s 已被手动解禁（操作人 %s，原%s封禁）", e.IP, actor, reasonText(prev.Reason)), map[string]interface{}{
		"ip": e.IP, "actor": actor, "note": note, "prev_reason": prev.Reason, "grace_until": e.GraceUntil,
	}}
}

// Allow 加入（或更新）白名单；不改变封禁状态。
func (l *List) Allow(ip string, d time.Duration, permanent bool, actor, note string) error {
	_, err := l.AllowWithOptions(ip, AllowSpec{Duration: d, Permanent: permanent}, actor, note, false)
	return err
}

// AllowWithOptions 加入（或更新）白名单。目标当前被封时：unban=true 一并解禁（进入宽限期）；
// 否则保持封禁并返回 Warning，绝不静默。
func (l *List) AllowWithOptions(ip string, spec AllowSpec, actor, note string, unban bool) (BanResult, error) {
	key, err := NormalizeKey(ip)
	if err != nil {
		return BanResult{}, err
	}
	if !spec.Permanent && spec.Duration <= 0 {
		return BanResult{}, ErrInvalidDuration
	}
	note = cleanNote(note)
	now := l.now()
	var res BanResult
	l.mu.Lock()
	e := l.touch(key, now)
	if e == nil {
		l.mu.Unlock()
		return BanResult{}, ErrCapacity
	}
	audits, _ := l.refresh(e, now)
	if e.Ban.active(now) {
		if unban {
			audits = append(audits, l.unbanLocked(e, now, actor, note))
		} else {
			res.Warning = "该 IP 当前处于封禁中，加入白名单不会解除封禁；如需放行请同时选择解禁"
		}
	}
	audits = append(audits, l.allowLocked(e, now, spec, actor, note))
	l.place(e, now, true)
	l.mu.Unlock()
	l.emit(audits)
	l.requestSave(true)
	return res, nil
}

func (l *List) allowLocked(e *entry, now int64, spec AllowSpec, actor, note string) auditRec {
	a := &allowRec{Since: now, Permanent: spec.Permanent, Actor: actor, Note: note}
	durText := "永久"
	if !spec.Permanent {
		a.Until = now + int64(spec.Duration/time.Second)
		durText = fmtDuration(spec.Duration)
	}
	e.Allow = a
	e.hits = nil
	e.addHistory(historyRec{TS: now, Action: "allow", Actor: actor, Note: note, Detail: "加入白名单 " + durText})
	return auditRec{logger.AuditLevelInfo, "ip_allowlisted", fmt.Sprintf("IP %s 已加入白名单 %s（操作人 %s）", e.IP, durText, actor), map[string]interface{}{
		"ip": e.IP, "actor": actor, "note": note, "duration": durText, "until": a.Until,
	}}
}

// Disallow 移出白名单，不影响封禁状态。
func (l *List) Disallow(ip, actor, note string) error {
	key, err := NormalizeKey(ip)
	if err != nil {
		return err
	}
	note = cleanNote(note)
	now := l.now()
	l.mu.Lock()
	e := l.entries[key]
	var audits []auditRec
	if e != nil {
		audits, _ = l.refresh(e, now)
	}
	if e == nil || !e.Allow.active(now) {
		if e != nil {
			l.place(e, now, false)
		}
		l.mu.Unlock()
		l.emit(audits)
		return ErrNotAllowlisted
	}
	e.Allow = nil
	e.addHistory(historyRec{TS: now, Action: "disallow", Actor: actor, Note: note, Detail: "移出白名单"})
	l.place(e, now, true)
	l.mu.Unlock()
	l.emit(append(audits, auditRec{logger.AuditLevelInfo, "ip_disallowed", fmt.Sprintf("IP %s 已移出白名单（操作人 %s）", key, actor), map[string]interface{}{"ip": key, "actor": actor, "note": note}}))
	l.requestSave(true)
	return nil
}

// Sweep 清理到期的封禁、白名单、宽限期、记忆衰减与过期历史；由 relay cleanupLoop 每分钟调用。
// 主表与记忆都有界，全量遍历的代价有上限（默认约 1.2 万 + 1.6 万条，每分钟一次）。
func (l *List) Sweep() {
	if l == nil {
		return
	}
	now := l.now()
	decay := int64(l.cfg.OffenseDecay / time.Second)
	goodCutoff := now - int64(goodClientWindow/time.Second)
	retention := int64(historyRetention / time.Second)
	var audits []auditRec
	changed := false
	l.mu.Lock()
	var cooled []*entry // 分值已衰减完、要从 scoring 移回 idle 的条目
	for _, e := range l.entries {
		a, c := l.refresh(e, now)
		audits = append(audits, a...)
		changed = changed || c
		if n := len(e.History); n > 0 && !e.pinned(now) && e.History[n-1].TS <= now-retention {
			e.History = nil
			changed = true
		}
		if e.lru == l.scoring && len(e.hits) == 0 && !e.pinned(now) {
			cooled = append(cooled, e)
			continue
		}
		l.place(e, now, false)
	}
	l.mergeIntoIdle(cooled)
	for _, mem := range l.mem {
		var stale []string
		for el := mem.ll.Front(); el != nil; el = el.Next() {
			r := el.Value.(*memRec)
			if r.Offenses > 0 && now-r.LastBanEnd > decay {
				if e := l.entries[r.Key]; e == nil || !e.Ban.active(now) {
					r.Offenses, r.LastBanEnd = 0, 0
					changed = true
				}
			}
			if r.Offenses == 0 && r.LastGood <= goodCutoff {
				stale = append(stale, r.Key)
			}
		}
		for _, k := range stale {
			mem.del(k)
			changed = true
		}
	}
	l.mu.Unlock()
	l.emit(audits)
	if l.dirty.Swap(false) || changed {
		l.requestSave(false)
	}
}

// mergeIntoIdle 把分值衰减完的条目从 scoring 移回 idle，按 LastSeen 归并插入而不是推到队尾：
// 否则每分钟一次的 Sweep 会把一批很久没出现的条目"刷新"成最近使用，打乱淘汰顺序。O(n + m log m)。
func (l *List) mergeIntoIdle(cooled []*entry) {
	if len(cooled) == 0 {
		return
	}
	sort.Slice(cooled, func(i, j int) bool { return cooled[i].LastSeen < cooled[j].LastSeen })
	cur := l.idle.Front()
	for _, e := range cooled {
		if e.lru != l.scoring {
			continue // 同一轮 Sweep 里已被 LRU 淘汰（其他条目到期回到 LRU 时触发）
		}
		e.lru.Remove(e.elem)
		for cur != nil && cur.Value.(*entry).LastSeen <= e.LastSeen {
			cur = cur.Next()
		}
		if cur == nil {
			e.elem = l.idle.PushBack(e)
		} else {
			e.elem = l.idle.InsertBefore(e, cur)
		}
		e.lru = l.idle
	}
}

// ---------- 视图 ----------

type BanView struct {
	Since     int64  `json:"since"`
	Until     int64  `json:"until"`
	Permanent bool   `json:"permanent"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor"`
	Note      string `json:"note"`
	Kind      string `json:"kind,omitempty"`
}

type AllowView struct {
	Since     int64  `json:"since"`
	Until     int64  `json:"until"`
	Permanent bool   `json:"permanent"`
	Actor     string `json:"actor"`
	Note      string `json:"note"`
}

type ClientView struct {
	Role      string `json:"role"`
	Version   string `json:"version"`
	Host      string `json:"host"`
	ClientFP  string `json:"client_fp"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	Count     int64  `json:"count"`
	Verified  bool   `json:"verified"` // 成功加入（持有效协助码）或注册后完成过配对；false = 仅注册，身份为自报
}

type EventView struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Points int    `json:"points"`
}

type HistoryView struct {
	TS     int64  `json:"ts"`
	Action string `json:"action"`
	Actor  string `json:"actor"`
	Note   string `json:"note"`
	Detail string `json:"detail"`
}

// EntryView 是单个键的完整只读快照，供 Web 控制台展示。
type EntryView struct {
	IP          string        `json:"ip"` // IPv4 地址或 IPv6 /64 前缀
	Status      string        `json:"status"`
	Ban         *BanView      `json:"ban,omitempty"`
	Allow       *AllowView    `json:"allow,omitempty"`
	GraceUntil  int64         `json:"grace_until,omitempty"`
	Points      int           `json:"points"`
	Offenses    int           `json:"offenses"`
	BanCount    int           `json:"ban_count"`
	Connections int64         `json:"connections"`
	Rejected    int64         `json:"rejected"`
	FirstSeen   int64         `json:"first_seen"`
	LastSeen    int64         `json:"last_seen"`
	LastGood    int64         `json:"last_good,omitempty"`
	Clients     []ClientView  `json:"clients"`
	Events      []EventView   `json:"events"`
	History     []HistoryView `json:"history"`
	Verdict     string        `json:"verdict"`
	VerdictSelf string        `json:"verdict_self_report,omitempty"` // Verdict 中客户端自报的片段
}

// ConfigView 生效配置（秒）。
type ConfigView struct {
	Threshold     int   `json:"threshold"`
	WindowSec     int64 `json:"window_seconds"`
	BanBaseSec    int64 `json:"ban_base_seconds"`
	BanMaxSec     int64 `json:"ban_max_seconds"`
	DecaySec      int64 `json:"offense_decay_seconds"`
	UnbanGraceSec int64 `json:"unban_grace_seconds"`
	MaxTracked    int   `json:"max_tracked"`
	MaxAutoBans   int   `json:"max_auto_bans"`
}

// SummaryView 是列表页的单行摘要：不含客户端/事件/历史明细，明细按需走 Detail。
type SummaryView struct {
	IP          string     `json:"ip"`
	Status      string     `json:"status"` // banned | allowlisted | grace | suspect | seen
	Ban         *BanView   `json:"ban,omitempty"`
	Allow       *AllowView `json:"allow,omitempty"`
	GraceUntil  int64      `json:"grace_until,omitempty"`
	Points      int        `json:"points"`
	Offenses    int        `json:"offenses"`
	BanCount    int        `json:"ban_count"`
	Connections int64      `json:"connections"`
	Rejected    int64      `json:"rejected"`
	FirstSeen   int64      `json:"first_seen"`
	LastSeen    int64      `json:"last_seen"`
	ClientCount int        `json:"client_count"`
	EventCount  int        `json:"event_count"`
	LastEvent   *EventView `json:"last_event,omitempty"`
	Verdict     string     `json:"verdict"`
	VerdictSelf string     `json:"verdict_self_report,omitempty"` // Verdict 中客户端自报的片段
}

// Page 是 Query 的结果：服务端过滤 + 分页。
type Page struct {
	Persistent bool          `json:"persistent"`
	Config     ConfigView    `json:"config"`
	Tracked    int           `json:"tracked"`   // 主表条目数
	Untracked  uint64        `json:"untracked"` // 主表满时未能跟踪的事件累计数
	Total      int           `json:"total"`     // 过滤后的条目数（分页前）
	Offset     int           `json:"offset"`    // 同 Start（兼容按 offset 翻页的调用方）
	Start      int           `json:"start"`     // 本页第一条在当前完整排序结果里的下标（0 起）
	Limit      int           `json:"limit"`
	NextCursor string        `json:"next_cursor"` // 本页最后一条的排序键；下一页传 cursor=它、dir=next
	PrevCursor string        `json:"prev_cursor"` // 本页第一条的排序键；上一页传 cursor=它、dir=prev
	HasNext    bool          `json:"has_next"`
	HasPrev    bool          `json:"has_prev"`
	Entries    []SummaryView `json:"entries"`
	Now        int64         `json:"now"`

	Sort        string `json:"sort"`                   // 本次生效的排序方式
	Order       string `json:"order"`                  // 本次生效的方向
	CursorReset bool   `json:"cursor_reset,omitempty"` // 游标与本次排序/筛选不一致，已忽略并从第一页返回
}

// 分页参数：列表接口单页最多返回 MaxPageLimit 条摘要。
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// Filter 选择要返回的条目。Status：banned | allowlisted | grace | suspect | all（默认，
// 有封禁/白名单/宽限期/分值/事件/历史之一）| tracked（全部跟踪条目）。
// Reason：auto | manual，仅匹配有封禁的条目。
//
// 翻页：Cursor 为空时按 Offset 取页；Cursor 非空时按排序键取页（keyset）——Dir=next 取严格排在
// 游标之后的 Limit 条，Dir=prev 取严格排在游标之前的最后 Limit 条。条目的名次会因处置或新的连接
// 而变化，按 offset 翻页会跳过或重复条目；按排序键翻页不受其他条目名次变化的影响。
type Filter struct {
	Status string
	Reason string
	Offset int
	Limit  int
	Cursor string // 不透明游标（来自 Page.NextCursor / PrevCursor）
	Dir    string // next | prev
	Kind   string // 触发原因：自动封禁时命中的事件类型（Kind 常量之一），仅匹配有封禁的条目
	Search string // 子串搜索（不区分大小写）：键(IP/网段)、客户端主机名、版本、封禁/白名单备注
	Sort   string // status（默认）| last_seen | banned_at | expires | points
	Order  string // desc（默认）| asc
}

func (l *List) configView() ConfigView {
	return ConfigView{
		Threshold:     l.cfg.Threshold,
		WindowSec:     int64(l.cfg.Window / time.Second),
		BanBaseSec:    int64(l.cfg.BanBase / time.Second),
		BanMaxSec:     int64(l.cfg.BanMax / time.Second),
		DecaySec:      int64(l.cfg.OffenseDecay / time.Second),
		UnbanGraceSec: int64(l.cfg.UnbanGrace / time.Second),
		MaxTracked:    l.cfg.MaxTracked,
		MaxAutoBans:   l.cfg.MaxAutoBans,
	}
}

var statusRank = map[string]int{"banned": 0, "allowlisted": 1, "grace": 2, "suspect": 3, "seen": 4}

// quickStatus 只看最后一次命中判断"正在计分"，O(1)（命中按时间追加）。
func quickStatus(e *entry, now, cutoff int64) string {
	switch {
	case e.Ban.active(now):
		return "banned"
	case e.Allow.active(now):
		return "allowlisted"
	case e.GraceUntil > now:
		return "grace"
	case len(e.hits) > 0 && e.hits[len(e.hits)-1].ts > cutoff:
		return "suspect"
	default:
		return "seen"
	}
}

// Detail 返回单个键（IPv4 地址、IPv6 地址或 /64 前缀）的完整视图。非法或未跟踪返回 false。
func (l *List) Detail(ip string) (EntryView, bool) {
	key, err := NormalizeKey(ip)
	if err != nil {
		return EntryView{}, false
	}
	now := l.now()
	l.mu.RLock()
	defer l.mu.RUnlock()
	e := l.entries[key]
	if e == nil {
		return EntryView{}, false
	}
	return l.detailLocked(e, now), true
}

// statusLocked 计算状态与当前分值，不分配内存。
func (l *List) statusLocked(e *entry, now int64) (string, int) {
	pts := e.points(now, l.cfg.Window)
	switch {
	case e.Ban.active(now):
		return "banned", pts
	case e.Allow.active(now):
		return "allowlisted", pts
	case e.GraceUntil > now:
		return "grace", pts
	case pts > 0:
		return "suspect", pts
	default:
		return "seen", pts
	}
}

func (l *List) offenses(key string) int {
	if r := l.memOf(key).get(key); r != nil {
		return r.Offenses
	}
	return 0
}

func (l *List) summaryLocked(e *entry, now int64) SummaryView {
	st, pts := l.statusLocked(e, now)
	vText, vSelf := verdict(e.Clients, e.Events)
	v := SummaryView{
		IP:          e.IP,
		Status:      st,
		Points:      pts,
		Offenses:    l.offenses(e.IP),
		BanCount:    e.BanCount,
		Connections: e.Connections,
		Rejected:    atomic.LoadInt64(&e.Rejected),
		FirstSeen:   e.FirstSeen,
		LastSeen:    e.LastSeen,
		ClientCount: len(e.Clients),
		EventCount:  len(e.Events),
		Ban:         banView(e.Ban, now),
		Allow:       allowView(e.Allow, now),
		Verdict:     vText,
		VerdictSelf: vSelf,
	}
	if e.GraceUntil > now {
		v.GraceUntil = e.GraceUntil
	}
	if n := len(e.Events); n > 0 {
		ev := EventView(e.Events[n-1])
		v.LastEvent = &ev
	}
	return v
}

func (l *List) detailLocked(e *entry, now int64) EntryView {
	s := l.summaryLocked(e, now)
	v := EntryView{
		IP: s.IP, Status: s.Status, Ban: s.Ban, Allow: s.Allow, GraceUntil: s.GraceUntil,
		Points: s.Points, Offenses: s.Offenses, BanCount: s.BanCount, Connections: s.Connections,
		Rejected: s.Rejected, FirstSeen: s.FirstSeen, LastSeen: s.LastSeen, Verdict: s.Verdict, VerdictSelf: s.VerdictSelf,
		Clients: make([]ClientView, 0, len(e.Clients)),
		Events:  make([]EventView, 0, len(e.Events)),
		History: make([]HistoryView, 0, len(e.History)),
	}
	if r := l.memOf(e.IP).get(e.IP); r != nil {
		v.LastGood = r.LastGood
	}
	clients := append([]clientRec(nil), e.Clients...)
	sort.Slice(clients, func(i, j int) bool { return clients[i].LastSeen > clients[j].LastSeen })
	for _, c := range clients {
		v.Clients = append(v.Clients, ClientView(c))
	}
	for i := len(e.Events) - 1; i >= 0; i-- {
		v.Events = append(v.Events, EventView(e.Events[i]))
	}
	for _, h := range e.History {
		v.History = append(v.History, HistoryView(h))
	}
	return v
}

func banView(b *banRec, now int64) *BanView {
	if !b.active(now) {
		return nil
	}
	return &BanView{Since: b.Since, Until: b.Until, Permanent: b.Permanent, Reason: b.Reason, Actor: b.Actor, Note: b.Note, Kind: b.Kind}
}

func allowView(a *allowRec, now int64) *AllowView {
	if !a.active(now) {
		return nil
	}
	return &AllowView{Since: a.Since, Until: a.Until, Permanent: a.Permanent, Actor: a.Actor, Note: a.Note}
}

// latestClient 最近出现的客户端；verifiedOnly 时只看成功加入/完成配对的。
func latestClient(clients []clientRec, verifiedOnly bool) *clientRec {
	var best *clientRec
	for i := range clients {
		if verifiedOnly && !clients[i].Verified {
			continue
		}
		if best == nil || clients[i].LastSeen > best.LastSeen {
			best = &clients[i]
		}
	}
	return best
}

func clientDesc(c *clientRec) string {
	parts := []string{}
	if c.Version != "" {
		parts = append(parts, c.Version)
	}
	if c.Host != "" {
		parts = append(parts, c.Host)
	}
	if len(parts) == 0 {
		return roleText(c.Role) + "，未上报版本与身份"
	}
	return roleText(c.Role) + " " + strings.Join(parts, ", ")
}

// verdict 生成帮助运维判断"自己人还是扫描器"的提示。
//
// 注册不需要任何凭据，版本/身份串也是客户端自报、可随意伪造（如"财务部-张三-PC"）：只有成功加入
// （持有效协助码）或完成过配对的客户端才算"成功客户端"；仅注册的只如实标注为未认证。
//
// 返回值 self 是判定文本里客户端自报的那一段（原样包含在 text 中），前端用 <bdi> 隔离渲染，
// 即使自报串里含有未被剔除的方向性字符，也不会影响警示文字的显示方向。
func verdict(clients []clientRec, events []eventRec) (text, self string) {
	if v := latestClient(clients, true); v != nil {
		self = clientDesc(v)
		s := fmt.Sprintf("曾有成功加入/完成配对的客户端（自报：%s）", self)
		if len(clients) > 1 {
			s += fmt.Sprintf("，共 %d 个客户端", len(clients))
		}
		return s, self
	}
	ev := eventVerdict(events)
	if c := latestClient(clients, false); c != nil {
		self = clientDesc(c)
		s := fmt.Sprintf("仅有未认证的注册（自报：%s；注册无需凭据、身份可伪造，不能据此认定是自己人）", self)
		if len(events) > 0 {
			s += "；" + ev
		}
		return s, self
	}
	return ev, ""
}

func eventVerdict(events []eventRec) string {
	if len(events) == 0 {
		return "暂无违规记录"
	}
	tlsN, aborted, proto := 0, 0, 0
	for _, ev := range events {
		switch Kind(ev.Kind) {
		case KindTLSGarbage:
			tlsN++
		case KindTLSAborted:
			tlsN++
			aborted++
		case KindJoinFailed, KindJoinFailuresExceeded:
		default:
			proto++
		}
	}
	switch {
	case aborted == len(events):
		return "只有连上即断/握手被对端中止（可能是健康检查或证书不匹配的客户端），未计分"
	case tlsN == len(events):
		return "从未完成握手，疑似扫描器"
	case proto > 0:
		return "从未成功注册/加入，协议异常，疑似扫描器或非本客户端"
	default:
		return "从未成功加入，多次协助码错误，疑似爆破"
	}
}

func roleText(role string) string {
	switch role {
	case "share":
		return "被协助端"
	case "help":
		return "协助端"
	default:
		return "客户端"
	}
}

func reasonText(reason string) string {
	if reason == "manual" {
		return "手动"
	}
	return "自动"
}

// cleanNote 剔除控制字符并截断，备注来自 Web 表单。
func cleanNote(s string) string {
	return truncate(strings.TrimSpace(cleanText(s)), maxNoteLen)
}

// cleanText 剔除控制字符（Cc）、格式字符（Cf：双向控制符 U+202A–202E / U+2066–2069、U+200E/200F、
// U+061C、零宽字符等）以及行/段分隔符 U+2028/2029。客户端自报的版本/身份串、事件 detail、备注都可能
// 被攻击者控制：双向控制符能让判定提示里"身份可伪造"的警示在界面上被倒序渲染（界面欺骗），
// 分隔符在 JSON 里会转义成 6 字节，放大持久化文件。
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return -1
		}
		return r
	}, s)
}

// cleanField 客户端自报的版本/身份串：剔除控制/格式字符后按字节截断到 maxClientField。
func cleanField(s string) string {
	return truncateBytes(cleanText(s), maxClientField)
}

// truncateBytes 按字节截断到 n 以内，不切断 UTF-8 字符。
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func fmtDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d 天", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d 小时", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", d/time.Minute)
	default:
		return d.String()
	}
}
