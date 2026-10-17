package client

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock           { return &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)} }
func testProgress(tty bool, width int) (*transferProgress, *bytes.Buffer, *fakeClock) {
	var buf bytes.Buffer
	clk := newFakeClock()
	p := newTransferProgress(&buf, tty, func() int { return width })
	p.now = clk.now
	p.logf = func(string, ...any) {}
	return p, &buf, clk
}

const mib = 1 << 20

// lastFrame 返回终端上「最后一次 \r 之后」的内容，即当前可见的进度行。
func lastFrame(out string) string {
	if i := strings.LastIndex(out, "\r"); i >= 0 {
		return strings.TrimRight(out[i+1:], " ")
	}
	return out
}

func TestProgressUploadAggregatesChunksIntoOneLine(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	const total = 4 * mib
	p.Event(agent.TransferEvent{Dir: "upload", Path: `C:\Users\Public\xgimi_remote.apk`, Total: total, Start: true})
	// 并发分块乱序到达：先到的是第二块，此时只传完 1/4，不能因为起点被后面的
	// 偏移占住而显示成 50%。
	clk.add(500 * time.Millisecond)
	p.Event(agent.TransferEvent{Dir: "upload", Path: `C:\Users\Public\xgimi_remote.apk`, Offset: mib, Bytes: mib, Total: total})
	p.tick()
	if frame := lastFrame(buf.String()); !strings.Contains(frame, "25.0%") {
		t.Fatalf("乱序首块后应显示 25%%：%q", frame)
	}
	clk.add(500 * time.Millisecond)
	p.Event(agent.TransferEvent{Dir: "upload", Path: `C:\Users\Public\xgimi_remote.apk`, Offset: 0, Bytes: mib, Total: total})
	p.tick()
	frame := lastFrame(buf.String())
	for _, want := range []string{"上传 xgimi_remote.apk", "[==========>", "50.0%", "2.0 MB/4.0 MB", "2.0 MB/s", "剩余 00:01"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("进度行缺少 %q：%q", want, frame)
		}
	}
	if strings.Contains(buf.String(), "\n") {
		t.Fatalf("传输中途不应该换行刷屏：%q", buf.String())
	}

	// 第三块响应丢了被 help 端重试，同一偏移到两次：不能重复计入而提前判完成。
	for _, at := range []int64{2 * mib, 2 * mib} {
		clk.add(500 * time.Millisecond)
		p.Event(agent.TransferEvent{Dir: "upload", Path: `C:\Users\Public\xgimi_remote.apk`, Offset: at, Bytes: mib, Total: total})
	}
	if strings.Contains(buf.String(), "上传完成") {
		t.Fatalf("重试的重复分块让传输提前完成了：%q", buf.String())
	}
	clk.add(500 * time.Millisecond)
	p.Event(agent.TransferEvent{Dir: "upload", Path: `C:\Users\Public\xgimi_remote.apk`, Offset: 3 * mib, Bytes: mib, Total: total})
	// 收尾后最后一块的重试迟到：吞掉，不能冒出第二条汇总。
	clk.add(100 * time.Millisecond)
	p.Event(agent.TransferEvent{Dir: "upload", Path: `C:\Users\Public\xgimi_remote.apk`, Offset: 3 * mib, Bytes: mib, Total: total})
	out := buf.String()
	if !strings.Contains(out, `上传完成 C:\Users\Public\xgimi_remote.apk：4.0 MB，用时 00:03，平均 1.6 MB/s`) {
		t.Fatalf("缺少完成汇总：%q", out)
	}
	if n := strings.Count(out, "上传完成"); n != 1 {
		t.Fatalf("应只有 1 条完成汇总，实际 %d：%q", n, out)
	}
	if len(p.active) != 0 {
		t.Fatalf("传输结束后应清掉状态，剩 %d 个", len(p.active))
	}
	// 收尾后不应再重画进度行
	before := buf.Len()
	p.tick()
	if buf.Len() != before {
		t.Fatalf("没有活动传输时 tick 不应输出：%q", buf.String()[before:])
	}
}

