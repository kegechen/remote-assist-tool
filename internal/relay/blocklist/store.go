package blocklist

import (
	"bytes"
	"container/list"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// 持久化格式：单个 JSON 文件，原子写（同目录临时文件 + fsync + rename + 目录 fsync），权限 0600。
// 选择 JSON 而非 SQLite：数据量有界（默认上限下不足 4 MiB）、写盘已合并、不引入依赖、
// 出问题能直接查看和手工修复；临时文件 + fsync + rename 保证崩溃后只会看到旧的或新的完整文件。
const (
	fileSchema = 2
	// maxFileBytes 拒绝加载的文件大小上限。按默认上限估算正常文件 < 4 MiB，留足余量；
	// 超限多半是被篡改或损坏，整读进内存会 OOM，直接拒绝并退回纯内存模式。
	maxFileBytes = 32 << 20
	// maxSaveBytes 写盘字节预算，必须小于 maxFileBytes：绝不写出下次启动加载不了的文件
	// （否则重启后所有手动封禁与白名单丢失、持久化长期停用）。超出时按优先级丢弃，见 saveNow。
	maxSaveBytes = 24 << 20
	// memReserveBytes 写盘预算中为记忆预留的上限（实际预留取它、记忆实际大小与预算 1/8 的较小者）。
	memReserveBytes = 3 << 20
	// snapshotChunk 写盘快照每次持读锁处理的条目数。
	snapshotChunk = 512
	tmpPrefix     = ".blocklist-"
	tmpSuffix     = ".tmp"
)

type fileData struct {
	Schema  int       `json:"schema"`
	Salt    string    `json:"salt"`
	SavedAt int64     `json:"saved_at"`
	Entries []*entry  `json:"entries"`
	Memory  []*memRec `json:"memory"`
}

type loadedData struct {
	salt    []byte
	entries []*entry
	mem     []*memRec
}

// openStore 取得进程锁、清理上次崩溃残留的临时文件并加载文件。文件不存在返回 (lock, nil, nil)。
// 任何错误都会释放锁，且绝不修改原文件。
func openStore(path string) (*fileLock, *loadedData, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve blocklist path: %w", err)
	}
	lock, err := acquireFileLock(abs + ".lock")
	if err != nil {
		return nil, nil, err
	}
	cleanStaleTemps(filepath.Dir(abs))
	data, err := loadFile(abs)
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	return lock, data, nil
}

// cleanStaleTemps 删除写盘中途崩溃留下的 .blocklist-*.tmp（只匹配本包自己的命名）。
func cleanStaleTemps(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, tmpPrefix+"*"+tmpSuffix))
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasPrefix(base, tmpPrefix) && strings.HasSuffix(base, tmpSuffix) {
			_ = os.Remove(m)
		}
	}
}

