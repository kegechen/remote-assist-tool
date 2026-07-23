package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/ratelimit"
)

func TestAcquireConnSlotPerIPLimit(t *testing.T) {
	s := &Server{connPerIP: make(map[string]int), limits: DefaultLimits()}
	ip := "1.2.3.4"

	for i := 0; i < maxConnsPerIP; i++ {
		if !s.acquireConnSlot(ip) {
			t.Fatalf("acquire #%d should succeed (under per-IP limit)", i)
		}
	}
	if s.acquireConnSlot(ip) {
		t.Fatal("acquire beyond per-IP limit should fail")
	}
	// 另一个 IP 不受同一 IP 的限额影响
	if !s.acquireConnSlot("5.6.7.8") {
		t.Fatal("different IP should not be limited by another IP's count")
	}
	// 释放一个名额后该 IP 可再次获取
	s.releaseConnSlot(ip)
	if !s.acquireConnSlot(ip) {
		t.Fatal("acquire after release should succeed")
	}
}

func TestReleaseConnSlotCleansUpMap(t *testing.T) {
	s := &Server{connPerIP: make(map[string]int), limits: DefaultLimits()}
	ip := "1.2.3.4"

	s.acquireConnSlot(ip)
	s.releaseConnSlot(ip)

	if _, ok := s.connPerIP[ip]; ok {
		t.Fatal("per-IP entry should be deleted when its count reaches 0")
	}
	if s.connTotal != 0 {
		t.Fatalf("connTotal should be 0 after release, got %d", s.connTotal)
	}
}

func TestAcquireConnSlotTotalLimit(t *testing.T) {
	s := &Server{connPerIP: make(map[string]int), limits: DefaultLimits()}

	// 用各不相同的 IP 撑满全局上限（每个 IP 只占 1，不触发 per-IP 限额）
	for i := 0; i < maxConnsTotal; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)
		if !s.acquireConnSlot(ip) {
			t.Fatalf("acquire #%d should succeed (under total limit)", i)
		}
	}
	if s.acquireConnSlot("200.200.200.200") {
		t.Fatal("acquire beyond global total limit should fail")
	}
}

func TestDisableSourceIPLimitsStillEnforcesGlobalConnectionLimit(t *testing.T) {
	s := &Server{config: &Config{DisableSourceIPLimits: true}, connPerIP: make(map[string]int), limits: DefaultLimits()}
	for i := 0; i < maxConnsPerIP+1; i++ {
		if !s.acquireConnSlot("1.2.3.4") {
			t.Fatalf("关闭来源 IP 限制后第 %d 个同 IP 连接应放行", i+1)
		}
	}
	s.connTotal = maxConnsTotal
	if s.acquireConnSlot("5.6.7.8") {
		t.Fatal("关闭来源 IP 限制后全局连接上限仍应生效")
	}
}

func TestDisableSourceIPLimitsStillEnforcesPerConnectionDataLimit(t *testing.T) {
	srv, err := NewServer(&Config{DisableSourceIPLimits: true})
	if err != nil {
		t.Fatal(err)
	}
	srv.tunnelDataLimiterPerConn = ratelimit.NewKeyedLimiter(0, 0, 10, time.Minute)
	srv.tunnelDataLimiterGlobal = ratelimit.NewBucket(100, 100)
	if srv.allowDataMessage(&ClientConn{ID: "limited-conn"}) {
		t.Fatal("source-IP 旁路不得关闭 per-connection 数据限流")
	}
}

func TestDisableSourceIPLimitsBypassesSessionPerIPButKeepsGlobalCreateLimit(t *testing.T) {
	srv, err := NewServer(&Config{CodeTTL: time.Minute, CodeLength: 10, DisableSourceIPLimits: true})
	if err != nil {
		t.Fatal(err)
	}
	srv.createLimiterPerIP = ratelimit.NewKeyedLimiter(0, 0, 10, time.Minute)
	srv.createLimiterGlobal = ratelimit.NewBucket(0, maxActiveSessionsPerIP+1)
	for i := 0; i < maxActiveSessionsPerIP+1; i++ {
		client := &ClientConn{
			ID:   fmt.Sprintf("snat-share-%d", i),
			Conn: &capturingConn{remoteAddr: "10.10.10.10:5000"},
		}
		if srv.handleRegister(client) {
			t.Fatalf("SNAT 模式第 %d 个同 IP 会话不应被 per-IP 限制拒绝", i+1)
		}
	}
	if got := srv.sessions.GetActiveSessions(); got != maxActiveSessionsPerIP+1 {
		t.Fatalf("SNAT 模式活跃会话数=%d", got)
	}
	overflow := &ClientConn{ID: "snat-global-reject", Conn: &capturingConn{remoteAddr: "10.10.10.10:5001"}}
	if !srv.handleRegister(overflow) {
		t.Fatal("全局 create 桶耗尽后应拒绝新会话并关闭连接")
	}
	if got := srv.sessions.GetActiveSessions(); got != maxActiveSessionsPerIP+1 {
		t.Fatalf("全局拒绝后不应新增会话，实际=%d", got)
	}
}

