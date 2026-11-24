package blocklist

import (
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 黑名单列表的搜索 / 筛选 / 排序 / 游标分页（Web 控制台的只读查询路径）。
//
// 与 accept 热路径（CheckAccept / IsBanned / ObserveConnect / RecordViolation）完全无关：这里不新增
// 任何索引、不改任何热路径的数据结构，也不在写路径上多做一点事；所有额外工作只发生在管理员点开
// 列表的那一次 Query 里。Query 的锁纪律：读锁内只做一次 O(n) 扫描（n ≤ 主表硬上限），每个条目
// 做常数次判断并拷出排序键；排序、游标定位、分页都在锁外；第二次读锁只为页内条目（≤ MaxPageLimit）
// 生成摘要。

// 列表排序方式与方向。
const (
	SortStatus   = "status"    // 默认：状态优先（已封禁 > 白名单 > 宽限期 > 观察中 > 正常），再按最近出现
	SortLastSeen = "last_seen" // 最近出现
	SortBannedAt = "banned_at" // 封禁时间（Ban.Since）；没有生效封禁的条目排最后
	SortExpires  = "expires"   // 封禁到期时间；永久封禁视为最晚；没有生效封禁的条目排最后
	SortPoints   = "points"    // 窗口内违规分

	OrderAsc  = "asc"
	OrderDesc = "desc" // 默认。desc = 「更近 / 更晚 / 更高」在前；SortStatus 的 desc 即默认顺序（已封禁在前）
)

// MaxSearchLen 搜索词最大字符数（按 rune 计）。
const MaxSearchLen = 64

// 游标最大字节数（解码后）。
const maxCursorLen = 192

var (
	// ErrInvalidCursor 列表游标无法解析。
	ErrInvalidCursor = errors.New("invalid cursor")
	// ErrInvalidFilter 列表筛选 / 排序参数非法。
	ErrInvalidFilter = errors.New("invalid filter")
)

// Validate 校验 Filter 里的 Sort / Order / Kind / Search（Status、Reason、Cursor 由调用方校验）。
// Query 对非法值的处理是退回默认（不报错），Web 层应先调用本方法并返回 400。
func (f Filter) Validate() error {
	if !validSort(f.Sort) {
		return fmt.Errorf("%w: sort must be one of status, last_seen, banned_at, expires, points", ErrInvalidFilter)
	}
	if f.Order != "" && f.Order != OrderAsc && f.Order != OrderDesc {
		return fmt.Errorf("%w: order must be asc or desc", ErrInvalidFilter)
	}
	if f.Kind != "" {
		if _, ok := kindWeight[Kind(f.Kind)]; !ok {
			return fmt.Errorf("%w: unknown kind", ErrInvalidFilter)
		}
	}
	if utf8.RuneCountInString(f.Search) > MaxSearchLen {
		return fmt.Errorf("%w: search too long (max %d characters)", ErrInvalidFilter, MaxSearchLen)
	}
	return nil
}

func validSort(s string) bool {
	switch s {
	case "", SortStatus, SortLastSeen, SortBannedAt, SortExpires, SortPoints:
		return true
	}
	return false
}

// sortSpec 归一化排序参数：空 / 未知排序 = SortStatus，空 / 未知方向 = desc。
func (f Filter) sortSpec() (name string, asc bool) {
	name = f.Sort
	if name == "" || !validSort(name) {
		name = SortStatus
	}
	return name, f.Order == OrderAsc
}

func normSearch(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// fingerprint 是筛选条件（状态、来源、触发原因、搜索词）的短指纹，写进游标：
// 游标只对产生它的那组筛选有效。
func (f Filter) fingerprint(search string) string {
	status := f.Status
	switch status {
	case "banned", "allowlisted", "grace", "suspect", "tracked":
	default:
		status = "all"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.Join([]string{status, f.Reason, f.Kind, search}, "\x00")))
	return fmt.Sprintf("%08x", h.Sum32())
}

// pageKey 是列表的排序键，全序（ip 唯一，作最后的 tie-breaker）。
//
// 比较顺序：值缺失(null)的条目永远排最后（与方向无关）；其余先比 v1、再比 v2，desc 时大的在前、
// asc 时小的在前；最后比 ip，desc 时升序、asc 时降序（asc 即 desc 的整体反转，null 组除外）。
// 各排序方式的取值：
//
//	status    v1 = 4 - 状态等级（已封禁最大）, v2 = last_seen            —— desc 即旧版默认顺序
//	last_seen v1 = last_seen
//	banned_at v1 = Ban.Since（无生效封禁 = null）
//	expires   v1 = Ban.Until（永久 = MaxInt64，无生效封禁 = null）
//	points    v1 = 窗口内分值, v2 = last_seen
type pageKey struct {
	null   bool
	v1, v2 int64
	ip     string
}

func (a pageKey) less(b pageKey, asc bool) bool {
	if a.null != b.null {
		return !a.null
	}
	if !a.null {
		if a.v1 != b.v1 {
			return (a.v1 < b.v1) == asc
		}
		if a.v2 != b.v2 {
			return (a.v2 < b.v2) == asc
		}
	}
	if a.ip != b.ip {
		return (a.ip < b.ip) != asc
	}
	return false
}

// recentPoints 窗口内分值，与 entry.points 同口径。命中按时间追加，从尾部往回数，遇到窗口外就停
// （和 quickStatus 一样依赖这一点），不必扫完最多 256 条。
func recentPoints(e *entry, cutoff int64) int {
	sum := 0
	for i := len(e.hits) - 1; i >= 0 && e.hits[i].ts > cutoff; i-- {
		sum += e.hits[i].weight
	}
	return sum
}

func keyFor(e *entry, sortName, status string, now, cutoff int64) pageKey {
	k := pageKey{ip: e.IP}
	switch sortName {
	case SortLastSeen:
		k.v1 = e.LastSeen
	case SortBannedAt:
		if !e.Ban.active(now) {
			k.null = true
			return k
		}
		k.v1 = e.Ban.Since
	case SortExpires:
		switch {
		case !e.Ban.active(now):
			k.null = true
		case e.Ban.Permanent:
			k.v1 = math.MaxInt64
		default:
			k.v1 = e.Ban.Until
		}
	case SortPoints:
		k.v1, k.v2 = int64(recentPoints(e, cutoff)), e.LastSeen
	default:
		k.v1, k.v2 = int64(4-statusRank[status]), e.LastSeen
	}
	return k
}

// ---------- 游标 ----------
//
// 游标是不透明字符串：base64url("v2|<sort>|<order>|<filterFP>|<null>|<v1>|<v2>|<after>|<ip>")。
// 编码了排序键（含 tie-breaker ip）、排序方式、方向与筛选指纹，因此只在同一组排序 / 筛选下有效。
// after=1 表示"紧挨在该键之后的位置"，用于空页（末页之后）往回翻：向前取 ≤ 该键的最后 limit 条。

type cursor struct {
	sort, order, fp string
	key             pageKey
	after           bool
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (c cursor) encode() string {
	raw := strings.Join([]string{"v2", c.sort, c.order, c.fp, boolStr(c.key.null),
		strconv.FormatInt(c.key.v1, 10), strconv.FormatInt(c.key.v2, 10), boolStr(c.after), c.key.ip}, "|")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor 校验并解析不透明游标（Web 接口据此返回 400）。
func ParseCursor(s string) error {
	_, err := decodeCursor(s)
	return err
}

func decodeCursor(s string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) == 0 || len(raw) > maxCursorLen {
		return cursor{}, ErrInvalidCursor
	}
	parts := strings.SplitN(string(raw), "|", 9)
	if len(parts) != 9 || parts[0] != "v2" {
		return cursor{}, ErrInvalidCursor
	}
	var c cursor
	c.sort, c.order, c.fp = parts[1], parts[2], parts[3]
	if c.sort == "" || !validSort(c.sort) || (c.order != OrderAsc && c.order != OrderDesc) || len(c.fp) != 8 {
		return cursor{}, ErrInvalidCursor
	}
	for _, ch := range c.fp {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return cursor{}, ErrInvalidCursor
		}
	}
	null, nullOK := parseFlag(parts[4])
	after, afterOK := parseFlag(parts[7])
	v1, err1 := strconv.ParseInt(parts[5], 10, 64)
	v2, err2 := strconv.ParseInt(parts[6], 10, 64)
	if !nullOK || !afterOK || err1 != nil || err2 != nil || parts[8] == "" || len(parts[8]) > 64 {
		return cursor{}, ErrInvalidCursor
	}
	c.key = pageKey{null: null, v1: v1, v2: v2, ip: parts[8]}
	c.after = after
	return c, nil
}

func parseFlag(s string) (v, ok bool) {
	switch s {
	case "0":
		return false, true
	case "1":
		return true, true
	}
	return false, false
}

// ---------- 筛选 ----------

func matchFilter(e *entry, status string, f Filter, now int64) bool {
	banned := e.Ban.active(now)
	if f.Reason != "" && (!banned || e.Ban.Reason != f.Reason) {
		return false
	}
	if f.Kind != "" && (!banned || e.Ban.Kind != f.Kind) {
		return false
	}
	switch f.Status {
	case "banned":
		return banned
	case "allowlisted":
		return e.Allow.active(now)
	case "grace":
		return e.GraceUntil > now
	case "suspect":
		return status == "suspect"
	case "tracked":
		return true
	default: // all
		return status != "seen" || len(e.Events) > 0 || len(e.History) > 0
	}
}

// matchSearch 子串搜索（q 已小写、去首尾空白）：键（IP 或 IPv6 /64 网段）、各客户端的主机名与版本、
// 生效中的封禁 / 白名单备注。不区分大小写。在读锁内按块调用，每条目常数次比较（客户端数 ≤ maxClientsPerIP），
// 且不分配内存（见 containsFold）。
func matchSearch(e *entry, q string, now int64) bool {
	if containsFold(e.IP, q) {
		return true
	}
	for i := range e.Clients {
		if containsFold(e.Clients[i].Host, q) || containsFold(e.Clients[i].Version, q) {
			return true
		}
	}
	if e.Ban.active(now) && containsFold(e.Ban.Note, q) {
		return true
	}
	return e.Allow.active(now) && containsFold(e.Allow.Note, q)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// caseless 判断 q 里没有任何有大小写之分的字符（纯数字、标点、中日韩文字等）：此时 ToLower 对 q
// 不起作用，s 里的大小写也不会让它多出匹配，逐字节子串查找即可。
func caseless(q string) bool {
	for _, r := range q {
		if unicode.ToLower(r) != r || unicode.ToUpper(r) != r {
			return false
		}
	}
	return true
}

// containsFold 判断 s 是否包含 q（q 已小写）。持读锁时调用，所以常见情形都不分配内存：
//   - q 为纯 ASCII：逐字节忽略 ASCII 大小写比较；s 里的非 ASCII 字节（0x80 以上）与 ASCII 的 q 永远
//     不相等，不必先 ToLower（此前中文主机名 / 备注在这里每次分配，读锁内多花十几毫秒）。
//   - q 没有大小写之分（如中文）：直接 strings.Contains。
//
// 只有 q 含有大小写之分的非 ASCII 字母（如 é、Я）时才退回 ToLower。
func containsFold(s, q string) bool {
	if q == "" {
		return true
	}
	if !isASCII(q) {
		if caseless(q) {
			return strings.Contains(s, q)
		}
		return strings.Contains(strings.ToLower(s), q)
	}
	for i := 0; i+len(q) <= len(s); i++ {
		j := 0
		for ; j < len(q); j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != q[j] {
				break
			}
		}
		if j == len(q) {
			return true
		}
	}
	return false
}

// ---------- 查询 ----------

// scanChunk 是 Query 每次取读锁处理的条目数；按 ~2µs/条目算，单次持锁约 0.5ms。
const scanChunk = 256

// Query 在服务端完成过滤、排序与分页，只为当前页构造摘要。
//
// 持锁时间尽量短：第一次读锁内只做一次 O(n) 扫描（n ≤ 主表硬上限），每个条目常数次判断并拷出
// 排序键；排序与分页在锁外完成；第二次读锁只为页内条目（≤ MaxPageLimit）生成摘要。
// 洪泛时写锁排在读锁之后、accept 的读锁又排在写锁之后，锁内排序会让 accept 停顿数毫秒。
//
// 游标与排序 / 筛选不一致（换了排序方式、方向、状态、来源、触发原因或搜索词）时，游标被忽略，
// 从第一页返回并置 Page.CursorReset（offset 此时也不生效）。
func (l *List) Query(f Filter) Page {
	now := l.now()
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultPageLimit
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	sortName, asc := f.sortSpec()
	order := OrderDesc
	if asc {
		order = OrderAsc
	}
	search := normSearch(f.Search)
	fp := f.fingerprint(search)
	page := Page{Persistent: l.persistent, Now: now, Config: l.configView(), Offset: offset, Limit: limit, Entries: []SummaryView{}, Untracked: l.untracked.Load(), Sort: sortName, Order: order}

	type sortKey struct {
		e *entry
		pageKey
	}
	cutoff := now - int64(l.cfg.Window/time.Second)
	// 分块扫描：先在读锁内拷出全部条目指针（O(n) 指针拷贝），之后每 scanChunk 个条目取一次读锁。
	// 洪泛时写锁排在读锁后面，随后 accept 的读锁又排在写锁后面，所以单次持锁时间必须与表大小无关。
	// 块之间条目可能被淘汰：已摘出的条目仍是完整的堆对象，只是可能略旧，与第二次读锁取摘要之间
	// 本来就存在的间隙同性质。
	l.mu.RLock()
	page.Tracked = len(l.entries)
	all := make([]*entry, 0, len(l.entries))
	for _, e := range l.entries {
		all = append(all, e)
	}
	l.mu.RUnlock()
	keys := make([]sortKey, 0, 64)
	for from := 0; from < len(all); from += scanChunk {
		to := from + scanChunk
		if to > len(all) {
			to = len(all)
		}
		l.mu.RLock()
		for _, e := range all[from:to] {
			st := quickStatus(e, now, cutoff)
			if !matchFilter(e, st, f, now) {
				continue
			}
			if search != "" && !matchSearch(e, search, now) {
				continue
			}
			keys = append(keys, sortKey{e: e, pageKey: keyFor(e, sortName, st, now, cutoff)})
		}
		l.mu.RUnlock()
	}

	page.Total = len(keys)
	sort.Slice(keys, func(i, j int) bool { return keys[i].pageKey.less(keys[j].pageKey, asc) })
	start := offset
	if f.Cursor != "" {
		if cur, err := decodeCursor(f.Cursor); err == nil {
			if cur.sort != sortName || cur.order != order || cur.fp != fp {
				start = 0
				page.CursorReset = true
			} else if f.Dir == "prev" {
				// 严格排在游标位置之前的最后 limit 条。
				end := sort.Search(len(keys), func(i int) bool {
					if cur.after {
						return cur.key.less(keys[i].pageKey, asc) // 第一个严格晚于游标键的
					}
					return !keys[i].pageKey.less(cur.key, asc) // 第一个不早于游标键的
				})
				start = end - limit
				if start < 0 {
					// 越过开头：返回从头开始的满页（就是第一页），而不是不足一页甚至空页。
					start = 0
				} else {
					limit = end - start
				}
			} else {
				start = sort.Search(len(keys), func(i int) bool { return cur.key.less(keys[i].pageKey, asc) })
			}
		}
	}
	if start > len(keys) {
		start = len(keys)
	}
	end := start + limit
	if end > len(keys) {
		end = len(keys)
	}
	page.Start, page.Offset = start, start
	page.HasPrev = start > 0
	page.HasNext = end < len(keys)
	mk := func(k pageKey, after bool) string {
		return cursor{sort: sortName, order: order, fp: fp, key: k, after: after}.encode()
	}
	if start < end {
		page.PrevCursor = mk(keys[start].pageKey, false)
		page.NextCursor = mk(keys[end-1].pageKey, false)
	} else if start > 0 {
		// 空页（游标之后已没有条目）：上一页从末尾往前取。
		page.PrevCursor = mk(keys[start-1].pageKey, true)
	}
	l.mu.RLock()
	for _, k := range keys[start:end] {
		page.Entries = append(page.Entries, l.summaryLocked(k.e, now))
	}
	l.mu.RUnlock()
	return page
}
