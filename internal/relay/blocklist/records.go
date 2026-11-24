package blocklist

// 封禁记录：每一次封禁（自动或手动）一条，记下完整生命周期——何时、因何被封，计划多久，
// 实际何时以何种方式结束。与名单条目分开保存、分开淘汰：名单只回答"现在封没封"，
// 记录回答"过去封过谁、为什么、封了多久"。
//
// 锁：Records 自带互斥锁，是叶子锁。List 在持有自己的锁时同步调用它（记录顺序与名单状态严格一致），
// 它绝不回调 List。记录写入后不再原地修改（结束时整条换成新副本），读取与写盘在锁内只复制指针切片。
//
// 关联：记录按 (对象类型, 键, since, until, permanent) 对应名单里的生效封禁，只比 since 不够（同一秒内
// 自动封禁被手动永久封禁覆盖时 since 相同）。banRec 里不加记录 id——blocklist.json 的格式
// 一个字段都不能加（线上旧版 loadFile 开了 DisallowUnknownFields，见 compat_golden_test.go）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 封禁对象类型。
const (
	TargetIP     = "ip"
	TargetDevice = "device"
)

// 封禁结束方式；未结束为空。
const (
	EndExpired  = "expired"  // 到期（ended_at 取计划到期时间）
	EndUnban    = "unban"    // 人工解禁
	EndEvicted  = "evicted"  // 自动封禁超出配额，被提前解除
	EndReplaced = "replaced" // 被新的手动封禁覆盖
	EndLost     = "lost"     // 重启后名单里对不上（例如名单文件因写盘预算丢了这条）
)

const (
	// DefaultMaxRecords 与 DefaultRecordRetention：最近 5000 条或 90 天，先到为准；生效中的记录不受限。
	DefaultMaxRecords      = 5000
	DefaultRecordRetention = 90 * 24 * time.Hour

	recordsSchema = 1
	// recordsSaveBudget 已结束记录的写盘预算：从新到旧装到预算为止，超出先丢最旧的。生效中的记录不占它。
	recordsSaveBudget = 4 << 20
	// recordsHardBudget 整个文件的字节上限：生效中的记录最多占「硬上限 − 已结束预算」（只有上万条封禁同时
	// 生效才可能碰到），超出丢最旧的生效记录，下次启动对账时按名单补回；已结束记录另有自己的预算。
	// 保证写出的文件永远在加载上限之内，大规模封禁也不会把已结束的审计历史挤出文件。
	recordsHardBudget   = 28 << 20
	maxRecordsFileBytes = 32 << 20
	maxRecordTrigger    = 3
	maxRecordClients    = 3
	maxRecordField      = 64
	// maxRecordID 记录 id 的上限。文件内容不可信：id 或 next_id 被改成接近 int64 上限，新 id 会溢出成负数，
	// 打乱按 id 的二分查找，记录从此结束不了。运行时远到不了这个数（2^53 次封禁）。
	maxRecordID = 1 << 53
	// recordsTmpPrefix 记录文件的临时文件前缀。不能用名单的 ".blocklist-"：名单打开时会清理那个命名下的
	// 残留临时文件，同目录正在写的记录临时文件会被误删。
	recordsTmpPrefix = ".blrec-"
)

// TriggerItem 自动封禁时窗口内分值的一项分类汇总，如 join_failed/code_invalid ×20（20 分）。
type TriggerItem struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	Count  int    `json:"count"`
	Points int    `json:"points"`
}

// RecordClient 封禁时该键最近出现的客户端（不含 ClientID 及其指纹）。
type RecordClient struct {
	Role    string `json:"role"`
	Version string `json:"version"`
	Host    string `json:"host"`
}

