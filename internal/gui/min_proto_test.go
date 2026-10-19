package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 「兼容旧版」只在 connect 上放宽下限，没勾时绝不能带 min_proto：
// 空串也算一个值，help 会把它当非法取值拒掉。
func TestConnectToolArgsMinProto(t *testing.T) {
	if _, ok := connectToolArgs("ABCD-EFGHIJ", "", false, "")["min_proto"]; ok {
		t.Fatal("未勾选兼容旧版时不应带 min_proto")
	}
	if got := connectToolArgs("ABCD-EFGHIJ", "", false, "1")["min_proto"]; got != "1" {
		t.Fatalf("min_proto = %v, want 1", got)
	}
}

// 自动重连读的是 lastMinProto；状态面板读的是 protoVersion。
func TestRecordConnectMetadataKeepsMinProto(t *testing.T) {
	s := &Server{}
	raw := json.RawMessage(`{"content":[{"type":"text","text":"{\"connected\":true,\"proto_version\":\"1\"}"}]}`)
	s.recordConnectMetadata(raw, "ABCD-EFGHIJ", "", false, "1")
	if s.lastMinProto != "1" {
		t.Fatalf("lastMinProto = %q, want 1（否则断线自动重连会退回默认值被拒）", s.lastMinProto)
	}
	if s.protoVersion != "1" {
		t.Fatalf("protoVersion = %q, want 1", s.protoVersion)
	}
}

// reconnectPeer 是升级回滚与回连旧 share 的路径，min_proto 必须真的上线。
func TestReconnectPeerSendsMinProto(t *testing.T) {
	c, srv := newFakeClient()
	defer c.Close()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := reconnectPeer(ctx, c, "ABCD-EFGHIJ", "", false, "1")
		done <- err
	}()

	if !srv.fromClient.Scan() {
		t.Fatal("client 没发出请求")
	}
	var req struct {
		ID     int64 `json:"id"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(srv.fromClient.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	if req.Params.Name != "connect" || req.Params.Arguments["min_proto"] != "1" {
		t.Fatalf("请求应为 connect 且带 min_proto=1，实际 %s", srv.fromClient.Bytes())
	}
	srv.respond(t, req.ID, `{"content":[{"type":"text","text":"{\"connected\":true}"}]}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestMain 让测试二进制兼任一个假的 `remote help --mcp-stdio` 子进程：设了
// RAT_GUI_FAKE_MCP_LOG 时，它只回 initialize / connect，并把 connect 的参数写进该文件。
// 这样 tryReconnect 走的是真实的建子进程 → initialize → connect 全流程。
func TestMain(m *testing.M) {
	if logPath := os.Getenv("RAT_GUI_FAKE_MCP_LOG"); logPath != "" {
		runFakeMCP(logPath)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFakeMCP(logPath string) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		var result any = map[string]any{}
		if req.Method == "tools/call" && req.Params.Name == "connect" {
			os.WriteFile(logPath, req.Params.Arguments, 0o644)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": `{"connected":true,"proto_version":"1"}`}}}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		os.Stdout.Write(append(b, '\n'))
	}
}

// 守护线程的自动重连必须把「兼容旧版」带上：靠它才连上 0.0.x 的会话，断线重连若退回
// 默认值就会被拒，GUI 表现为"自动重连失败"。
func TestTryReconnectSendsMinProto(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "connect-args.json")
	t.Setenv("RAT_GUI_FAKE_MCP_LOG", logPath)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(exe, "")
	if got := s.tryReconnect(s.connGen, "ABCD-EFGHIJ", "", false, "1"); got != reconnectOK {
		t.Fatalf("tryReconnect = %v, want reconnectOK", got)
	}
	defer s.client.Close()

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("假子进程没收到 connect: %v", err)
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args["min_proto"] != "1" {
		t.Fatalf("自动重连的 connect 参数应带 min_proto=1，实际 %s", raw)
	}
	if s.lastMinProto != "1" || s.protoVersion != "1" {
		t.Fatalf("重连后应保留 lastMinProto/protoVersion，实际 %q/%q", s.lastMinProto, s.protoVersion)
	}
}