// mockConn 实现 Conn 接口，捕获写出的数据用于断言
type mockConn struct {
	buf bytes.Buffer
}

func (m *mockConn) Read(p []byte) (int, error)         { return 0, nil }
func (m *mockConn) Write(p []byte) (int, error)        { return m.buf.Write(p) }
func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) RemoteAddr() string                 { return "127.0.0.1:9999" }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

func TestHandleRegisterNoAuth(t *testing.T) {
	cfg := &Config{
		CodeTTL:    5 * time.Minute,
		CodeLength: 10,
		NoAuth:     true,
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	mc := &mockConn{}
	client := &ClientConn{
		ID:   "test-client-1",
		Conn: mc,
		Send: make(chan []byte, 10),
	}

	srv.clientsMu.Lock()
	srv.clients[client.ID] = client
	srv.clientsMu.Unlock()

	srv.handleRegister(client)

	// 解析发送的 RegisterResponse
	data := mc.buf.Bytes()
	if len(data) == 0 {
		t.Fatal("handleRegister sent no data")
	}
	var msg proto.Message
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &msg); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, data)
	}
	if msg.Type != proto.MsgRegisterResponse {
		t.Fatalf("expected MsgRegisterResponse, got %s", msg.Type)
	}
	var resp proto.RegisterResponse
	if err := proto.DecodePayload(&msg, &resp); err != nil {
		t.Fatalf("decode RegisterResponse: %v", err)
	}
	if resp.Code != proto.NoAuthCode {
		t.Fatalf("expected code=%q in NoAuth mode, got %q", proto.NoAuthCode, resp.Code)
	}
}

func TestHandleRegisterNormalStillRandom(t *testing.T) {
	cfg := &Config{
		CodeTTL:    5 * time.Minute,
		CodeLength: 10,
		NoAuth:     false, // 正常模式
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	mc := &mockConn{}
	client := &ClientConn{
		ID:   "test-client-2",
		Conn: mc,
		Send: make(chan []byte, 10),
	}

	srv.clientsMu.Lock()
	srv.clients[client.ID] = client
	srv.clientsMu.Unlock()

	srv.handleRegister(client)

	data := mc.buf.Bytes()
	if len(data) == 0 {
		t.Fatal("handleRegister sent no data")
	}
	var msg proto.Message
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &msg); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, data)
	}
	var resp proto.RegisterResponse
	if err := proto.DecodePayload(&msg, &resp); err != nil {
		t.Fatalf("decode RegisterResponse: %v", err)
	}
	if resp.Code == proto.NoAuthCode {
		t.Fatal("normal mode should NOT return NoAuthCode")
	}
	if len(resp.Code) != 10 {
		t.Fatalf("expected 10-char random code, got %q (len=%d)", resp.Code, len(resp.Code))
	}
}

// TestNoAuthRegisterThenJoin 是 join 闭环回归测试：
// NoAuthCode 必须能在 register 存入 byCode 后、被 join 端 normalizeCode 处理仍命中。
// 历史 bug：NoAuthCode 曾为 "no-auth"（含连字符），register 按原值存 byCode["no-auth"]，
// 而 handleJoin 先 normalizeCode → "noauth" 查表，永远 invalid code。
func TestNoAuthRegisterThenJoin(t *testing.T) {
	cfg := &Config{CodeTTL: 5 * time.Minute, CodeLength: 10, NoAuth: true}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	share := &ClientConn{ID: "share-1", Conn: &mockConn{}, Send: make(chan []byte, 10)}
	srv.clientsMu.Lock()
	srv.clients[share.ID] = share
	srv.clientsMu.Unlock()
	srv.handleRegister(share)

	// help 端发送的是 normalizeCode 后的 code（client 在 NewHelpMode 里就 normalize 了）。
	help := &ClientConn{ID: "help-1", Conn: &mockConn{}, Send: make(chan []byte, 10)}
	if _, err := srv.sessions.JoinSession(normalizeCode(proto.NoAuthCode), help); err != nil {
		t.Fatalf("no-auth join must succeed with normalized code %q, got: %v", normalizeCode(proto.NoAuthCode), err)
	}

	// 同时确认 NoAuthCode 本身就是 normalize 安全的（不含连字符），
	// 否则 share(原值派生) 与 help(normalize 值派生) 的会话密钥会不一致。
	if normalizeCode(proto.NoAuthCode) != proto.NoAuthCode {
		t.Fatalf("NoAuthCode %q must be normalize-safe (no hyphen/space/underscore)", proto.NoAuthCode)
	}
}