// Record 一次封禁。字段名即记录文件的 JSON 格式，改名会破坏兼容。
type Record struct {
	ID      int64         `json:"id"`     // 单调递增，持久化，兼作分页游标
	Target  string        `json:"target"` // ip | device
	Key     string        `json:"key"`    // IP 键或 dev- 键
	Label   string        `json:"label,omitempty"`
	Via     string        `json:"via,omitempty"`
	Reason  string        `json:"reason"` // auto | manual
	Actor   string        `json:"actor"`
	Note    string        `json:"note,omitempty"`
	Trigger []TriggerItem `json:"trigger,omitempty"`
	// TriggerPartial 分项没凑够封禁时的累计分值：事件只留最近 20 条，0 分事件或调高的阈值会让更早的计分事件无从统计。
	TriggerPartial bool           `json:"trigger_partial,omitempty"`
	Clients        []RecordClient `json:"clients,omitempty"`
	Offense        int            `json:"offense,omitempty"` // 第几次自动封禁；手动为 0
	Since          int64          `json:"since"`
	Until          int64          `json:"until"` // 永久封禁为 0
	Permanent      bool           `json:"permanent"`
	EndedAt        int64          `json:"ended_at,omitempty"`
	End            string         `json:"end,omitempty"`
	EndActor       string         `json:"end_actor,omitempty"`
}

// RecordsConfig 封禁记录配置。零值字段取默认值。
type RecordsConfig struct {
	Path         string        // 持久化文件；空 = 纯内存
	MaxRecords   int           // 默认 DefaultMaxRecords
	Retention    time.Duration // 已结束记录的保留期，默认 DefaultRecordRetention
	SaveInterval time.Duration // 最小写盘间隔，默认 5s
	Now          func() time.Time

	saveBudget   int // 测试可调小；0 = recordsSaveBudget
	hardBudget   int // 测试可调小；0 = recordsHardBudget
	retryBackoff []time.Duration
	// atomicWrite 写盘函数，测试用来观察实际使用的临时文件命名；nil = writeFileAtomicPattern。
	atomicWrite func(path string, data []byte, pattern string) error
}

// Records 并发安全的封禁记录。nil 上的方法都是空操作。
type Records struct {
	cfg RecordsConfig

	mu     sync.Mutex
	recs   []*Record        // 按 id 升序；元素写入后不可变
	open   map[string]int64 // openKey(target, key) -> 未结束记录的 id
	nextID int64

	persistent bool
	path       string
	lock       *fileLock
	saveMu     sync.Mutex
	saveCh     chan struct{}
	stopCh     chan struct{}
	doneCh     chan struct{}
	closeOnce  sync.Once
	saveCount  atomic.Int64
}

type recordsFile struct {
	Schema  int       `json:"schema"`
	NextID  int64     `json:"next_id"`
	SavedAt int64     `json:"saved_at"`
	Records []*Record `json:"records"`
}

// SiblingPath 由名单文件路径派生同目录的附属文件：blocklist.json → blocklist-<suffix>.json。空路径返回空。
func SiblingPath(path, suffix string) string {
	if path == "" {
		return ""
	}
	ext := filepath.Ext(path)
	if ext == filepath.Base(path) { // ".blocklist" 这类只有前导点的文件名没有扩展名
		ext = ""
	}
	return strings.TrimSuffix(path, ext) + "-" + suffix + ext
}

// OpenRecords 创建封禁记录。cfg.Path 非空时从文件加载并持久化。
//
// 打开失败（锁被占用、文件损坏、过大、权限过宽）时返回一个可用的纯内存 Records 和错误：记录只是审计辅助
// 数据，名单照常工作；原文件不会被覆盖。
func OpenRecords(cfg RecordsConfig) (*Records, error) {
	if cfg.MaxRecords <= 0 {
		cfg.MaxRecords = DefaultMaxRecords
	}
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultRecordRetention
	}
	if cfg.SaveInterval <= 0 {
		cfg.SaveInterval = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.saveBudget <= 0 {
		cfg.saveBudget = recordsSaveBudget
	}
	if cfg.hardBudget <= 0 {
		cfg.hardBudget = recordsHardBudget
	}
	if len(cfg.retryBackoff) == 0 {
		cfg.retryBackoff = defaultRetryBackoff
	}
	r := &Records{cfg: cfg, open: make(map[string]int64), nextID: 1}
	if cfg.Path == "" {
		return r, nil
	}
	abs, err := filepath.Abs(cfg.Path)
	if err != nil {
		return r, fmt.Errorf("resolve blocklist records path: %w", err)
	}
	lock, err := acquireFileLock(abs + ".lock")
	if err != nil {
		return r, err
	}
	cleanStaleRecordTemps(abs)
	fd, err := loadRecordsFile(abs)
	if err != nil {
		_ = lock.Close()
		return r, err
	}
	r.lock, r.path, r.persistent = lock, abs, true
	if fd != nil {
		r.load(fd, r.now())
	}
	r.saveCh = make(chan struct{}, 1)
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	go r.saver()
	return r, nil
}