// 中途有别的活动行（exec 之类）时，要先擦掉进度行再打，打完重画进度。
func TestProgressPrintDoesNotGlueToProgressLine(t *testing.T) {
	p, buf, _ := testProgress(true, 120)
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/a.bin", Offset: 0, Bytes: mib, Total: 4 * mib})
	p.tick()
	drawn := p.drawn
	buf.Reset()
	p.Print("[12:00:00] exec: exec ls argc=1 (3ms, ok)\n")
	out := buf.String()
	wantPrefix := "\r" + strings.Repeat(" ", drawn) + "\r[12:00:00] exec"
	if !strings.HasPrefix(out, wantPrefix) {
		t.Fatalf("活动行前没有擦掉进度行：%q", out)
	}
	if !strings.Contains(lastFrame(out), "上传 a.bin") {
		t.Fatalf("活动行之后没有重画进度：%q", out)
	}

	// log 输出经 Writer 包装后同样处理
	buf.Reset()
	var logBuf bytes.Buffer
	w := p.Writer(&logBuf)
	w.Write([]byte("2026/10/17 log line\n"))
	if !strings.HasPrefix(buf.String(), "\r"+strings.Repeat(" ", drawn)+"\r") || logBuf.String() != "2026/10/17 log line\n" {
		t.Fatalf("log 写入前没有擦进度行：stdout=%q log=%q", buf.String(), logBuf.String())
	}
}

// 旧版 help 不带 total：只显示字节数和速度，空闲到阈值后打汇总。
func TestProgressWithoutTotalFinishesOnIdle(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	for i := int64(0); i < 3; i++ {
		clk.add(time.Second)
		p.Event(agent.TransferEvent{Dir: "upload", Path: "/a.bin", Offset: i * mib, Bytes: mib})
	}
	p.tick()
	frame := lastFrame(buf.String())
	if !strings.Contains(frame, "上传 a.bin  3.0 MB  1.0 MB/s") || strings.Contains(frame, "%") || strings.Contains(frame, "[") {
		t.Fatalf("无 total 的进度行不对：%q", frame)
	}
	clk.add(p.unknownIdle)
	p.tick()
	if !strings.Contains(buf.String(), "上传完成 /a.bin：3.0 MB") {
		t.Fatalf("空闲超时后没有汇总：%q", buf.String())
	}
}

// 已知 total 却停在半路（连接断了），汇总要明说中断，而不是谎报完成。
func TestProgressReportsInterruptedTransfer(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/a.bin", Total: 4 * mib, Start: true})
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/a.bin", Offset: 0, Bytes: mib, Total: 4 * mib})
	// help 端还在重试时只标停滞，不能当成结束
	clk.add(30 * time.Second)
	p.tick()
	if out := buf.String(); strings.Contains(out, "中断") || !strings.Contains(lastFrame(out), "0 B/s  停滞 00:30") || strings.Contains(lastFrame(out), "剩余") {
		t.Fatalf("停滞 30 秒时应只标停滞：%q", out)
	}
	clk.add(p.abandonAfter)
	p.tick()
	out := buf.String()
	if !strings.Contains(out, "上传中断（25.0%，1.0 MB/4.0 MB） /a.bin") || strings.Contains(out, "上传完成") {
		t.Fatalf("中断汇总不对：%q", out)
	}
}

// 续传下载：从 offset 开始读，百分比按文件总大小算，平均速度只算本次传的量。
func TestProgressResumedDownload(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	p.Event(agent.TransferEvent{Dir: "download", Path: "/var/log/big.log", Offset: 3 * mib, Bytes: 0, Total: 4 * mib})
	clk.add(time.Second)
	p.Event(agent.TransferEvent{Dir: "download", Path: "/var/log/big.log", Offset: 3 * mib, Bytes: mib, Total: 4 * mib, EOF: true})
	out := buf.String()
	if !strings.Contains(out, "下载完成 /var/log/big.log：1.0 MB，用时 00:01，平均 1.0 MB/s") {
		t.Fatalf("续传下载汇总不对：%q", out)
	}
}

