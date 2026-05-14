package agent

import (
	"context"
	"encoding/json"
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
