package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingPTYClient 的 pty_write 在 release 关闭前阻塞（模拟对端 PTY 缓冲满）；记录每次写入的数据。
type blockingPTYClient struct {
	mu      sync.Mutex
	release chan struct{}
	writes  [][]byte
}

func (c *blockingPTYClient) CallTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	if name == "pty_write" {
		data, _ := args["data"].([]byte)
		if len(data) > 1 || (len(data) == 1 && data[0] != 0x03) {
			select { // 大块/普通输入被卡住
			case <-c.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		c.mu.Lock()
		c.writes = append(c.writes, data)
		c.mu.Unlock()
	}
	return json.RawMessage(`{}`), nil
}
func (c *blockingPTYClient) CallToolStream(context.Context, string, map[string]any, func(string, []byte)) (json.RawMessage, error) {
	return nil, nil
}

func postPTYWrite(h http.Handler, id string, data []byte) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"id": id, "data": data})
	req := httptest.NewRequest(http.MethodPost, "/api/pty/write", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 某次 /api/pty/write 被卡住（大段粘贴）时，后续的 Ctrl+C 不能排在会话锁后面，必须立刻送达被控端。
func TestPTYWriteCtrlCNotQueuedBehindBlockedWrite(t *testing.T) {
	s := &Server{}
	h := http.HandlerFunc(s.handlePTYWrite)
	cl := &blockingPTYClient{release: make(chan struct{})}
	const id = "urgent-test"
	ptySessions.Store(id, newPTYEntry(cl, func() {}))
	defer ptySessions.Delete(id)

	blocked := make(chan struct{})
	go func() {
		postPTYWrite(h, id, []byte(strings.Repeat("x", 4096)))
		close(blocked)
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	rec := postPTYWrite(h, id, []byte{0x03})
	if rec.Code != http.StatusOK {
		t.Fatalf("Ctrl+C 返回 %d: %s", rec.Code, rec.Body.String())
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Ctrl+C 被卡了 %v", d)
	}
	cl.mu.Lock()
	if len(cl.writes) != 1 || len(cl.writes[0]) != 1 || cl.writes[0][0] != 0x03 {
		t.Fatalf("被控端应先收到 0x03，实际 %v", cl.writes)
	}
	cl.mu.Unlock()
	close(cl.release)
	<-blocked
}

func TestIsUrgentPTYInput(t *testing.T) {
	for _, b := range [][]byte{{0x03}, {0x1a}, {0x1c}, []byte("ab\x03")} {
		if !isUrgentPTYInput(b) {
			t.Errorf("%q 应是紧急输入", b)
		}
	}
	for _, b := range [][]byte{nil, []byte("ls\r"), {0x04}, []byte(strings.Repeat("\x03", 200))} {
		if isUrgentPTYInput(b) {
			t.Errorf("%q 不应是紧急输入", b)
		}
	}
}
