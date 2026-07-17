package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"

	"github.com/remote-assist/tool/internal/agent"
)

type TailLogArgs struct {
	Path  string `json:"path"`
	Lines int    `json:"lines,omitempty"`
	// Follow は v1 では非対応。フィールドは後方互換のため残す。
	Follow bool `json:"follow,omitempty"`
}

type TailLogResult struct {
	Followed       bool   `json:"followed"`
	Lines          string `json:"lines,omitempty"`
	LinesTruncated bool   `json:"lines_truncated,omitempty"`
}

const tailLogMaxOutput = 64 * 1024 // 64 KiB

type TailLogTool struct{ sb *agent.Sandbox }

func NewTailLog(sb *agent.Sandbox) *TailLogTool { return &TailLogTool{sb: sb} }
func (t *TailLogTool) Name() string             { return "tail_log" }

func (t *TailLogTool) Run(ctx context.Context, raw json.RawMessage, sink agent.StreamSink) (json.RawMessage, error) {
	var a TailLogArgs
	json.Unmarshal(raw, &a)
	if a.Lines == 0 {
		a.Lines = 100
	}
	resolved, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	last, err := readLastLines(resolved, a.Lines)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, err
	}
	// 同时走 sink（兼容旧 consumer）和把内容放进 result。result 是权威来源：调用方不要
	// 流式时（Claude、GUI 的非终端调用都不要）没人接 sink，块会被 bridge 丢掉，所以
	// lines 必须嵌在 result 里。
	if sink != nil && len(last) > 0 {
		sink.Send("stdout", last)
	}
	// 保留尾部：tail_log 的语义就是看最新的内容，截掉头部才是对的。
	// 截头会丢掉刚写进日志的那几行——恰恰是排查时唯一要看的东西。
	lines := string(last)
	linesTrunc := false
	if len(lines) > tailLogMaxOutput {
		lines = TruncateTail(lines, tailLogMaxOutput)
		linesTrunc = true
	}
	return json.Marshal(TailLogResult{Followed: false, Lines: lines, LinesTruncated: linesTrunc})
}

func readLastLines(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ring []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	var out []byte
	for _, l := range ring {
		out = append(out, l...)
		out = append(out, '\n')
	}
	return out, nil
}