// 进度行不能超出终端宽度，否则 \r 只能回到折行后的那一行，屏幕上会越刷越多。
func TestProgressLineFitsTerminalWidth(t *testing.T) {
	for _, width := range []int{120, 80, 60, 40, 20} {
		p, buf, _ := testProgress(true, width)
		p.Event(agent.TransferEvent{Dir: "upload", Path: `D:\资料\一个名字特别特别长的安装包_最终版_真的最终版.apk`, Offset: 0, Bytes: mib, Total: 3 * mib})
		p.tick()
		frame := lastFrame(buf.String())
		if w := displayWidth(frame); w > width-1 {
			t.Fatalf("width=%d: 进度行占 %d 列：%q", width, w, frame)
		}
		if width >= 80 && !strings.Contains(frame, "33.3%") {
			t.Fatalf("width=%d: 百分比被裁掉了：%q", width, frame)
		}
	}
}

// 非终端输出（重定向到文件）不能用 \r，改为隔一段时间打一整行。
func TestProgressPlainModeForNonTTY(t *testing.T) {
	p, buf, clk := testProgress(false, 0)
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/a.bin", Total: 4 * mib, Start: true})
	for i := int64(0); i < 4; i++ {
		clk.add(2 * time.Second)
		p.tick()
		p.Event(agent.TransferEvent{Dir: "upload", Path: "/a.bin", Offset: i * mib, Bytes: mib, Total: 4 * mib})
	}
	out := buf.String()
	if strings.Contains(out, "\r") {
		t.Fatalf("非终端输出里出现了 \\r：%q", out)
	}
	if n := len(regexp.MustCompile(`(?m)^上传 a\.bin`).FindAllString(out, -1)); n != 1 {
		t.Fatalf("8 秒里按 5 秒一行应打 1 行进度，实际 %d：%q", n, out)
	}
	if !strings.Contains(out, "上传完成 /a.bin：4.0 MB") {
		t.Fatalf("缺少完成汇总：%q", out)
	}
}

func TestTruncateMiddleRespectsWidth(t *testing.T) {
	for _, s := range []string{"abcdefghijklmnop", "一二三四五六七八九十", "a一b二c三d四"} {
		for max := 1; max <= 14; max++ {
			if got := truncateMiddle(s, max); displayWidth(got) > max {
				t.Fatalf("truncateMiddle(%q,%d)=%q 占 %d 列", s, max, got, displayWidth(got))
			}
		}
	}
}

// 窗口被拉窄后，擦除和补齐用的空格也不能超过当前宽度，否则会折行留下残影。
func TestProgressShrinkingWindowDoesNotWrap(t *testing.T) {
	width := 120
	var buf bytes.Buffer
	p := newTransferProgress(&buf, true, func() int { return width })
	p.logf = func(string, ...any) {}
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/" + strings.Repeat("长", 30) + ".bin", Offset: 0, Bytes: mib, Total: 3 * mib})
	p.tick()
	if p.drawn <= 40 {
		t.Fatalf("前提不成立：首帧只有 %d 列", p.drawn)
	}
	width = 40
	buf.Reset()
	p.tick()
	p.Print("x\n")
	for _, seg := range strings.FieldsFunc(buf.String(), func(r rune) bool { return r == '\r' || r == '\n' }) {
		if w := displayWidth(seg); w > width-1 {
			t.Fatalf("窄窗口下输出了 %d 列的片段：%q", w, seg)
		}
	}
}

