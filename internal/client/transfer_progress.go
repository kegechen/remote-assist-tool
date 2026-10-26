package client

import (
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

// transferProgress 把 share 端收到的文件传输分块（agent.TransferEvent）聚合成一条
// 进度显示，代替逐块刷屏的活动行。
//
// 终端（tty）下用 \r 原地刷新一行：
//
//	上传 xgimi_remote.apk [=========>          ]  47.3%  17.8/37.6 MB  3.1 MB/s  剩余 00:06
//
// 非终端（重定向到文件、无控制台）下不能用 \r，改为每隔 plainEvery 打一整行。
// 两种模式传输结束都打一行汇总。其他活动行与日志经 Print / Writer 输出，先擦掉
// 进度行、写完再重画，不会和进度行串在一起。
type transferProgress struct {
	mu    sync.Mutex
	out   io.Writer
	tty   bool
	width func() int // 终端列数；<=0 表示未知
	now   func() time.Time
	logf  func(format string, args ...any) // 审计日志，默认 log.Printf；只在解锁后调用
	// quietSummaryLogs 为真时不写传输开始/汇总的审计日志：log 与进度在同一个控制台时
	// 它们和屏幕上的汇总重复。重复分块的日志不受影响——它们不上屏，日志是唯一的痕迹。
	quietSummaryLogs bool

	stallAfter   time.Duration // 这么久没有新分块，进度行上标「停滞」
	abandonAfter time.Duration // 已知总大小却停滞这么久，判为中断并收尾
	unknownIdle  time.Duration // 未知总大小时，没有新分块多久算传完的下限（见 idleFinish）
	recentWindow time.Duration // 收尾后多久内的迟到重复分块会被吞掉
	plainEvery   time.Duration // 非终端模式下的进度行间隔

	active      map[string]*transferState
	order       []string // 按开始顺序，决定显示哪一个
	recent      map[string]*transferState
	pendingLogs []string // 持锁期间攒下的审计日志，解锁后再写（log 的输出也要拿 p.mu）
	drawn       int      // 当前屏幕上进度行占的列数，0 表示没有进度行
	stopped     bool     // Stop 之后不再画进度行（迟到的 tick / 活动行不能把收尾换行又盖掉）
	lastPlain   time.Time

	stop     chan struct{}
	stopOnce sync.Once
}

type transferState struct {
	dir      string
	path     string
	total    int64 // 0 = 未知
	base     int64 // 本次传输的起点（续传时不为 0），取见过的最小偏移
	baseSet  bool
	received int64           // 本次传输收到的字节数，按偏移去重
	chunks   map[int64]int64 // 偏移 -> 字节数：响应丢失后重试的分块会再来一次，不能重复计入
	last     int64           // 最近一块的偏移
	grown    bool            // 上传到达 total 后又有后续分块：文件在长大，改靠空闲判定结束
	// started 是第一个事件到达的时刻。没有 Start 事件（fromStart 为假：下载、续传）时，
	// 第一块（firstBytes）在这之前就已经在网上传完了，算平均速度要把它扣掉，否则速度
	// 虚高（两块时近一倍）。
	fromStart  bool
	firstBytes int64
	started    time.Time
	lastSeen   time.Time
	samples    []rateSample
}

type rateSample struct {
	at   time.Time
	done int64
}

const (
	progressRedraw = 200 * time.Millisecond
	rateWindow     = 3 * time.Second
	barWidth       = 20
)

func newTransferProgress(out io.Writer, tty bool, width func() int) *transferProgress {
	if width == nil {
		width = func() int { return 0 }
	}
	return &transferProgress{
		out:   out,
		tty:   tty,
		width: width,
		now:   time.Now,
		logf:  log.Printf,
		// 进度事件只在 share 端处理完一整块（512 KiB）时才有。链路慢于约 170 KB/s、
		// 或 help 端重试退避（1/2/3 秒）时，块间隔轻松超过几秒，所以空闲只用来标停滞，
		// 不用来判结束：结束靠「上传到达 total」「下载读到 EOF」。help 端放弃（重试
		// 用尽、断线）之后才会长时间没有分块，那时再判中断。
		stallAfter:   3 * time.Second,
		abandonAfter: 2 * time.Minute,
		unknownIdle:  10 * time.Second,
		recentWindow: 10 * time.Second,
		plainEvery:   5 * time.Second,
		active:       map[string]*transferState{},
		recent:       map[string]*transferState{},
		stop:         make(chan struct{}),
	}
}

// Start 启动定时刷新（速度/剩余时间随时间变化，停滞与中断也要靠它判定）。
func (p *transferProgress) Start() {
	go func() {
		t := time.NewTicker(progressRedraw)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.tick()
			}
		}
	}()
}