func loadFile(path string) (*loadedData, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read blocklist file: %w", err)
	}
	defer f.Close()
	if err := validatePrivateFile(path); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read blocklist file: %w", err)
	}
	if len(raw) > maxFileBytes {
		return nil, fmt.Errorf("blocklist file %s exceeds %d bytes; refusing to load", path, maxFileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fd fileData
	if err := dec.Decode(&fd); err != nil {
		return nil, fmt.Errorf("blocklist file %s is corrupt: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("blocklist file %s is corrupt: trailing data", path)
	}
	if fd.Schema == 1 {
		return nil, fmt.Errorf("blocklist file %s has schema 1, a development-only format that is not migrated; rename or delete the file and restart (the relay keeps running in memory-only mode and will not overwrite it)", path)
	}
	if fd.Schema != fileSchema {
		return nil, fmt.Errorf("blocklist file %s has unsupported schema %d", path, fd.Schema)
	}
	salt, err := hex.DecodeString(fd.Salt)
	if err != nil || len(salt) != 32 {
		return nil, fmt.Errorf("blocklist file %s has an invalid salt", path)
	}
	// 单条非法/非规范/重复的键只丢弃该条并记日志（重复的保留第一条），绝不因一条坏键拒绝整份文件：
	// 那会让所有手动封禁和白名单随之丢失、持久化停用。结构性损坏（JSON 语法、未知字段、schema、salt）
	// 仍整份拒绝。
	entries := make([]*entry, 0, len(fd.Entries))
	seen := make(map[string]bool, len(fd.Entries))
	var dropped []string
	for _, e := range fd.Entries {
		if e == nil {
			dropped = append(dropped, "<null>")
			continue
		}
		key, err := NormalizeKey(e.IP)
		if err != nil || key != e.IP || seen[key] {
			dropped = append(dropped, e.IP)
			continue
		}
		seen[key] = true
		entries = append(entries, e)
	}
	mems := make([]*memRec, 0, len(fd.Memory))
	memSeen := make(map[string]bool, len(fd.Memory))
	droppedMem := 0
	for _, r := range fd.Memory {
		if r == nil {
			droppedMem++
			continue
		}
		key, err := NormalizeKey(r.Key)
		if err != nil || key != r.Key || memSeen[key] {
			droppedMem++
			continue
		}
		memSeen[key] = true
		mems = append(mems, r)
	}
	if len(dropped) > 0 || droppedMem > 0 {
		sample := dropped
		if len(sample) > 10 {
			sample = sample[:10]
		}
		for i := range sample {
			sample[i] = truncateBytes(sample[i], 64) // 键来自文件，可能被篡改得很长
		}
		log.Printf("blocklist: file %s: dropped %d entries with invalid or duplicate keys (e.g. %q) and %d memory records; the rest was loaded", path, len(dropped), sample, droppedMem)
	}
	return &loadedData{salt: salt, entries: entries, mem: mems}, nil
}

// requestSave 通知后台 saver 写盘。urgent（人工处置）用更短的最小间隔。纯内存模式下为 no-op。
func (l *List) requestSave(urgent bool) {
	if !l.persistent {
		return
	}
	if urgent {
		l.urgent.Store(true)
	}
	select {
	case l.saveCh <- struct{}{}:
	default:
	}
}

// saver 合并写盘：两次写之间至少隔 SaveInterval（人工处置 UrgentSaveInterval），
// 洪峰期间无论多少次变更都只按间隔写。
func (l *List) saver() {
	defer close(l.doneCh)
	var last time.Time
	failures := 0
	backoff := l.retryBackoff
	if len(backoff) == 0 {
		backoff = defaultRetryBackoff
	}
	var retry <-chan time.Time // 非 nil 表示有一次失败待重试
	for {
		select {
		case <-l.stopCh:
			return
		case <-l.saveCh:
		case <-retry:
		}
		retry = nil
		for {
			interval := l.cfg.SaveInterval
			if l.urgent.Load() {
				interval = l.cfg.UrgentSaveInterval
			}
			wait := time.Until(last.Add(interval))
			if wait <= 0 {
				break
			}
			if wait > 50*time.Millisecond {
				wait = 50 * time.Millisecond // 期间可能升级为 urgent，分段等待
			}
			select {
			case <-l.stopCh:
				return
			case <-time.After(wait):
			}
		}
		l.urgent.Store(false)
		if err := l.saveNow(); err != nil {
			l.saveFailures.Add(1)
			// 连续失败按退避持续重试（默认 5s、30s、之后每 5 分钟），恢复后无需新的变更也会写成功。
			d := backoff[min(failures, len(backoff)-1)]
			failures++
			if failures == 1 || failures%10 == 0 {
				log.Printf("blocklist: save failed (%d consecutive), retrying in %s: %v", failures, d, err)
			}
			retry = time.After(d)
		} else {
			if failures > 0 {
				log.Printf("blocklist: save succeeded after %d failed attempt(s)", failures)
			}
			failures = 0
		}
		last = time.Now()
	}
}

// defaultRetryBackoff 写盘连续失败时的重试间隔：逐次递增，最后一个封顶。
var defaultRetryBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}

// snapArena 写盘快照的批量分配区：读锁内不做逐条的小分配（上限规模下两族 1.6 万条自动封禁，
// 逐条分配会让读锁持有数十毫秒、拖住 accept）。
type snapArena struct {
	ents []entry
	bans []banRec
	ev   []eventRec
	hist []historyRec
	cl   []clientRec
}

func newSnapArena(n int) *snapArena {
	return &snapArena{
		ents: make([]entry, 0, n),
		bans: make([]banRec, 0, n),
		ev:   make([]eventRec, 0, n*persistAutoEvents),
		hist: make([]historyRec, 0, n*persistAutoHistory),
		cl:   make([]clientRec, 0, n*persistAutoClient),
	}
}

// snapshotEntry 逐字段复制条目用于持久化，只取需要的尾部（maxEvents/maxClients/maxHistory 为 0 表示全取）。
// 不排序：自动封禁只取最近的 maxClients 个客户端，用 O(n) 选择。Rejected 在读锁下被 CheckAccept 原子递增，
// 必须单独原子读取，不能随结构体整体复制（数据竞争）。a 非 nil 时从批量分配区取内存。
func snapshotEntry(e *entry, maxEvents, maxClients, maxHistory int, a *snapArena) *entry {
	var cp *entry
	if a != nil && len(a.ents) < cap(a.ents) {
		a.ents = append(a.ents, entry{})
		cp = &a.ents[len(a.ents)-1]
	} else {
		cp = &entry{}
	}
	cp.IP, cp.FirstSeen, cp.LastSeen, cp.Connections = e.IP, e.FirstSeen, e.LastSeen, e.Connections
	cp.Rejected = atomic.LoadInt64(&e.Rejected)
	cp.GraceUntil, cp.BanCount, cp.heapIdx = e.GraceUntil, e.BanCount, -1
	events := e.Events
	if maxEvents > 0 && len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	history := e.History
	if maxHistory > 0 && len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
	}
	clients := e.Clients
	if maxClients == 1 && len(clients) > 1 {
		best := 0
		for i := range clients {
			if clients[i].LastSeen > clients[best].LastSeen {
				best = i
			}
		}
		clients = clients[best : best+1]
	} else if maxClients > 0 && len(clients) > maxClients {
		clients = latestClients(clients, maxClients)
	}
	if a != nil {
		cp.Events = arenaCopy(&a.ev, events)
		cp.History = arenaCopy(&a.hist, history)
		cp.Clients = arenaCopy(&a.cl, clients)
	} else {
		cp.Events = append([]eventRec(nil), events...)
		cp.History = append([]historyRec(nil), history...)
		cp.Clients = append([]clientRec(nil), clients...)
	}
	if e.Ban != nil {
		if a != nil && len(a.bans) < cap(a.bans) {
			a.bans = append(a.bans, *e.Ban)
			cp.Ban = &a.bans[len(a.bans)-1]
		} else {
			b := *e.Ban
			cp.Ban = &b
		}
	}
	if e.Allow != nil {
		al := *e.Allow
		cp.Allow = &al
	}
	return cp
}