// 慢链路（块间隔远超几秒，或 help 端重试退避）上不能把一次传输切成一串「中断」汇总。
func TestProgressSlowLinkStaysOneTransfer(t *testing.T) {
	for _, dir := range []string{"upload", "download"} {
		p, buf, clk := testProgress(true, 120)
		const total = 4 * fileTransferChunk
		if dir == "upload" {
			p.Event(agent.TransferEvent{Dir: dir, Path: "/a", Total: total, Start: true})
		}
		for i := int64(0); i < 4; i++ {
			for j := 0; j < 25; j++ { // 每块 5 秒
				clk.add(200 * time.Millisecond)
				p.tick()
			}
			p.Event(agent.TransferEvent{Dir: dir, Path: "/a", Offset: i * fileTransferChunk, Bytes: fileTransferChunk, Total: total, EOF: dir == "download" && i == 3})
		}
		out := buf.String()
		if strings.Contains(out, "中断") || strings.Count(out, "完成") != 1 {
			t.Fatalf("%s: 慢链路被拆成了多次传输：%q", dir, out)
		}
		if !strings.Contains(out, "停滞") {
			t.Fatalf("%s: 块间隔 5 秒时进度行应标出停滞：%q", dir, out)
		}
	}
}

// 文件大小恰为块大小整数倍时，下载在读满最后一块后还会再读一次拿到 0 字节 + EOF；
// 那次读不能变成第二条「下载完成 0 B」。
func TestProgressDownloadExactMultipleOfChunk(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	const total = 2 * fileTransferChunk
	p.Event(agent.TransferEvent{Dir: "download", Path: "/a", Offset: 0, Bytes: fileTransferChunk, Total: total})
	clk.add(time.Second)
	p.Event(agent.TransferEvent{Dir: "download", Path: "/a", Offset: fileTransferChunk, Bytes: fileTransferChunk, Total: total})
	clk.add(100 * time.Millisecond)
	p.Event(agent.TransferEvent{Dir: "download", Path: "/a", Offset: total, Bytes: 0, Total: total, EOF: true})
	out := buf.String()
	if strings.Count(out, "下载完成") != 1 || !strings.Contains(out, "下载完成 /a：1.0 MB") {
		t.Fatalf("应只有一条 1.0 MB 的完成汇总：%q", out)
	}
}

// 正在增长的文件（比如日志）下载时超过 stat 时的大小，不能提前判完成。
func TestProgressDownloadGrowingFile(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	for i := int64(0); i < 3; i++ {
		clk.add(time.Second)
		p.Event(agent.TransferEvent{Dir: "download", Path: "/log", Offset: i * mib, Bytes: mib, Total: 2 * mib, EOF: i == 2})
	}
	out := buf.String()
	if strings.Count(out, "下载完成") != 1 || !strings.Contains(out, "下载完成 /log：3.0 MB") {
		t.Fatalf("增长中的文件应一次完成、共 3.0 MB：%q", out)
	}
}

// 单块文件（< 512 KiB，最常见）的响应丢了被重试：不能打两条完成汇总。
func TestProgressSingleChunkRetry(t *testing.T) {
	for _, dir := range []string{"upload", "download"} {
		p, buf, clk := testProgress(true, 120)
		if dir == "upload" {
			p.Event(agent.TransferEvent{Dir: dir, Path: "/a", Total: 100, Start: true})
		}
		for i := 0; i < 2; i++ {
			p.Event(agent.TransferEvent{Dir: dir, Path: "/a", Offset: 0, Bytes: 100, Total: 100, EOF: dir == "download"})
			clk.add(time.Second)
		}
		if out := buf.String(); strings.Count(out, "完成") != 1 {
			t.Fatalf("%s: 单块重试打了多条汇总：%q", dir, out)
		}
	}
}

// 同一文件收尾后马上再下载一遍（多块）：从起点开始的新传输要照常显示。
func TestProgressRedownloadRightAfterFinish(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	for round := 0; round < 2; round++ {
		for i := int64(0); i < 2; i++ {
			clk.add(time.Second)
			p.Event(agent.TransferEvent{Dir: "download", Path: "/a", Offset: i * mib, Bytes: mib, Total: 2 * mib, EOF: i == 1})
		}
	}
	if out := buf.String(); strings.Count(out, "下载完成 /a：2.0 MB") != 2 {
		t.Fatalf("两次下载应各有一条完成汇总：%q", out)
	}
}