// Stop 停止刷新；屏幕上还留着进度行时补一个换行，免得后续输出接在它后面。
func (p *transferProgress) Stop() {
	p.stopOnce.Do(func() {
		close(p.stop)
		p.mu.Lock()
		p.stopped = true
		if p.drawn > 0 {
			fmt.Fprintln(p.out)
			p.drawn = 0
		}
		p.mu.Unlock()
	})
}

// unlock 解锁并写出持锁期间攒下的审计日志。log 的输出可能被 Writer 包装、
// 要再拿 p.mu，所以绝不能在持锁时调用 logf。
func (p *transferProgress) unlock() {
	logs := p.pendingLogs
	p.pendingLogs = nil
	p.mu.Unlock()
	for _, l := range logs {
		p.logf("%s", l)
	}
}

// Event 接收一个传输分块。
func (p *transferProgress) Event(ev agent.TransferEvent) {
	p.mu.Lock()
	defer p.unlock()
	now := p.now()
	key := ev.Dir + "\x00" + ev.Path
	st := p.active[key]
	if ev.Start && st != nil {
		// 同一文件重新从头传：上一轮没收尾就算作中断。
		p.finishLocked(key, now)
		st = nil
	}
	if st == nil && !ev.Start {
		if old := p.recent[key]; old != nil && now.Sub(old.lastSeen) < p.recentWindow {
			switch {
			case p.lateDuplicate(old, ev):
				// 屏幕上不显示，但对端确实又读/写了一次，审计日志里要有。
				p.logDuplicate(ev)
				return
			case ev.Dir == "upload" && ev.Offset == old.done():
				// 上传途中本地文件长大了：到达 stat 时的 total 就收了尾，后面紧接着的分块
				// 并回原传输，而不是每块各开一轮、各打一行汇总。
				st = old
				st.grown = true
				delete(p.recent, key)
				p.active[key] = st
				p.order = append(p.order, key)
			}
		}
	}
	if st == nil {
		if len(p.order) == 0 {
			p.lastPlain = now // 非终端模式下第一行进度在开始 plainEvery 之后再打，不打 0%
		}
		st = &transferState{dir: ev.Dir, path: ev.Path, started: now, chunks: map[int64]int64{}, fromStart: ev.Start, firstBytes: ev.Bytes}
		p.active[key] = st
		p.order = append(p.order, key)
		delete(p.recent, key)
		if !p.quietSummaryLogs {
			p.pendingLogs = append(p.pendingLogs, fmt.Sprintf("tool | transfer | %s %s | start offset=%d total=%d", ev.Dir, ev.Path, ev.Offset, ev.Total))
		}
	}
	if ev.Total > 0 {
		st.total = ev.Total
	}
	if ev.Start {
		// 从头传，起点必然是 0。直接定下来，免得分块乱序到达时 base 先被后面的
		// 偏移占住、进度先虚高再回落。
		st.base, st.baseSet = 0, true
	} else {
		if !st.baseSet || ev.Offset < st.base {
			st.base, st.baseSet = ev.Offset, true
		}
		prev, dup := st.chunks[ev.Offset]
		if dup {
			// 重试，或者对端反复改写同一位置（不规范的调用方直接带 at 写）：进度不重复
			// 计入，但每一次写入都要留在审计日志里。
			p.logDuplicate(ev)
		}
		if !dup || ev.Bytes > prev {
			st.received += ev.Bytes - prev
			st.chunks[ev.Offset] = ev.Bytes
		}
		st.last = ev.Offset
		if st.total > 0 && st.done() > st.total {
			st.total = st.done() // 文件在传输中长大了（比如正在写的日志），以实际为准
		}
	}
	st.lastSeen = now
	st.samples = append(st.samples, rateSample{at: now, done: st.done()})
	st.trimSamples(now)
	if ev.EOF {
		// 读到 EOF 就是读完了。文件可能比 stat 时短（/proc 下的文件、被轮转的日志），
		// 以实际读到的为准，不能报成「中断」。
		st.total = st.done()
	}
	// 下载只认 EOF（文件可能在长大）；上传没有 EOF 标记，到达 total 即完成。
	// 已知局限：上传途中本地文件变短时永远到不了 total，要等 abandonAfter 才收尾并报
	// 「中断」，而 help 端其实成功了。只影响显示，审计日志里的字节数仍是实际值。
	if ev.EOF || (ev.Dir == "upload" && !st.grown && st.total > 0 && st.done() >= st.total) {
		p.finishLocked(key, now)
	}
}