// arenaCopy 把 src 追加进分配区并返回独立的子切片（三下标切片，后续追加不会覆盖它）。
func arenaCopy[T any](buf *[]T, src []T) []T {
	if len(src) == 0 {
		return nil
	}
	start := len(*buf)
	*buf = append(*buf, src...)
	return (*buf)[start:len(*buf):len(*buf)]
}

// latestClients 最近出现的 n 个客户端（n 很小，部分选择排序）。
func latestClients(clients []clientRec, n int) []clientRec {
	cp := append([]clientRec(nil), clients...)
	for i := 0; i < n; i++ {
		best := i
		for j := i + 1; j < len(cp); j++ {
			if cp[j].LastSeen > cp[best].LastSeen {
				best = j
			}
		}
		cp[i], cp[best] = cp[best], cp[i]
	}
	return cp[:n]
}

// saveNow 生成可持久化状态的快照并原子写入。saveMu 串行化写盘，避免旧快照覆盖新快照。
//
// 读锁内只做逐字段复制（不排序、不编码），编码与预算裁剪都在锁外完成。
//
// 持久化内容与优先级（写盘字节预算 maxSaveBytes 内按优先级装入，超出的按相反顺序丢弃并记日志）：
//  1. 人工钉住条目：手动封禁、白名单、宽限期（含叠加了白名单/宽限的自动封禁）。完整写入；预算不够时
//     降级为精简形式（不含事件、1 个客户端、最近 5 条历史），仍不够才丢弃——正常规模下不会发生。
//  2. 只有自动封禁的条目：只留最近 5 条事件、2 个客户端；按到期时间倒序，最早到期的最先被丢弃。
//  3. 递增/成功客户端记忆：最近使用的优先。
//  4. 最近 30 天内有人工处置历史的非钉住条目（最多 1000 条），最先被丢弃。
//
// 客户端自报的版本/身份串保存时截断到 64 字节，编码不做 HTML 转义（'<' 不会膨胀成 6 字节）。
func (l *List) saveNow() error {
	if !l.persistent {
		return nil
	}
	l.saveMu.Lock()
	defer l.saveMu.Unlock()
	now := l.now()
	cutoff := now - int64(historyRetention/time.Second)
	var manual, autos, others []*entry
	// 第一遍：读锁内只收集条目指针与记忆链表元素指针，不分配、不复制记录。容量在加锁前按上限预分配：
	// 在读锁内分配几万个元素的切片会触发 GC 协助，偶发把持锁时间拉长数毫秒。
	ptrs := make([]*entry, 0, l.hardCap()+64)
	memEls := make([]*list.Element, 0, 2*l.cfg.MaxMemory)
	l.mu.RLock()
	lockStart := time.Now()
	for _, e := range l.entries {
		ptrs = append(ptrs, e)
	}
	for _, mem := range l.mem {
		for el := mem.ll.Back(); el != nil; el = el.Prev() { // 最近使用的在前
			memEls = append(memEls, el)
		}
	}
	l.mu.RUnlock()
	hold := time.Since(lockStart)
	// 记忆分块复制：块之间被淘汰的记录（map 里已不指向该元素）跳过。
	mems := make([]memRec, 0, len(memEls))
	for i := 0; i < len(memEls); i += snapshotChunk {
		l.mu.RLock()
		chunkStart := time.Now()
		for _, el := range memEls[i:min(i+snapshotChunk, len(memEls))] {
			r := el.Value.(*memRec)
			if l.memOf(r.Key).m[r.Key] != el {
				continue
			}
			mems = append(mems, *r)
		}
		l.mu.RUnlock()
		hold = max(hold, time.Since(chunkStart))
	}
	// 第二遍：分块持读锁做逐条快照，每块之间释放锁，写锁（以及排在它后面的 accept）最多等一块。
	// 每个条目的快照本身一致；块之间条目可能变化或被淘汰（不在主表的跳过），文件本就是某一刻附近的近似快照。
	arena := newSnapArena(len(ptrs))
	autos = make([]*entry, 0, len(ptrs))
	for i := 0; i < len(ptrs); i += snapshotChunk {
		l.mu.RLock()
		chunkStart := time.Now()
		for _, e := range ptrs[i:min(i+snapshotChunk, len(ptrs))] {
			if l.entries[e.IP] != e {
				continue
			}
			switch {
			case e.Ban.active(now) && e.Ban.Reason == "auto" && !e.Allow.active(now) && e.GraceUntil <= now:
				autos = append(autos, snapshotEntry(e, persistAutoEvents, persistAutoClient, persistAutoHistory, arena))
			case e.pinned(now):
				manual = append(manual, snapshotEntry(e, 0, 0, 0, nil))
			default:
				if e.lastManualTS() > cutoff {
					others = append(others, snapshotEntry(e, persistAutoEvents, persistAutoClient, 0, nil))
				}
			}
		}
		l.mu.RUnlock()
		hold = max(hold, time.Since(chunkStart))
	}
	l.snapshotHold.Store(int64(hold))

	budget := l.saveBudget
	if budget <= 0 {
		budget = maxSaveBytes
	}
	data, report, err := encodeWithBudget(hex.EncodeToString(l.salt), now, manual, autos, mems, others, budget)
	if err != nil {
		return err
	}
	if report.any() {
		log.Printf("blocklist: persisted state exceeds the %d-byte budget; %s", budget, report)
	}
	if err := writeFileAtomic(l.path, data); err != nil {
		return err
	}
	l.saveCount.Add(1)
	l.lastSaveReport.Store(&report)
	return nil
}