// Persistent 报告是否在持久化模式。
func (r *Records) Persistent() bool { return r != nil && r.persistent }

// Close 停止后台写盘，执行最后一次写盘并释放文件锁。共用它的名单要先关。
func (r *Records) Close() error {
	if r == nil {
		return nil
	}
	var err error
	r.closeOnce.Do(func() {
		if !r.persistent {
			return
		}
		close(r.stopCh)
		<-r.doneCh
		err = r.saveNow()
		_ = r.lock.Close()
	})
	return err
}

// Snapshot 返回全部记录的副本，按 id 升序。副本与内部记录不共享内存（调用方改动不会影响内部记录，
// 也就不会和锁外编码的写盘产生数据竞争）。
func (r *Records) Snapshot() []Record {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	ptrs := append([]*Record(nil), r.recs...)
	r.mu.Unlock()
	out := make([]Record, len(ptrs))
	for i, p := range ptrs {
		out[i] = *p
		out[i].Trigger = append([]TriggerItem(nil), p.Trigger...)
		out[i].Clients = append([]RecordClient(nil), p.Clients...)
	}
	return out
}

func (r *Records) now() int64 { return r.cfg.Now().Unix() }

func openKey(target, key string) string { return target + "\x00" + key }

// indexOf 按 id 二分查找记录下标；找不到返回 -1。调用方持锁。
func (r *Records) indexOf(id int64) int {
	i := sort.Search(len(r.recs), func(i int) bool { return r.recs[i].ID >= id })
	if i < len(r.recs) && r.recs[i].ID == id {
		return i
	}
	return -1
}

// start 为一次新生效的封禁开一条记录（分配 id）。调用方（List）持有名单锁。
func (r *Records) start(rec Record) {
	if r == nil {
		return
	}
	r.mu.Lock()
	k := openKey(rec.Target, rec.Key)
	if id, ok := r.open[k]; ok {
		// 名单开新封禁前总会先结束旧的（到期、解禁或覆盖），正常不会走到这里；唯一的合理来路是名单文件
		// 加载失败、名单退回纯内存：它不认识记录里仍生效的封禁。按对不上结束旧记录。
		// 用自己的一次性日志标志，别占用名单不变量的那一个。
		if recordsOverlapLogged.CompareAndSwap(false, true) {
			log.Printf("blocklist records: a ban started while the previous record of the same key was still open (expected only when the blocklist file failed to load); closed the old record as lost")
		}
		r.endLocked(k, r.indexOf(id), EndLost, rec.Since, autoActor)
	}
	if r.nextID > maxRecordID { // id 用尽（只可能来自被篡改的文件）：不再开新记录，免得发出下次加载会被丢弃的越界 id；
		// 同键旧记录的清理要在这之前做完，否则它会一直显示「封禁中」
		r.mu.Unlock()
		r.requestSave()
		if recordsIDExhaustedLogged.CompareAndSwap(false, true) {
			log.Printf("blocklist records: record id space exhausted (file tampered?); new bans are no longer recorded")
		}
		return
	}
	rec.ID = r.nextID
	r.nextID++
	r.recs = append(r.recs, &rec)
	r.open[k] = rec.ID
	r.trimIfNeededLocked()
	r.mu.Unlock()
	r.requestSave()
}

var recordsOverlapLogged, recordsIDExhaustedLogged atomic.Bool

// trimIfNeededLocked 已结束的记录超出上限一个余量才裁剪：裁剪是 O(n) 的，而开/结束记录时持有名单的写锁，
// 逐条裁剪会拖慢封禁洪峰期的 accept 热路径；留余量让代价摊还到 O(1)。写盘与加载时按上限精确裁剪。
func (r *Records) trimIfNeededLocked() {
	if len(r.recs)-len(r.open) > r.cfg.MaxRecords+r.cfg.MaxRecords/8 {
		r.trimLocked(r.now())
	}
}