// lateDuplicate 判断一个分块是不是刚收尾的传输迟到的重复：最后一块的响应丢了、
// help 端又重试了一次。新版 help 端按顺序传输，收尾之后只可能迟到这一种（下载读满
// 最后一块后那次 0 字节的 EOF 读发生在收尾之前，会正常并进进度）。重新传同一个文件
// 从起点开始，不会被误吞——单块文件的起点就是最后一块，这种重传在 recentWindow
// 内会被当成重复，只影响显示。
func (p *transferProgress) lateDuplicate(old *transferState, ev agent.TransferEvent) bool {
	if ev.Offset == old.last {
		return true
	}
	// 旧版 help 并发上传时，重试的不一定是最后一块
	_, seen := old.chunks[ev.Offset]
	return seen && ev.Offset != old.base
}

func (p *transferProgress) logDuplicate(ev agent.TransferEvent) {
	p.pendingLogs = append(p.pendingLogs, fmt.Sprintf("tool | transfer | %s %s | repeated chunk offset=%d bytes=%d", ev.Dir, ev.Path, ev.Offset, ev.Bytes))
}

// Print 原样输出一段文本（应以换行结尾），不与进度行串行。
func (p *transferProgress) Print(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	fmt.Fprint(p.out, text)
	p.drawLocked()
}

// Writer 包装另一个输出（如 log 的 stderr）：写入前擦掉进度行，写完重画。
// 只在它和 out 是同一个终端时才有意义，由调用方判断。
func (p *transferProgress) Writer(w io.Writer) io.Writer {
	return progressPassthrough{p: p, w: w}
}

type progressPassthrough struct {
	p *transferProgress
	w io.Writer
}

func (pw progressPassthrough) Write(b []byte) (int, error) {
	pw.p.mu.Lock()
	defer pw.p.mu.Unlock()
	pw.p.clearLocked()
	n, err := pw.w.Write(b)
	pw.p.drawLocked()
	return n, err
}

func (p *transferProgress) tick() {
	p.mu.Lock()
	defer p.unlock()
	now := p.now()
	for key, st := range p.recent {
		if now.Sub(st.lastSeen) >= p.recentWindow {
			delete(p.recent, key)
		}
	}
	for _, key := range append([]string(nil), p.order...) {
		if st := p.active[key]; st != nil && now.Sub(st.lastSeen) >= p.idleFinish(st) {
			p.finishLocked(key, now)
		}
	}
	if p.tty {
		p.drawLocked()
		return
	}
	if len(p.order) > 0 && now.Sub(p.lastPlain) >= p.plainEvery {
		p.lastPlain = now
		for _, key := range p.order {
			fmt.Fprintln(p.out, p.lineLocked(p.active[key], now, 0))
		}
	}
}

