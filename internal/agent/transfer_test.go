package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

func TestBase64JSONLen(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 4, 5, 524288, 524289} {
		raw, _ := json.Marshal(make([]byte, n))
		if got := base64JSONLen(raw); got != int64(n) {
			t.Fatalf("n=%d: got %d (raw %.20s)", n, got, raw)
		}
	}
	if got := base64JSONLen(json.RawMessage(`null`)); got != 0 {
		t.Fatalf("null: got %d", got)
	}
}

func TestParseTransfer(t *testing.T) {
	content := base64.StdEncoding.EncodeToString(make([]byte, 1000))
	cases := []struct {
		name   string
		tool   string
		args   string
		result string
		want   TransferEvent
		ok     bool
	}{
		{name: "上传分块带 total", tool: "write_file",
			args: `{"path":"C:\\a.apk","content":"` + content + `","at":524288,"total":4000}`,
			want: TransferEvent{Dir: "upload", Path: `C:\a.apk`, Offset: 524288, Bytes: 1000, Total: 4000}, ok: true},
		{name: "旧版 help 的上传分块不带 total 也要认出来", tool: "write_file",
			args: `{"path":"/a","content":"` + content + `","at":0}`,
			want: TransferEvent{Dir: "upload", Path: "/a", Bytes: 1000}, ok: true},
		{name: "从头传前的 truncate", tool: "write_file",
			args: `{"path":"/a","content":"","create":true,"total":4000}`,
			want: TransferEvent{Dir: "upload", Path: "/a", Total: 4000, Start: true}, ok: true},
		{name: "普通 write_file（GUI 编辑器保存）照常逐条显示", tool: "write_file",
			args: `{"path":"/a","content":"` + content + `","create":true}`},
		{name: "旧版 help 的 truncate 不带 total", tool: "write_file",
			args: `{"path":"/a","content":"","create":true}`},
		{name: "下载分块", tool: "read_file",
			args:   `{"path":"/a","offset":1048576,"length":524288,"total":2000000}`,
			result: `{"bytes":"` + content + `","eof":true}`,
			want:   TransferEvent{Dir: "download", Path: "/a", Offset: 1048576, Bytes: 1000, Total: 2000000, EOF: true}, ok: true},
		{name: "普通 read_file 不算传输", tool: "read_file",
			args: `{"path":"/a","offset":10}`, result: `{"bytes":"` + content + `","eof":false}`},
		{name: "其他工具", tool: "exec", args: `{"argv":["x"],"at":1}`},
	}
	for _, c := range cases {
		got, ok := parseTransfer(c.tool, json.RawMessage(c.args), json.RawMessage(c.result))
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%+v, %v), want (%+v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// scriptedTool 以给定名字注册，返回固定结果或错误。
type scriptedTool struct {
	name   string
	result json.RawMessage
	err    error
}

func (s scriptedTool) Name() string { return s.name }
func (s scriptedTool) Run(context.Context, json.RawMessage, StreamSink) (json.RawMessage, error) {
	return s.result, s.err
}

// 传输分块成功时只走 OnTransfer、不再逐块产生活动行；失败的分块必须照常出现在
// 活动行里，不能被进度聚合吞掉。
func TestDaemonRoutesTransferChunksToOnTransfer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		wantTransfer bool
	}{
		{name: "成功分块", wantTransfer: true},
		{name: "失败分块", err: errors.New("disk full")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := make(chan *proto.Message, 4)
			out := make(chan *proto.Message, 4)
			r := NewRegistry()
			r.Register(scriptedTool{name: "write_file", result: json.RawMessage(`{"bytes_written":3}`), err: tc.err})
			d := NewDaemon(r, &fakeConn{in: in, out: out}, proto.Session{})
			var mu sync.Mutex
			var activities []string
			var transfers []TransferEvent
			d.OnActivity = func(line string) { mu.Lock(); activities = append(activities, line); mu.Unlock() }
			d.OnTransfer = func(ev TransferEvent) { mu.Lock(); transfers = append(transfers, ev); mu.Unlock() }
			go d.RunLoop(context.Background())

			req := proto.ToolReq{ID: 1, Tool: "write_file", ArgsJSON: json.RawMessage(`{"path":"/a","content":"YWJj","at":0,"total":3}`)}
			msg, _ := proto.NewMessage(proto.MsgToolReq, &req)
			d.Inject(msg)
			select {
			case <-out:
			case <-time.After(time.Second):
				t.Fatal("no response in 1s")
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.wantTransfer {
				if len(transfers) != 1 || transfers[0].Bytes != 3 || len(activities) != 0 {
					t.Fatalf("transfers=%+v activities=%q", transfers, activities)
				}
			} else if len(transfers) != 0 || len(activities) != 1 {
				t.Fatalf("transfers=%+v activities=%q", transfers, activities)
			}
		})
	}
}

// 没设 OnTransfer（旧调用方）时行为不变：分块照旧逐条走 OnActivity。
func TestDaemonWithoutOnTransferKeepsActivityLines(t *testing.T) {
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 4)
	r := NewRegistry()
	r.Register(scriptedTool{name: "write_file", result: json.RawMessage(`{"bytes_written":3}`)})
	d := NewDaemon(r, &fakeConn{in: in, out: out}, proto.Session{})
	got := make(chan string, 1)
	d.OnActivity = func(line string) { got <- line }
	go d.RunLoop(context.Background())
	req := proto.ToolReq{ID: 1, Tool: "write_file", ArgsJSON: json.RawMessage(`{"path":"/a","content":"YWJj","at":0}`)}
	msg, _ := proto.NewMessage(proto.MsgToolReq, &req)
	d.Inject(msg)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("activity line missing")
	}
}