// end 结束 (target, key) 上 ref 那次封禁的记录；对不上（没有记录，或 since / 计划时长不同）时什么也不做。
// 必须按整个计划匹配：同一秒内自动封禁被手动永久封禁覆盖、崩溃后名单文件停在覆盖之前，重启时旧封禁到期，
// 只比 since 会把到期记到新的手动永久记录头上。
func (r *Records) end(target, key string, ref banRef, end string, endedAt int64, actor string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	k := openKey(target, key)
	id, ok := r.open[k]
	i := -1
	if ok {
		i = r.indexOf(id)
	}
	if i < 0 || !ref.matches(r.recs[i]) {
		r.mu.Unlock()
		return
	}
	r.endLocked(k, i, end, endedAt, actor)
	r.trimIfNeededLocked()
	r.mu.Unlock()
	r.requestSave()
}

func (r *Records) endLocked(k string, i int, end string, endedAt int64, actor string) {
	delete(r.open, k)
	if i < 0 {
		return
	}
	cp := *r.recs[i]
	cp.End, cp.EndedAt, cp.EndActor = end, max(endedAt, cp.Since), actor
	r.recs[i] = &cp
}

// banRef 名单里一条封禁的关联键（对账用）：since 加计划时长，同一秒内被覆盖的封禁（自动 → 手动永久）
// since 相同、时长不同，只比 since 会把它们当成同一次封禁。
type banRef struct {
	since, until int64
	permanent    bool
}

func banRefOf(b *banRec) banRef {
	return banRef{since: b.Since, until: b.Until, permanent: b.Permanent}
}

func (b banRef) matches(x *Record) bool {
	return b.since == x.Since && b.until == x.Until && b.permanent == x.Permanent
}

