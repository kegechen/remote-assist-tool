package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// chunkConn 故意把单次 Write 拆成两半、中间让出调度，放大并发写交错。
// 若 sendMsg 不串行化写，多个 goroutine 的半截帧就会交叉进 buf，产生撕裂的非法行。
type chunkConn struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *chunkConn) Write(p []byte) (int, error) {
	half := len(p) / 2
	c.mu.Lock()
	c.buf.Write(p[:half])
	c.mu.Unlock()
	runtime.Gosched()
	c.mu.Lock()
	c.buf.Write(p[half:])
	c.mu.Unlock()
	return len(p), nil
}

func (c *chunkConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *chunkConn) Close() error             { return nil }
func (c *chunkConn) RemoteAddr() string       { return "test" }

// TestSendMsgConcurrentNoFrameCorruption 验证 per-client 写锁：并发 sendMsg 不撕裂帧。
func TestSendMsgConcurrentNoFrameCorruption(t *testing.T) {
	cc := &chunkConn{}
	client := &ClientConn{ID: "test", Conn: cc}

	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			msg, _ := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: uint64(id), OK: true})
			sendMsg(client, msg)
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(cc.buf.String(), "\n"), "\n")
	if len(lines) != N {
		t.Fatalf("期望 %d 行，实际 %d —— 帧交错（写未串行化）", N, len(lines))
	}
	seen := make(map[uint64]bool, N)
	for _, ln := range lines {
		var m proto.Message
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("帧损坏，非法 JSON 行: %q (%v)", ln, err)
		}
		var r proto.ToolResp
		if err := proto.DecodePayload(&m, &r); err != nil {
			t.Fatalf("payload 解码失败: %q (%v)", ln, err)
		}
		seen[r.ID] = true
	}
	if len(seen) != N {
		t.Fatalf("期望 %d 个不同 ID，实际 %d —— 有帧丢失", N, len(seen))
	}
}
