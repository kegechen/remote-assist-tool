package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/remote-assist/tool/internal/agent"
)

type TailLogArgs struct {
	Path   string `json:"path"`
	Lines  int    `json:"lines,omitempty"`
	Follow bool   `json:"follow,omitempty"`
}

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
	if sink != nil && len(last) > 0 {
		sink.Send("stdout", last)
	}

	if !a.Follow {
		return json.RawMessage(`{"followed":false}`), nil
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	defer w.Close()
	w.Add(resolved)

	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return json.RawMessage(`{"followed":true,"closed":"context"}`), nil
		case ev, ok := <-w.Events:
			if !ok {
				return json.RawMessage(`{"followed":true,"closed":"watcher"}`), nil
			}
			if ev.Op&fsnotify.Write != 0 {
				for {
					n, _ := f.Read(buf)
					if n == 0 {
						break
					}
					if sink != nil {
						sink.Send("stdout", append([]byte{}, buf[:n]...))
					}
				}
			}
		case <-time.After(500 * time.Millisecond):
			for {
				n, _ := f.Read(buf)
				if n == 0 {
					break
				}
				if sink != nil {
					sink.Send("stdout", append([]byte{}, buf[:n]...))
				}
			}
		}
	}
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
