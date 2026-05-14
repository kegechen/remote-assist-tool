package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

type fakeTool struct{ name string }

func (f *fakeTool) Name() string { return f.name }
func (f *fakeTool) Run(ctx context.Context, in json.RawMessage, out StreamSink) (json.RawMessage, error) {
	return json.RawMessage(`{"echo":"` + f.name + `"}`), nil
}

func TestRegistryDispatchOK(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeTool{name: "ping"})
	resp := r.Dispatch(context.Background(), &proto.ToolReq{ID: 1, Tool: "ping", ArgsJSON: json.RawMessage(`{}`)}, nil)
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp)
	}
	if string(resp.ResultJSON) != `{"echo":"ping"}` {
		t.Fatalf("result: %s", resp.ResultJSON)
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	r := NewRegistry()
	resp := r.Dispatch(context.Background(), &proto.ToolReq{ID: 1, Tool: "ghost"}, nil)
	if resp.OK || resp.ErrorCode != "unknown_tool" {
		t.Fatalf("expected unknown_tool err, got %+v", resp)
	}
}

func TestSummarizeArgsExec(t *testing.T) {
	s := summarizeArgs("exec", json.RawMessage(`{"argv":["go","test","./..."]}`))
	if !strings.Contains(s, "go") || !strings.Contains(s, "argc=3") {
		t.Fatalf("got %q", s)
	}
}

func TestSummarizeArgsWriteFileNoContent(t *testing.T) {
	s := summarizeArgs("write_file", json.RawMessage(`{"path":"/a","content":"c2VjcmV0"}`))
	if strings.Contains(s, "c2VjcmV0") {
		t.Fatalf("leaked content: %q", s)
	}
}