// 下载没有 Start 事件，计时从第一块到达才开始，平均速度必须扣掉第一块，否则虚高。
func TestProgressAverageExcludesUntimedFirstChunk(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	for i := int64(0); i < 2; i++ {
		p.Event(agent.TransferEvent{Dir: "download", Path: "/a", Offset: i * mib, Bytes: mib, Total: 2 * mib, EOF: i == 1})
		clk.add(time.Second)
	}
	if out := buf.String(); !strings.Contains(out, "下载完成 /a：2.0 MB，用时 00:01，平均 1.0 MB/s") {
		t.Fatalf("平均速度应为 1.0 MB/s：%q", out)
	}
}

// 成功的传输分块不再逐条写日志，但开始与结束必须进审计日志（stderr 被重定向时
// 这是唯一的记录）。日志经 Writer 写回同一个渲染器也不能死锁。
func TestProgressWritesAuditLogWithoutDeadlock(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	var logBuf bytes.Buffer
	logW := p.Writer(&logBuf)
	p.logf = func(format string, args ...any) { fmt.Fprintf(logW, format+"\n", args...) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Event(agent.TransferEvent{Dir: "upload", Path: "/a", Total: 2 * mib, Start: true})
		clk.add(time.Second)
		p.Event(agent.TransferEvent{Dir: "upload", Path: "/a", Offset: 0, Bytes: mib, Total: 2 * mib})
		p.tick()
		clk.add(time.Second)
		p.Event(agent.TransferEvent{Dir: "upload", Path: "/a", Offset: mib, Bytes: mib, Total: 2 * mib})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("审计日志经 Writer 回写时死锁")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "tool | transfer | upload /a | start offset=0 total=2097152") ||
		!strings.Contains(logs, "tool | transfer | [12:00:02] 上传完成 /a：2.0 MB") {
		t.Fatalf("审计日志缺开始或结束：%q", logs)
	}
	if strings.Contains(buf.String(), "tool | transfer") {
		t.Fatalf("审计日志不该写进 stdout：%q", buf.String())
	}
}

// 退出时进度行还在，要补换行，免得 shell 提示符接在进度行后面。
func TestProgressStopEndsLine(t *testing.T) {
	p, buf, _ := testProgress(true, 120)
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/a", Offset: 0, Bytes: mib, Total: 4 * mib})
	p.tick()
	p.Stop()
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("Stop 后没有换行：%q", buf.String())
	}
}

// 被当成重复吞掉、不再显示的分块，审计日志里也必须有记录：单块小文件 10 秒内被读
// 两次、或对端反复改写同一位置，都不能在 share 端完全看不见。
func TestProgressRepeatedChunksAreAudited(t *testing.T) {
	p, _, clk := testProgress(true, 120)
	var logs []string
	p.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	p.Event(agent.TransferEvent{Dir: "download", Path: "/etc/shadow", Offset: 0, Bytes: 100, Total: 100, EOF: true})
	clk.add(5 * time.Second)
	p.Event(agent.TransferEvent{Dir: "download", Path: "/etc/shadow", Offset: 0, Bytes: 100, Total: 100, EOF: true})
	for i := 0; i < 3; i++ {
		clk.add(2 * time.Second)
		p.Event(agent.TransferEvent{Dir: "upload", Path: "/x/run.sh", Offset: 0, Bytes: 100})
	}
	all := strings.Join(logs, "\n")
	if n := strings.Count(all, "download /etc/shadow | repeated chunk offset=0 bytes=100"); n != 1 {
		t.Fatalf("重复读应记 1 条审计，实际 %d：\n%s", n, all)
	}
	if n := strings.Count(all, "upload /x/run.sh | repeated chunk offset=0 bytes=100"); n != 2 {
		t.Fatalf("同一位置的 3 次写应记 2 条重复审计，实际 %d：\n%s", n, all)
	}
}