// budgetReport 一次写盘中因预算被丢弃/降级的数量。
type budgetReport struct {
	ManualCompacted, ManualDropped, AutoDropped, MemoryDropped, HistoryDropped int
}

func (r budgetReport) any() bool {
	return r.ManualCompacted+r.ManualDropped+r.AutoDropped+r.MemoryDropped+r.HistoryDropped > 0
}

func (r budgetReport) String() string {
	return fmt.Sprintf("compacted %d manual entries, dropped %d manual entries, %d auto bans (earliest-expiring first), %d memory records, %d history-only entries",
		r.ManualCompacted, r.ManualDropped, r.AutoDropped, r.MemoryDropped, r.HistoryDropped)
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func validKeys(es []*entry) []*entry {
	out := es[:0]
	for _, e := range es {
		if k, err := NormalizeKey(e.IP); err != nil || k != e.IP {
			log.Printf("blocklist: skipping non-canonical key %q on save", truncateBytes(e.IP, 64))
			continue
		}
		out = append(out, e)
	}
	return out
}

func validMemKeys(ms []memRec) []memRec {
	out := ms[:0]
	for _, m := range ms {
		if k, err := NormalizeKey(m.Key); err != nil || k != m.Key {
			log.Printf("blocklist: skipping non-canonical memory key %q on save", truncateBytes(m.Key, 64))
			continue
		}
		out = append(out, m)
	}
	return out
}

// encodeWithBudget 按优先级把各部分装入 budget 字节内并拼出完整文件。
func encodeWithBudget(salt string, now int64, manual, autos []*entry, mems []memRec, others []*entry, budget int) ([]byte, budgetReport, error) {
	var rep budgetReport
	head := fmt.Sprintf(`{"schema":%d,"salt":%q,"saved_at":%d,"entries":[`, fileSchema, salt, now)
	const mid, tail = `],"memory":[`, `]}`
	used := len(head) + len(mid) + len(tail)
	var entries, memory [][]byte
	fits := func(b []byte) bool { return used+len(b)+1 <= budget }
	take := func(dst *[][]byte, b []byte) {
		*dst = append(*dst, b)
		used += len(b) + 1
	}

	// 写盘前自检：每个键都必须能被 NormalizeKey 原样接受，否则下次启动会被丢弃——在这里先跳过并记日志，
	// 便于尽早发现键规则不一致。
	manual, autos, others = validKeys(manual), validKeys(autos), validKeys(others)
	mems = validMemKeys(mems)
	sort.Slice(manual, func(i, j int) bool { return manual[i].lastManualTS() > manual[j].lastManualTS() })
	for _, e := range manual {
		b, err := marshalNoEscape(e)
		if err != nil {
			return nil, rep, err
		}
		if !fits(b) {
			c := *e
			c.Events = nil
			if len(c.Clients) > 1 {
				c.Clients = c.Clients[len(c.Clients)-1:]
			}
			if len(c.History) > 5 {
				c.History = c.History[len(c.History)-5:]
			}
			if b, err = marshalNoEscape(&c); err != nil {
				return nil, rep, err
			}
			if !fits(b) {
				rep.ManualDropped++
				continue
			}
			rep.ManualCompacted++
		}
		take(&entries, b)
	}
	// 记忆先编码，给它预留最多 memReserveBytes：恶意填充把自动封禁撑大时，递增次数与成功客户端记忆
	// 不至于被整体挤掉。
	memBytes := make([][]byte, len(mems))
	memTotal := 0
	for i := range mems {
		b, err := marshalNoEscape(&mems[i])
		if err != nil {
			return nil, rep, err
		}
		memBytes[i] = b
		memTotal += len(b) + 1
	}
	reserve := min(memTotal, memReserveBytes, budget/8)
	sort.Slice(autos, func(i, j int) bool { return autos[i].Ban.Until > autos[j].Ban.Until })
	for i, e := range autos {
		b, err := marshalNoEscape(e)
		if err != nil {
			return nil, rep, err
		}
		if used+len(b)+1 > budget-reserve {
			rep.AutoDropped = len(autos) - i
			break
		}
		take(&entries, b)
	}
	for i, b := range memBytes {
		if !fits(b) {
			rep.MemoryDropped = len(mems) - i
			break
		}
		take(&memory, b)
	}
	sort.Slice(others, func(i, j int) bool { return others[i].lastManualTS() > others[j].lastManualTS() })
	if len(others) > maxPersistHistory {
		others = others[:maxPersistHistory]
	}
	for i, e := range others {
		b, err := marshalNoEscape(e)
		if err != nil {
			return nil, rep, err
		}
		if !fits(b) {
			rep.HistoryDropped = len(others) - i
			break
		}
		take(&entries, b)
	}
	// 记忆按"最旧在前"写出：加载时按顺序 touch，最近使用的落在 LRU 队尾。
	for i, j := 0, len(memory)-1; i < j; i, j = i+1, j-1 {
		memory[i], memory[j] = memory[j], memory[i]
	}
	var out bytes.Buffer
	out.Grow(used)
	out.WriteString(head)
	out.Write(bytes.Join(entries, []byte{','}))
	out.WriteString(mid)
	out.Write(bytes.Join(memory, []byte{','}))
	out.WriteString(tail)
	return out.Bytes(), rep, nil
}

func writeFileAtomic(path string, data []byte) error {
	return writeFileAtomicPattern(path, data, tmpPrefix+"*"+tmpSuffix)
}

// writeFileAtomicPattern 同 writeFileAtomic，临时文件按 pattern（os.CreateTemp 的命名模式）创建。
func writeFileAtomicPattern(path string, data []byte, pattern string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp file: %w", err)
	}
	_ = os.Chmod(path, 0600)
	// rename 本身要落盘：Unix 上 fsync 所在目录，否则掉电后可能回到旧文件甚至丢失目录项。
	syncDir(dir)
	return nil
}
