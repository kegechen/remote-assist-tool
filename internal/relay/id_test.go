package relay

import (
	"strings"
	"testing"
)

// minIDRandomChars 128 bit 用 base32 编码后的字符数（16 字节 × 8 / 5 = 25.6 → 26）。
const minIDRandomChars = 26

// randomTail 取 "前缀_时间戳_随机段" 的最后一段。
func randomTail(t *testing.T, id string) string {
	t.Helper()
	i := strings.LastIndex(id, "_")
	if i < 0 {
		t.Fatalf("ID 没有随机段: %s", id)
	}
	return id[i+1:]
}

// TestIDEntropy 连接 ID 与会话 ID 的随机段必须有 128 bit。
//
// 这两个不是普通日志标识：连接 ID 是 byConnID 的唯一路由键（FindPeer 靠它决定把
// tunnel_data / tool_resp 投给谁），会话 ID 还兼作 P2P 打洞与 UDP relay 的准入凭据。
// 原来是 randomString(6)/randomString(8)，即 36^6≈2^31 / 36^8≈2^41——2^31 在持续打满
// create/join 限流的情况下是能撞出来的，撞了就是两条会话共用一条路由。
func TestIDEntropy(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  func() string
	}{
		{"client", generateClientID},
		{"session", generateSessionID},
	} {
		tail := randomTail(t, tc.gen())
		if len(tail) < minIDRandomChars {
			t.Errorf("%s ID 随机段只有 %d 字符（需要 >=%d，即 128 bit）: %q",
				tc.name, len(tail), minIDRandomChars, tail)
		}
	}
}

// TestIDsDoNotRepeat 同一秒内连发也不能重复（时间戳段相同，全靠随机段区分）。
func TestIDsDoNotRepeat(t *testing.T) {
	seen := make(map[string]struct{}, 2000)
	for i := 0; i < 1000; i++ {
		for _, id := range []string{generateClientID(), generateSessionID()} {
			if _, dup := seen[id]; dup {
				t.Fatalf("ID 重复: %s", id)
			}
			seen[id] = struct{}{}
		}
	}
}

// TestRegisterClientRejectsCollision 撞键时必须换 ID，绝不能覆盖已在线的那条连接。
//
// clients 表是覆盖写：撞键会让先到的连接从表里消失，它的 Send channel 再也没人写，
// 而 defer 里的 delete(s.clients, clientID) 又会把后来者一并删掉——两条连接一起坏，
// 且没有任何日志指向根因。
func TestRegisterClientRejectsCollision(t *testing.T) {
	s := &Server{clients: make(map[string]*ClientConn)}
	const dup = "cli_20260903000000_collide"

	first := &ClientConn{ID: dup, Send: make(chan []byte, 1)}
	s.clients[dup] = first

	second := &ClientConn{ID: dup, Send: make(chan []byte, 1)}
	got := s.registerClient(second)

	if got == dup {
		t.Fatal("撞键的连接仍用了同一个 ID，先到的那条被覆盖")
	}
	if s.clients[dup] != first {
		t.Fatal("已在线的连接被顶掉了")
	}
	if s.clients[got] != second {
		t.Fatalf("新连接没按新 ID %s 登记", got)
	}
	if second.ID != got {
		t.Fatalf("ClientConn.ID 没同步更新: %s != %s", second.ID, got)
	}
}