// Stop 之后迟到的 tick 或活动行不能再把进度行画回来，盖掉收尾换行。
func TestProgressNoRedrawAfterStop(t *testing.T) {
	p, buf, _ := testProgress(true, 120)
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/a", Offset: 0, Bytes: mib, Total: 4 * mib})
	p.Stop()
	p.tick()
	p.Print("Session ended\n")
	if out := buf.String(); strings.Contains(out, "上传 a") || !strings.HasSuffix(out, "Session ended\n") {
		t.Fatalf("Stop 后又画了进度行：%q", out)
	}
}

// stat 的大小比实际可读的大（/proc 下的文件、下载中被轮转的日志）：读到 EOF 就是完成。
func TestProgressDownloadShorterThanStat(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	p.Event(agent.TransferEvent{Dir: "download", Path: "/proc/x", Offset: 0, Bytes: 100, Total: 4096})
	clk.add(time.Second)
	p.Event(agent.TransferEvent{Dir: "download", Path: "/proc/x", Offset: 100, Bytes: 0, Total: 4096, EOF: true})
	if out := buf.String(); strings.Contains(out, "中断") || !strings.Contains(out, "下载完成 /proc/x：100 B") {
		t.Fatalf("读到 EOF 应报完成：%q", out)
	}
}

// 上传途中本地文件长大：到达 stat 时的 total 后紧接着的分块并回原传输，
// 不能每块各开一轮、各打一行汇总。
func TestProgressUploadGrowingLocalFile(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/a", Total: 2 * mib, Start: true})
	for i := int64(0); i < 4; i++ {
		clk.add(time.Second)
		p.Event(agent.TransferEvent{Dir: "upload", Path: "/a", Offset: i * mib, Bytes: mib, Total: 2 * mib})
	}
	clk.add(p.unknownIdle)
	p.tick()
	out := buf.String()
	if strings.Count(out, "上传完成") != 2 || !strings.Contains(out, "上传完成 /a：4.0 MB") || strings.Contains(out, "中断") {
		t.Fatalf("长大的文件应并回原传输、最终报 4.0 MB：%q", out)
	}
	if strings.Contains(out, "上传完成 /a：1.0 MB") {
		t.Fatalf("后续分块不应各自开新传输：%q", out)
	}
}

// 小文件几毫秒就传完，算出来的「平均 236 KB/s」没有意义，不显示。
func TestProgressNoAverageForInstantTransfer(t *testing.T) {
	p, buf, clk := testProgress(true, 120)
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/s.txt", Total: 1000, Start: true})
	clk.add(4 * time.Millisecond)
	p.Event(agent.TransferEvent{Dir: "upload", Path: "/s.txt", Offset: 0, Bytes: 1000, Total: 1000})
	if out := buf.String(); !strings.Contains(out, "上传完成 /s.txt：1000 B，用时 00:00\n") {
		t.Fatalf("瞬间完成的传输不应显示平均速度：%q", out)
	}
}

// log 与进度在同一个控制台（quietSummaryLogs）时只省掉和屏幕汇总重复的开始/结束日志，
// 重复分块不上屏，日志必须照写，否则又回到了「对端读写完全看不见」。
func TestProgressQuietSummaryKeepsRepeatedChunkLogs(t *testing.T) {
	p, _, clk := testProgress(true, 120)
	p.quietSummaryLogs = true
	var logs []string
	p.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	for i := 0; i < 2; i++ {
		p.Event(agent.TransferEvent{Dir: "download", Path: "/etc/shadow", Offset: 0, Bytes: 100, Total: 100, EOF: true})
		clk.add(time.Second)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "download /etc/shadow | repeated chunk offset=0") {
		t.Fatalf("应只剩 1 条重复分块日志：%q", logs)
	}
}
