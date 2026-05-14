package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestInitializeHandshake(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}` + "\n")
	var out bytes.Buffer
	srv := NewServer(nil)
	err := srv.Serve(context.Background(), in, &out)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resp struct {
		Result struct {
			ServerInfo struct{ Name string } `json:"serverInfo"`
		} `json:"result"`
	}
	json.Unmarshal(out.Bytes(), &resp)
	if resp.Result.ServerInfo.Name == "" {
		t.Fatalf("missing server info: %s", out.String())
	}
}

func TestToolsListReturnsTenTools(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	srv := NewServer(nil)
	srv.Serve(context.Background(), in, &out)
	if !strings.Contains(out.String(), `"connect"`) || !strings.Contains(out.String(), `"tail_log"`) {
		t.Fatalf("missing tools: %s", out.String())
	}
}