// reconcile 启动时对账：target 下每条未结束记录，名单里对应 (key, since, 计划时长) 的封禁已不在的——计划已过的
// 按到期补记（ended_at=until），否则记 lost（ended_at=now）。返回名单里有封禁、却没有对应未结束记录的键（按键排序）。
func (r *Records) reconcile(target string, bans map[string]banRef, now int64) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	matched := make(map[string]bool, len(bans))
	changed := false
	for k, id := range r.open {
		i := r.indexOf(id)
		if i < 0 {
			delete(r.open, k)
			continue
		}
		x := r.recs[i]
		if x.Target != target {
			continue
		}
		if b, ok := bans[x.Key]; ok && b.matches(x) {
			matched[x.Key] = true
			continue
		}
		if !x.Permanent && x.Until <= now {
			r.endLocked(k, i, EndExpired, x.Until, autoActor)
		} else {
			r.endLocked(k, i, EndLost, now, autoActor)
		}
		changed = true
	}
	r.mu.Unlock()
	if changed {
		r.requestSave()
	}
	var missing []string
	for key := range bans {
		if !matched[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

// trimLocked 先按保留期丢已结束的记录，再把剩下的已结束记录按条数上限从最旧的丢起；生效中的不丢、也不计入条数
// （大规模封禁时生效记录多，不能因此把已结束的审计历史挤光）。O(n)。
func (r *Records) trimLocked(now int64) {
	cutoff := now - int64(r.cfg.Retention/time.Second)
	excess := -r.cfg.MaxRecords
	for _, x := range r.recs {
		if x.End != "" && x.EndedAt > cutoff {
			excess++ // 没过保留期的已结束记录；过了的本来就会被丢，不占名额
		}
	}
	kept := r.recs[:0]
	for _, x := range r.recs {
		if x.End != "" {
			if x.EndedAt <= cutoff {
				continue
			}
			if excess > 0 {
				excess--
				continue
			}
		}
		kept = append(kept, x)
	}
	for i := len(kept); i < len(r.recs); i++ {
		r.recs[i] = nil
	}
	r.recs = kept
}

// checkAgainst 校验记录与名单一致（测试用，见 List.checkInvariants）：target 下每条未结束记录都对得上一条
// 带封禁的条目，每个带封禁的条目都恰有一条对应的未结束记录，且两边的计划（since、until、permanent）一致；
// id 严格递增；索引与记录一致。
func (r *Records) checkAgainst(target string, entries map[string]*entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	openSeen := 0
	for i, x := range r.recs {
		if i > 0 && x.ID <= r.recs[i-1].ID {
			return fmt.Errorf("记录 id 不递增：%d 之后是 %d", r.recs[i-1].ID, x.ID)
		}
		if x.ID >= r.nextID {
			return fmt.Errorf("记录 id %d 不小于 next_id %d", x.ID, r.nextID)
		}
		if x.End != "" {
			continue
		}
		openSeen++
		if r.open[openKey(x.Target, x.Key)] != x.ID {
			return fmt.Errorf("未结束记录 #%d（%s）不在索引里", x.ID, x.Key)
		}
		if x.Target != target {
			continue
		}
		e := entries[x.Key]
		if e == nil || e.Ban == nil { // 计划是否一致由下面按名单条目的那一遍统一比对
			return fmt.Errorf("记录 #%d 显示 %s 封禁中（since %d），名单里没有对应封禁", x.ID, x.Key, x.Since)
		}
	}
	if openSeen != len(r.open) {
		return fmt.Errorf("未结束记录 %d 条，索引 %d 条", openSeen, len(r.open))
	}
	for k, e := range entries {
		if e.Ban == nil {
			continue
		}
		id, ok := r.open[openKey(target, k)]
		i := r.indexOf(id)
		if !ok || i < 0 {
			return fmt.Errorf("%s 的封禁（since %d）没有未结束记录", k, e.Ban.Since)
		}
		if x := r.recs[i]; !banRefOf(e.Ban).matches(x) {
			return fmt.Errorf("%s 的未结束记录（since %d until %d permanent %v）与名单里的封禁（since %d until %d permanent %v）对不上",
				k, x.Since, x.Until, x.Permanent, e.Ban.Since, e.Ban.Until, e.Ban.Permanent)
		}
	}
	return nil
}

// ---------- 名单侧挂钩（List 持有写锁时调用） ----------

// recordTarget 本名单在封禁记录里的对象类型。
func (l *List) recordTarget() string { return TargetIP }

// recordBanStart 为 e 上刚生效的封禁开一条记录。
func (l *List) recordBanStart(e *entry, offense int, trigger []TriggerItem, partial bool) {
	r, b := l.cfg.Records, e.Ban
	if r == nil || b == nil {
		return
	}
	r.start(Record{
		Target: l.recordTarget(), Key: e.IP,
		Reason: b.Reason, Actor: b.Actor, Note: b.Note,
		Trigger: trigger, TriggerPartial: partial, Clients: recordClients(e.Clients), Offense: offense,
		Since: b.Since, Until: b.Until, Permanent: b.Permanent,
	})
}

// recordBanEnd 结束 e 当前封禁对应的记录。必须在清掉或替换 e.Ban 之前调用。
func (l *List) recordBanEnd(e *entry, end string, endedAt int64, actor string) {
	if r := l.cfg.Records; r != nil && e.Ban != nil {
		r.end(l.recordTarget(), e.IP, banRefOf(e.Ban), end, endedAt, actor)
	}
}

// reconcileRecords 启动对账（在加载名单、清扫停机期间到期的封禁之后）：结束名单里已对不上的记录，
// 并为没有记录的生效封禁补开一条（首次升级、记录文件丢失或因预算丢了生效记录时）。
//
// 补开的自动封禁不带触发分项：名单文件里自动封禁条目只留最近 1 条事件，据此汇总出的分项会严重失真
// （例如 20 次失败显示成 1 次）；封禁说明（note）里已有当时的累计分值与最后一类事件。
func (l *List) reconcileRecords() {
	r := l.cfg.Records
	if r == nil {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bans := make(map[string]banRef)
	for k, e := range l.entries {
		if e.Ban != nil {
			bans[k] = banRefOf(e.Ban)
		}
	}
	for _, k := range r.reconcile(l.recordTarget(), bans, now) {
		e := l.entries[k]
		// 名单文件被篡改出的非法封禁（since 非正、原因不是 auto/manual）开不出合法记录：开了也会在下次加载时被
		// 丢弃，每次重启都丢一条、补一条，id 不断变化。
		if e.Ban.Since <= 0 || (e.Ban.Reason != "auto" && e.Ban.Reason != "manual") {
			continue
		}
		offense := 0
		if e.Ban.Reason == "auto" {
			if m := l.memOf(k).get(k); m != nil {
				offense = m.Offenses
			}
		}
		l.recordBanStart(e, offense, nil, false)
	}
}

// banTrigger 汇总构成一次自动封禁的事件：从 at 往前、窗口内计了分的事件（新的在前），累计到 points 分为止，
// 按类型+详情分组，分值高的在前（同分次数多的在前），最多 maxRecordTrigger 项。
//
// 计分命中不带类型，但事件与命中一一对应（同时追加、分值相同），所以按分值累计正好取到构成这次封禁的
// 那些事件；更早的、已被封禁/解禁/加白清零的分值不会混进来。事件只留最近 maxEventsPerIP 条，再早的无从统计。
//
// 第二个返回值 partial 表示没凑够 points 分就没有事件可统计了（事件只留最近 maxEventsPerIP 条，0 分事件还会占位），
// 此时分项只是部分构成。
func banTrigger(events []eventRec, at int64, window time.Duration, points int) (items []TriggerItem, partial bool) {
	cutoff := at - int64(window/time.Second)
	idx := make(map[string]int)
	sum := 0
	reached := false
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.TS > at {
			continue
		}
		if ev.TS <= cutoff {
			break
		}
		if ev.Points <= 0 {
			continue
		}
		k := ev.Kind + "\x00" + ev.Detail
		j, ok := idx[k]
		if !ok {
			j = len(items)
			idx[k] = j
			items = append(items, TriggerItem{Kind: ev.Kind, Detail: ev.Detail})
		}
		items[j].Count++
		items[j].Points += ev.Points
		if sum += ev.Points; sum >= points {
			reached = true
			break
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Points != items[b].Points {
			return items[a].Points > items[b].Points
		}
		return items[a].Count > items[b].Count
	})
	if len(items) > maxRecordTrigger {
		items = items[:maxRecordTrigger]
	}
	return items, !reached
}

// recordClients 最近出现的 maxRecordClients 个客户端，新的在前。
func recordClients(cs []clientRec) []RecordClient {
	if len(cs) == 0 {
		return nil
	}
	cp := append([]clientRec(nil), cs...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].LastSeen > cp[j].LastSeen })
	if len(cp) > maxRecordClients {
		cp = cp[:maxRecordClients]
	}
	out := make([]RecordClient, len(cp))
	for i, c := range cp {
		out[i] = RecordClient{Role: c.Role, Version: c.Version, Host: c.Host}
	}
	return out
}