// idleFinish 是没有新分块多久就收尾。已知总大小时收尾意味着中断，要等 help 端
// 确实放弃；未知总大小（旧版 help 端）只能靠空闲判定传完，按块间隔自适应，
// 慢链路上不至于每块都被切成一次「完成」。
func (p *transferProgress) idleFinish(st *transferState) time.Duration {
	if st.total > 0 && !st.grown {
		return p.abandonAfter
	}
	idle := p.unknownIdle
	if n := len(st.chunks); n > 1 {
		if gap := 4 * st.lastSeen.Sub(st.started) / time.Duration(n); gap > idle {
			idle = gap
		}
	}
	return idle
}

func (p *transferProgress) lineLocked(st *transferState, now time.Time, max int) string {
	var stalled time.Duration
	if d := now.Sub(st.lastSeen); d >= p.stallAfter {
		stalled = d
	}
	return st.line(now, max, stalled)
}

// finishLocked 结束一个传输：擦掉进度行、打汇总、重画剩下的。
func (p *transferProgress) finishLocked(key string, now time.Time) {
	st := p.active[key]
	if st == nil {
		return
	}
	delete(p.active, key)
	p.recent[key] = st
	for i, k := range p.order {
		if k == key {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	p.clearLocked()
	sum := st.summary()
	fmt.Fprintln(p.out, sum)
	if !p.quietSummaryLogs {
		p.pendingLogs = append(p.pendingLogs, "tool | transfer | "+sum)
	}
	p.drawLocked()
}

func (p *transferProgress) clearLocked() {
	if p.drawn > 0 {
		// 窗口被拉窄后，上一帧已经被终端重排，擦除用的空格再按旧宽度写就会折行。
		fmt.Fprint(p.out, "\r"+strings.Repeat(" ", min(p.drawn, p.maxCols()))+"\r")
		p.drawn = 0
	}
}

// maxCols 是进度行可用的列数：留一列，写满最后一列时部分终端会自动换行。
func (p *transferProgress) maxCols() int {
	if w := p.width(); w > 1 {
		return w - 1
	}
	return 79
}

func (p *transferProgress) drawLocked() {
	if !p.tty || p.stopped || len(p.order) == 0 {
		return
	}
	max := p.maxCols()
	line := p.lineLocked(p.active[p.order[0]], p.now(), max)
	if extra := len(p.order) - 1; extra > 0 {
		if more := fmt.Sprintf("  (+%d)", extra); displayWidth(line)+displayWidth(more) <= max {
			line += more
		}
	}
	w := displayWidth(line)
	pad := ""
	if prev := min(p.drawn, max); w < prev {
		pad = strings.Repeat(" ", prev-w) // 盖掉上一次更长的残留
	}
	fmt.Fprint(p.out, "\r"+line+pad)
	p.drawn = w + len(pad)
}

func (st *transferState) done() int64 { return st.base + st.received }

func (st *transferState) trimSamples(now time.Time) {
	i := 0
	for i < len(st.samples)-1 && now.Sub(st.samples[i].at) > rateWindow {
		i++
	}
	st.samples = st.samples[i:]
}

// rate 返回最近 rateWindow 内的速度（字节/秒）；样本跨度太短时退回整体平均。
func (st *transferState) rate(now time.Time) float64 {
	if len(st.samples) >= 2 {
		first, last := st.samples[0], st.samples[len(st.samples)-1]
		if dt := last.at.Sub(first.at).Seconds(); dt >= 0.5 {
			if d := last.done - first.done; d > 0 {
				return float64(d) / dt
			}
			return 0
		}
	}
	if dt := now.Sub(st.started).Seconds(); dt > 0 {
		return float64(st.timedBytes()) / dt
	}
	return 0
}

// timedBytes 是 started 之后才传输的字节数，用来算平均速度。
func (st *transferState) timedBytes() int64 {
	n := st.done() - st.base
	if !st.fromStart {
		n -= st.firstBytes
	}
	if n < 0 {
		return 0
	}
	return n
}

func (st *transferState) verb() string {
	if st.dir == "download" {
		return "下载"
	}
	return "上传"
}

// line 生成一行进度。max>0 时按终端宽度裁剪：先截文件名，再去掉进度条。
func (st *transferState) line(now time.Time, max int, stalled time.Duration) string {
	done := st.done()
	r := st.rate(now)
	if stalled > 0 {
		r = 0 // 停滞时还按停滞前的速度报剩余时间是误导
	}
	var stats string
	if st.total > 0 {
		stats = fmt.Sprintf("%5.1f%%  %s/%s  %s/s", float64(done)*100/float64(st.total),
			formatBytes(done), formatBytes(st.total), formatBytes(int64(r)))
		if r > 0 && done < st.total {
			stats += "  剩余 " + formatETA(time.Duration(float64(st.total-done)/r*float64(time.Second)))
		}
	} else {
		stats = fmt.Sprintf("%s  %s/s", formatBytes(done), formatBytes(int64(r)))
	}
	if stalled > 0 {
		stats += "  停滞 " + formatETA(stalled)
	}
	bar := ""
	if st.total > 0 {
		bar = " " + progressBar(done, st.total, barWidth)
	}
	name := path.Base(strings.ReplaceAll(st.path, `\`, "/"))
	build := func(name, bar string) string { return st.verb() + " " + name + bar + "  " + stats }
	l := build(name, bar)
	if max <= 0 || displayWidth(l) <= max {
		return l
	}
	if room := max - displayWidth(build("", bar)); room >= 8 {
		return build(truncateMiddle(name, room), bar)
	}
	if room := max - displayWidth(build("", "")); room >= 8 {
		return build(truncateMiddle(name, room), "")
	}
	return truncateMiddle(build(name, ""), max)
}

func (st *transferState) summary() string {
	dur := st.lastSeen.Sub(st.started)
	moved := st.done() - st.base
	avg := ""
	// 不到半秒的传输（小文件）算出来的平均速度没有意义，不显示。
	if s, n := dur.Seconds(), st.timedBytes(); s >= 0.5 && n > 0 {
		avg = fmt.Sprintf("，平均 %s/s", formatBytes(int64(float64(n)/s)))
	}
	status := "完成"
	if st.total > 0 && st.done() < st.total {
		status = fmt.Sprintf("中断（%.1f%%，%s/%s）", float64(st.done())*100/float64(st.total),
			formatBytes(st.done()), formatBytes(st.total))
	}
	return fmt.Sprintf("[%s] %s%s %s：%s，用时 %s%s", st.lastSeen.Format("15:04:05"), st.verb(), status,
		st.path, formatBytes(moved), formatETA(dur), avg)
}

func progressBar(done, total int64, width int) string {
	filled := int(float64(done) / float64(total) * float64(width))
	if filled > width {
		filled = width
	}
	if filled == width {
		return "[" + strings.Repeat("=", width) + "]"
	}
	return "[" + strings.Repeat("=", filled) + ">" + strings.Repeat(" ", width-filled-1) + "]"
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, u := range []string{"KB", "MB", "GB"} {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f TB", v/unit)
}

func formatETA(d time.Duration) string {
	s := int64(d.Round(time.Second) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// displayWidth 估算字符串在终端占的列数：CJK 全角字符占 2 列。进度行刻意只用
// ASCII 符号，避开 █ 之类在中文代码页下宽度不确定的字符。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6, r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

// truncateMiddle 把 s 截到不超过 max 列，中间用 "..." 代替。
func truncateMiddle(s string, max int) string {
	if displayWidth(s) <= max {
		return s
	}
	if max <= 3 {
		return strings.Repeat(".", max)
	}
	rs := []rune(s)
	keep := max - 3
	headW, tailW := (keep+1)/2, keep/2
	var head, tail []rune
	w := 0
	for _, r := range rs {
		if w+runeWidth(r) > headW {
			break
		}
		head = append(head, r)
		w += runeWidth(r)
	}
	w = 0
	for i := len(rs) - 1; i >= len(head); i-- {
		if w+runeWidth(rs[i]) > tailW {
			break
		}
		tail = append([]rune{rs[i]}, tail...)
		w += runeWidth(rs[i])
	}
	return string(head) + "..." + string(tail)
}
