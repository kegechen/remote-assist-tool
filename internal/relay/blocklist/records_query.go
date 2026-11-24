package blocklist

// 封禁记录查询：控制台「封禁记录」表的服务端筛选 + 分页。

import (
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"
)

// Records 返回名单共用的封禁记录（控制台经由它查询）；没配置时为 nil。
// 放在这个控制台专用文件里，而不是 list.go：list.go 是两条线共用的公共文件，要保持一致。
func (l *List) Records() *Records {
	if l == nil {
		return nil
	}
	return l.cfg.Records
}

// 记录查询的状态筛选取值。
const (
	RecordStateActive = "active" // 未结束（封禁中）
	RecordStateEnded  = "ended"  // 已结束
)

// RecordFilter 选择要返回的封禁记录，各条件取交集；Target / State / Reason / End 的空串与 "all" 都表示不限。
//
// 翻页：结果按 id 倒序（最新的在前）。Cursor 为上一页最小的 id，只返回 id 比它小的记录；id 单调递增、
// 记录结束时 id 不变，所以翻页期间有新封禁或旧记录结束都不会造成跳过或重复。
type RecordFilter struct {
	Target string // ip | device
	State  string // active | ended
	Reason string // auto | manual
	End    string // 结束方式：expired | unban | evicted | replaced | lost
	Search string // 子串搜索（不区分大小写，去首尾空白）：键、via、label、封禁时客户端的主机名
	Limit  int    // 每页条数：0 取 DefaultPageLimit，上限 MaxPageLimit
	Cursor int64  // 0 = 第一页
}

// RecordPage 是 Records.Query 的结果。
type RecordPage struct {
	Persistent    bool     `json:"persistent"`     // 记录是否落盘（否则重启丢失）
	MaxRecords    int      `json:"max_records"`    // 已结束记录的条数上限
	RetentionDays int      `json:"retention_days"` // 已结束记录的保留天数
	Now           int64    `json:"now"`
	Total         int      `json:"total"`       // 符合筛选的记录数（与游标无关）
	Start         int      `json:"start"`       // 本页之前（id 不小于游标）符合筛选的记录数
	Limit         int      `json:"limit"`       // 本次生效的每页条数
	NextCursor    int64    `json:"next_cursor"` // 有下一页时为本页最小的 id，下一页传 cursor=它；否则 0
	HasNext       bool     `json:"has_next"`
	Records       []Record `json:"records"`
}

func isAllOr(v string, allowed ...string) bool {
	if v == "" || v == "all" {
		return true
	}
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// Validate 白名单校验全部参数。Query 对非法值不报错（按不限 / 默认处理），Web 层应先调用本方法并返回 400。
func (f RecordFilter) Validate() error {
	if !isAllOr(f.Target, TargetIP, TargetDevice) {
		return fmt.Errorf("%w: target must be all, ip or device", ErrInvalidFilter)
	}
	if !isAllOr(f.State, RecordStateActive, RecordStateEnded) {
		return fmt.Errorf("%w: state must be all, active or ended", ErrInvalidFilter)
	}
	if !isAllOr(f.Reason, "auto", "manual") {
		return fmt.Errorf("%w: reason must be all, auto or manual", ErrInvalidFilter)
	}
	if !isAllOr(f.End, EndExpired, EndUnban, EndEvicted, EndReplaced, EndLost) {
		return fmt.Errorf("%w: end must be all, expired, unban, evicted, replaced or lost", ErrInvalidFilter)
	}
	if utf8.RuneCountInString(f.Search) > MaxSearchLen {
		return fmt.Errorf("%w: search too long (max %d characters)", ErrInvalidFilter, MaxSearchLen)
	}
	if f.Limit < 0 || f.Limit > MaxPageLimit {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidFilter, MaxPageLimit)
	}
	if f.Cursor < 0 || f.Cursor > maxRecordID {
		return ErrInvalidCursor
	}
	return nil
}

// ParseRecordCursor 解析记录列表的游标：只接受十进制数字，范围 [1, 2^53]。
func ParseRecordCursor(s string) (int64, error) {
	if s == "" || len(s) > 16 {
		return 0, ErrInvalidCursor
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrInvalidCursor
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || n > maxRecordID {
		return 0, ErrInvalidCursor
	}
	return n, nil
}

// recordView 返回记录的展示副本（切片另行复制，不与内部记录共享内存）。计划已过、名单还没清扫到的生效记录
// 按到期展示：此时封禁已不再拦截，名单下次清扫（relay 每分钟一次）写入的正是 expired、ended_at=until、
// end_actor=auto，这里提前与之一致，免得与名单页的状态对不上。
func recordView(p *Record, now int64) Record {
	v := *p
	v.Trigger = append([]TriggerItem(nil), p.Trigger...)
	v.Clients = append([]RecordClient(nil), p.Clients...)
	if v.End == "" && !v.Permanent && v.Until <= now {
		v.End, v.EndedAt, v.EndActor = EndExpired, max(v.Until, v.Since), autoActor
	}
	return v
}

// recordEnd 记录在 now 时刻的结束方式（计划已过的生效记录按到期），与 recordView 同口径。
func recordEnd(p *Record, now int64) string {
	if p.End == "" && !p.Permanent && p.Until <= now {
		return EndExpired
	}
	return p.End
}

func (f RecordFilter) match(p *Record, search string, now int64) bool {
	if f.Target != "" && f.Target != "all" && p.Target != f.Target {
		return false
	}
	if f.Reason != "" && f.Reason != "all" && p.Reason != f.Reason {
		return false
	}
	end := recordEnd(p, now)
	switch f.State {
	case RecordStateActive:
		if end != "" {
			return false
		}
	case RecordStateEnded:
		if end == "" {
			return false
		}
	}
	if f.End != "" && f.End != "all" && end != f.End {
		return false
	}
	return search == "" || matchRecordSearch(p, search)
}

func matchRecordSearch(p *Record, q string) bool {
	if containsFold(p.Key, q) || containsFold(p.Via, q) || containsFold(p.Label, q) {
		return true
	}
	for i := range p.Clients {
		if containsFold(p.Clients[i].Host, q) {
			return true
		}
	}
	return false
}

// Query 按筛选返回一页记录，按 id 倒序。锁内只复制指针切片（记录写入后不可变，结束时整条换成新副本），
// 过滤在锁外做：Records 的锁在名单写锁之内被获取，查询不能拖住 accept 热路径上的封禁。
func (r *Records) Query(f RecordFilter) RecordPage {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultPageLimit
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	page := RecordPage{Limit: limit, Records: []Record{}}
	if r == nil {
		return page
	}
	now := r.now()
	page.Persistent, page.Now = r.persistent, now
	page.MaxRecords, page.RetentionDays = r.cfg.MaxRecords, int(r.cfg.Retention/(24*time.Hour))
	search := normSearch(f.Search)
	r.mu.Lock()
	ptrs := append([]*Record(nil), r.recs...)
	r.mu.Unlock()
	for i := len(ptrs) - 1; i >= 0; i-- {
		p := ptrs[i]
		if !f.match(p, search, now) {
			continue
		}
		page.Total++
		switch {
		case f.Cursor > 0 && p.ID >= f.Cursor:
			page.Start++
		case len(page.Records) < limit:
			page.Records = append(page.Records, recordView(p, now))
		default:
			page.HasNext = true
		}
	}
	if page.HasNext {
		page.NextCursor = page.Records[len(page.Records)-1].ID
	}
	return page
}