// ---------- 持久化 ----------

func cleanStaleRecordTemps(abs string) {
	dir, prefix := filepath.Dir(abs), recordsTmpPrefix+filepath.Base(abs)+"-"
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range ents {
		if n := de.Name(); strings.HasPrefix(n, prefix) && strings.HasSuffix(n, tmpSuffix) {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}

func loadRecordsFile(path string) (*recordsFile, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read blocklist records file: %w", err)
	}
	defer f.Close()
	if err := validatePrivateFile(path); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxRecordsFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read blocklist records file: %w", err)
	}
	if len(raw) > maxRecordsFileBytes {
		return nil, fmt.Errorf("blocklist records file %s exceeds %d bytes; refusing to load", path, maxRecordsFileBytes)
	}
	// 不开 DisallowUnknownFields：记录只是审计辅助数据，以后加字段再回滚到本版时应照常加载（丢掉不认识的字段），
	// 而不是整份拒绝、记录功能停摆。
	dec := json.NewDecoder(bytes.NewReader(raw))
	var fd recordsFile
	if err := dec.Decode(&fd); err != nil {
		return nil, fmt.Errorf("blocklist records file %s is corrupt: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("blocklist records file %s is corrupt: trailing data", path)
	}
	if fd.Schema != recordsSchema {
		return nil, fmt.Errorf("blocklist records file %s has unsupported schema %d", path, fd.Schema)
	}
	return &fd, nil
}

var validEnds = map[string]bool{"": true, EndExpired: true, EndUnban: true, EndEvicted: true, EndReplaced: true, EndLost: true}

// validRecord 单条记录的结构校验；不合格的只丢该条。
func validRecord(x *Record) bool {
	if x == nil || x.ID <= 0 || x.ID > maxRecordID || x.Since <= 0 || (x.Reason != "auto" && x.Reason != "manual") || !validEnds[x.End] {
		return false
	}
	if (x.End == "") != (x.EndedAt == 0) {
		return false
	}
	switch x.Target {
	case TargetIP:
		k, err := NormalizeKey(x.Key)
		return err == nil && k == x.Key
	case TargetDevice:
		return isDeviceKey(x.Key)
	}
	return false
}

// isDeviceKey 设备键：dev- 加 24 位小写十六进制。
func isDeviceKey(k string) bool {
	if len(k) != 28 || !strings.HasPrefix(k, "dev-") {
		return false
	}
	for _, c := range k[4:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sanitizeRecord 文件里的字符串只用于展示；截断到与运行时相同的长度上限，防篡改文件撑大内存。
func sanitizeRecord(x *Record) {
	x.Label, x.Via = cleanField(x.Label), cleanField(x.Via)
	x.Actor, x.EndActor = truncateBytes(cleanText(x.Actor), maxRecordField), truncateBytes(cleanText(x.EndActor), maxRecordField)
	x.Note = cleanNote(x.Note)
	if len(x.Trigger) > maxRecordTrigger {
		x.Trigger = x.Trigger[:maxRecordTrigger]
	}
	for i := range x.Trigger {
		x.Trigger[i].Kind = truncateBytes(cleanText(x.Trigger[i].Kind), maxRecordField)
		x.Trigger[i].Detail = truncateBytes(cleanText(x.Trigger[i].Detail), maxRecordField)
	}
	if len(x.Clients) > maxRecordClients {
		x.Clients = x.Clients[:maxRecordClients]
	}
	for i := range x.Clients {
		c := &x.Clients[i]
		c.Role, c.Version, c.Host = cleanField(c.Role), cleanField(c.Version), cleanField(c.Host)
	}
}

// load 装入文件内容：单条坏记录（结构不合格、id 重复）只丢该条并记日志；同一对象出现多条未结束记录时
// 只认最新一条，其余按对不上（lost）结束；最后按当前上限裁剪。
func (r *Records) load(fd *recordsFile, now int64) {
	recs := make([]*Record, 0, len(fd.Records))
	dropped := 0
	for _, x := range fd.Records {
		if !validRecord(x) {
			dropped++
			continue
		}
		sanitizeRecord(x)
		recs = append(recs, x)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	out := recs[:0]
	for _, x := range recs {
		if len(out) > 0 && out[len(out)-1].ID == x.ID {
			dropped++
			continue
		}
		out = append(out, x)
	}
	r.recs = out
	r.nextID = 1
	if fd.NextID > 1 && fd.NextID <= maxRecordID+1 { // 超出范围的 next_id 是篡改，忽略
		r.nextID = fd.NextID
	}
	if n := len(out); n > 0 {
		r.nextID = max(r.nextID, out[n-1].ID+1)
	}
	dupOpen := 0
	for i, x := range r.recs {
		if x.End != "" {
			continue
		}
		k := openKey(x.Target, x.Key)
		if id, ok := r.open[k]; ok {
			r.endLocked(k, r.indexOf(id), EndLost, now, autoActor)
			dupOpen++
		}
		r.open[k] = r.recs[i].ID
	}
	r.trimLocked(now)
	if dropped > 0 || dupOpen > 0 {
		log.Printf("blocklist records: file %s: dropped %d invalid or duplicate records, closed %d duplicate open records; the rest was loaded", r.path, dropped, dupOpen)
	}
}

func (r *Records) requestSave() {
	if !r.persistent {
		return
	}
	select {
	case r.saveCh <- struct{}{}:
	default:
	}
}

// saver 合并写盘：两次写之间至少隔 SaveInterval；失败按退避重试。
func (r *Records) saver() {
	defer close(r.doneCh)
	var last time.Time
	failures := 0
	var retry <-chan time.Time
	for {
		select {
		case <-r.stopCh:
			return
		case <-r.saveCh:
		case <-retry:
		}
		retry = nil
		if wait := time.Until(last.Add(r.cfg.SaveInterval)); wait > 0 {
			select {
			case <-r.stopCh:
				return
			case <-time.After(wait):
			}
		}
		if err := r.saveNow(); err != nil {
			d := r.cfg.retryBackoff[min(failures, len(r.cfg.retryBackoff)-1)]
			failures++
			if failures == 1 || failures%10 == 0 {
				log.Printf("blocklist records: save failed (%d consecutive), retrying in %s: %v", failures, d, err)
			}
			retry = time.After(d)
		} else {
			if failures > 0 {
				log.Printf("blocklist records: save succeeded after %d failed attempt(s)", failures)
			}
			failures = 0
		}
		last = time.Now()
	}
}

// saveNow 按保留规则裁剪后原子写盘。锁内只做裁剪与指针复制，编码在锁外。
func (r *Records) saveNow() error {
	if !r.persistent {
		return nil
	}
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	now := r.now()
	r.mu.Lock()
	r.trimLocked(now)
	snap := append([]*Record(nil), r.recs...)
	nextID := r.nextID
	r.mu.Unlock()
	data, openDropped, endedDropped, err := encodeRecords(nextID, now, snap, r.cfg.saveBudget, r.cfg.hardBudget)
	if err != nil {
		return err
	}
	if openDropped > 0 || endedDropped > 0 {
		log.Printf("blocklist records: persisted records exceed the budget; dropped %d oldest active records (restored from the blocklist on restart) and %d oldest ended records from the file", openDropped, endedDropped)
	}
	write := r.cfg.atomicWrite
	if write == nil {
		write = writeFileAtomicPattern
	}
	if err := write(r.path, data, recordsTmpPrefix+filepath.Base(r.path)+"-*"+tmpSuffix); err != nil {
		return err
	}
	r.saveCount.Add(1)
	return nil
}

// encodeRecords 按 id 升序拼出文件：生效中的记录从新到旧装，最多占 hard-budget 字节；已结束的从新到旧装，
// 另外最多占 budget 字节（遇到第一条装不下的就停）。整个文件不超过 hard。
// 两类各有各的额度：生效记录多（大规模封禁）时，已结束的审计历史不会被挤出文件。
func encodeRecords(nextID, now int64, recs []*Record, budget, hard int) (data []byte, openDropped, endedDropped int, err error) {
	head := fmt.Sprintf(`{"schema":%d,"next_id":%d,"saved_at":%d,"records":[`, recordsSchema, nextID, now)
	const tail = `]}`
	used := len(head) + len(tail)
	type item struct {
		id int64
		b  []byte
	}
	var items []item
	pass := func(active bool, limit int) (dropped int, err error) {
		full := false
		for i := len(recs) - 1; i >= 0; i-- {
			x := recs[i]
			if (x.End == "") != active {
				continue
			}
			if full {
				dropped++
				continue
			}
			b, err := marshalNoEscape(x)
			if err != nil {
				return 0, err
			}
			if used+len(b)+1 > limit {
				full = true
				dropped++
				continue
			}
			used += len(b) + 1
			items = append(items, item{x.ID, b})
		}
		return dropped, nil
	}
	if openDropped, err = pass(true, max(hard-budget, hard/2)); err != nil {
		return nil, 0, 0, err
	}
	if endedDropped, err = pass(false, used+min(budget, max(hard-used, 0))); err != nil {
		return nil, 0, 0, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	var out bytes.Buffer
	out.Grow(used)
	out.WriteString(head)
	for i, it := range items {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(it.b)
	}
	out.WriteString(tail)
	return out.Bytes(), openDropped, endedDropped, nil
}
