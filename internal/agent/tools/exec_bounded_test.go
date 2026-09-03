package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// feed 把 data 按 chunk 大小分批写进 s，模拟管道零散到达的真实情形。
func feed(s *boundedStream, data []byte, chunk int) {
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		s.Write(data[off:end])
	}
}

// TestBoundedStreamMatchesTruncateMiddle 是 boundedStream 的核心契约：边写边截的结果
// 必须与"先攒全量再 TruncateMiddle"逐字节相同——包括省略标记里那个真实字节数。不然
// 换成流式截断就等于悄悄改了 exec 的输出格式。
func TestBoundedStreamMatchesTruncateMiddle(t *testing.T) {
	// 混入多字节字符，专门压 alignDown/alignUp 的边界对齐
	unit := []byte("行abc中文行xyz\n")
	cases := []struct {
		name  string
		size  int
		max   int
		chunk int
	}{
		{"未超限", 500, 4096, 64},
		{"恰好等于上限", 4096, 4096, 64},
		{"刚超一字节", 4097, 4096, 64},
		{"远超上限-小块写", 200 * 1024, 4096, 7},
		{"远超上限-大块写", 200 * 1024, 4096, 65536},
		{"单次写盖过整个尾窗口", 200 * 1024, 1024, 200 * 1024},
		{"上限小于省略标记预留", 10000, 64, 13},
		{"上限恰好等于标记预留", 10000, truncMarkerReserve, 13},
		{"上限刚超标记预留", 10000, truncMarkerReserve + 1, 13},
		{"默认上限", 1 << 20, execDefaultMaxOutput, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := bytes.Repeat(unit, tc.size/len(unit)+1)[:tc.size]
			s := newBoundedStream(tc.max)
			feed(s, full, tc.chunk)
			got, gotTrunc := s.result()

			wantStr, wantTrunc := TruncateMiddle(string(full), tc.max)
			if gotTrunc != wantTrunc {
				t.Fatalf("truncated=%v want %v", gotTrunc, wantTrunc)
			}
			if string(got) != wantStr {
				t.Fatalf("结果与 TruncateMiddle 不一致\n got len=%d %q...%q\nwant len=%d %q...%q",
					len(got), head(string(got)), tail(string(got)),
					len(wantStr), head(wantStr), tail(wantStr))
			}
			if len(got) > tc.max {
				t.Fatalf("超出 max: len=%d max=%d", len(got), tc.max)
			}
		})
	}
}

// TestBoundedStreamKeepsMemoryBounded 证明"边写边丢"确实在丢：写进 64 MiB，保留的
// 两段加起来仍是 max 的常数倍。这正是 cmd.Output() 做不到的事——它会实打实攒下 64 MiB。
func TestBoundedStreamKeepsMemoryBounded(t *testing.T) {
	const max = 4096
	s := newBoundedStream(max)
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 64; i++ {
		s.Write(chunk)
	}
	if retained := len(s.head) + len(s.tail); retained > 4*max {
		t.Fatalf("写入 64 MiB 后仍保留 %d 字节，应为 max(%d) 的常数倍", retained, max)
	}
	if s.total != 64<<20 {
		t.Fatalf("total=%d want %d", s.total, 64<<20)
	}
}

func TestResolveMaxOutputClampsToCeiling(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, execDefaultMaxOutput},
		{-1, execDefaultMaxOutput},
		{1024, 1024},
		{execMaxOutputCeiling, execMaxOutputCeiling},
		{execMaxOutputCeiling + 1, execMaxOutputCeiling},
		{1 << 40, execMaxOutputCeiling},
	}
	for _, tc := range cases {
		if got := resolveMaxOutput(tc.in); got != tc.want {
			t.Fatalf("resolveMaxOutput(%d)=%d want %d", tc.in, got, tc.want)
		}
	}
}

// TestExecTruncatesLargeOutput 端到端确认非流式 exec 仍按 max_output_bytes 截断，
// 且保留了尾部（编译错误 / panic 堆栈都在末尾，只保开头等于丢掉最关键的信息）。
func TestExecTruncatesLargeOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	const max = 4096
	tool := NewExec(nil)
	args, _ := json.Marshal(map[string]any{
		"argv":             []string{"/bin/sh", "-c", "i=0; while [ $i -lt 20000 ]; do echo line-$i; i=$((i+1)); done; echo TAIL-MARKER"},
		"max_output_bytes": max,
	})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !r.StdoutTruncated {
		t.Fatal("期望被截断")
	}
	if len(r.Stdout) > max {
		t.Fatalf("stdout len=%d 超过 max=%d", len(r.Stdout), max)
	}
	s := string(r.Stdout)
	if !strings.HasPrefix(s, "line-0\n") {
		t.Fatalf("头部丢了: %q", head(s))
	}
	if !strings.HasSuffix(s, "TAIL-MARKER\n") {
		t.Fatalf("尾部丢了: %q", tail(s))
	}
	if !strings.Contains(s, "字节已省略") {
		t.Fatalf("缺少省略标记: %q", s)
	}
}
