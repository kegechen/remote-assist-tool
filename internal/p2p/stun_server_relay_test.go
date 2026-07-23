package p2p

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/ratelimit"
)

// buildRelayDatagram 构造一个 sid 长 sidLen、payload 长 payloadLen 的合法 relay 数据报。
func buildRelayDatagram(sidLen, payloadLen int) []byte {
	sid := make([]byte, sidLen)
	for i := 0; i < sidLen; i++ {
		sid[i] = byte('a' + i%26)
	}
	return buildRelayDatagramForID(string(sid), payloadLen)
}

func buildRelayDatagramForID(sessionID string, payloadLen int) []byte {
	buf := make([]byte, 2+len(sessionID)+payloadLen)
	buf[0] = relayMarker
	buf[1] = byte(len(sessionID))
	copy(buf[2:], sessionID)
	for i := 0; i < payloadLen; i++ {
		buf[2+len(sessionID)+i] = byte(i % 256)
	}
	return buf
}

func TestParseRelayHeaderSidLenBounds(t *testing.T) {
	// sidLen==0 拒绝
	if _, _, ok := parseRelayHeader([]byte{relayMarker, 0, 'x'}); ok {
		t.Fatal("sidLen==0 应被拒绝")
	}
	// sidLen>maxSessionIDLen 拒绝
	over := make([]byte, 2+maxSessionIDLen+1+1)
	over[0] = relayMarker
	over[1] = byte(maxSessionIDLen + 1)
	if _, _, ok := parseRelayHeader(over); ok {
		t.Fatal("sidLen>maxSessionIDLen 应被拒绝")
	}
	// header 声明长度超过实际数据 拒绝
	if _, _, ok := parseRelayHeader([]byte{relayMarker, 10, 'a', 'b'}); ok {
		t.Fatal("header 声明长度超过实际数据 应被拒绝")
	}
	// 合法：sidLen=maxSessionIDLen + 1 字节 payload
	d := buildRelayDatagram(maxSessionIDLen, 1)
	if _, payload, ok := parseRelayHeader(d); !ok || len(payload) != 1 {
		t.Fatalf("合法数据报解析失败 ok=%v payloadLen=%d", ok, len(payload))
	}
}

// startTestSTUN 启动一个监听随机端口的 STUNServer，返回它与其地址。
func startTestSTUN(t *testing.T) (*STUNServer, *net.UDPAddr) {
	t.Helper()
	s, err := NewSTUNServerWithValidator("127.0.0.1:0", func(string) bool { return true })
	if err != nil {
		t.Fatalf("NewSTUNServer: %v", err)
	}
	return s, s.LocalAddr().(*net.UDPAddr)
}

func TestRelayRequiresValidator(t *testing.T) {
	s, err := NewSTUNServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.handleRelayPacket(buildRelayDatagram(8, 1), &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 10001})
	if got := s.relaySessionCount(); got != 0 {
		t.Fatalf("nil validator 不得启用 relay，实际状态数=%d", got)
	}
}

func TestRelayValidatorRecheckedBeforeCreate(t *testing.T) {
	firstCheck := make(chan struct{})
	releaseFirst := make(chan struct{})
	active := true
	var mu sync.Mutex
	calls := 0
	validator := func(string) bool {
		mu.Lock()
		calls++
		call := calls
		value := active
		mu.Unlock()
		if call == 1 {
			close(firstCheck)
			<-releaseFirst
			return true // 模拟断连前已经取得的陈旧结果
		}
		return value
	}
	s, err := NewSTUNServerWithValidator("127.0.0.1:0", validator)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	done := make(chan struct{})
	go func() {
		s.handleRelayPacket(buildRelayDatagram(8, 1), &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 10002})
		close(done)
	}()
	<-firstCheck
	mu.Lock()
	active = false
	mu.Unlock()
	close(releaseFirst)
	<-done
	if got := s.relaySessionCount(); got != 0 {
		t.Fatalf("锁内复检为 false 后不得创建 relay 状态，实际=%d", got)
	}
}

func TestRelayStateLimitAndInvalidateRelease(t *testing.T) {
	s, err := NewSTUNServerWithValidator("127.0.0.1:0", func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 11000}

	for i := 0; i < maxRelaySessionsPerIP; i++ {
		sid := fmt.Sprintf("sid-%04d", i)
		s.handleRelayPacket(buildRelayDatagramForID(sid, 1), addr)
	}
	if got := s.relaySessionCount(); got != maxRelaySessionsPerIP {
		t.Fatalf("relay per-IP 状态数=%d，期望=%d", got, maxRelaySessionsPerIP)
	}
	sid := "sid-overflow"
	s.handleRelayPacket(buildRelayDatagramForID(sid, 1), addr)
	if got := s.relaySessionCount(); got != maxRelaySessionsPerIP {
		t.Fatalf("超限后状态数不应增长，实际=%d", got)
	}

	s.InvalidateRelaySession("sid-0000")
	s.handleRelayPacket(buildRelayDatagramForID(sid, 1), addr)
	if got := s.relaySessionCount(); got != maxRelaySessionsPerIP {
		t.Fatalf("释放一个配额后应允许新状态，实际=%d", got)
	}
}

func TestRelayPerSessionLimitDoesNotConsumeGlobalBudget(t *testing.T) {
	s, err := NewSTUNServerWithValidator("127.0.0.1:0", func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.relayByteGlobal = ratelimit.NewBucket(0, 1)

	hotA := listenTestUDP(t)
	defer hotA.Close()
	hotB := listenTestUDP(t)
	defer hotB.Close()
	fairA := listenTestUDP(t)
	defer fairA.Close()
	fairB := listenTestUDP(t)
	defer fairB.Close()

	s.handleRelayPacket(buildRelayDatagramForID("hot", 1), hotA.LocalAddr().(*net.UDPAddr))
	s.relayMu.Lock()
	s.relaySessions["hot"].byteLimiter = ratelimit.NewBucket(0, 0)
	s.relayMu.Unlock()
	s.handleRelayPacket(buildRelayDatagramForID("hot", 1), hotB.LocalAddr().(*net.UDPAddr))

	// hot 被单会话桶拒绝后，全局唯一令牌必须仍可供另一个会话使用。
	s.handleRelayPacket(buildRelayDatagramForID("fair", 1), fairA.LocalAddr().(*net.UDPAddr))
	s.handleRelayPacket(buildRelayDatagramForID("fair", 1), fairB.LocalAddr().(*net.UDPAddr))
	if err := fairA.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if n, _, err := fairA.ReadFromUDP(buf); err != nil || n != 1 {
		t.Fatalf("公平会话应使用未被热会话消耗的全局额度: n=%d err=%v", n, err)
	}
}

func TestRelayInvalidateAndCloseReleaseCountsIdempotently(t *testing.T) {
	s, err := NewSTUNServerWithValidator("127.0.0.1:0", func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12000}
	s.handleRelayPacket(buildRelayDatagramForID("counted", 1), addr)
	s.InvalidateRelaySession("counted")
	s.InvalidateRelaySession("counted")
	s.relayMu.Lock()
	if len(s.relaySessions) != 0 || len(s.relayCountPerIP) != 0 {
		t.Fatalf("重复失效后状态未清空: sessions=%d perIP=%v", len(s.relaySessions), s.relayCountPerIP)
	}
	s.relayMu.Unlock()

	s.handleRelayPacket(buildRelayDatagramForID("counted-again", 1), addr)
	s.Close()
	s.Close()
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	if len(s.relaySessions) != 0 || len(s.relayCountPerIP) != 0 {
		t.Fatalf("Close 后状态未清空: sessions=%d perIP=%v", len(s.relaySessions), s.relayCountPerIP)
	}
}

func listenTestUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestSTUNCloseWhileReceiving(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, addr := startTestSTUN(t)
		sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			pkt := buildRelayDatagram(8, 1)
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = sender.WriteToUDP(pkt, addr)
				}
			}
		}()
		time.Sleep(time.Millisecond)
		s.Close()
		close(stop)
		wg.Wait()
		sender.Close()
		// Close 必须幂等且等待自有 goroutine 退出。
		s.Close()
	}
}

func (s *STUNServer) relaySessionCount() int {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	return len(s.relaySessions)
}

func TestRelayDatagramAtLimitAccepted(t *testing.T) {
	s, srvAddr := startTestSTUN(t)
	defer s.Close()

	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	// 恰好 maxRelayDatagramSize：sid=maxSessionIDLen, payload=maxUDPPayloadSize
	pkt := buildRelayDatagram(maxSessionIDLen, maxUDPPayloadSize)
	if len(pkt) != maxRelayDatagramSize {
		t.Fatalf("测试数据报尺寸=%d 期望 %d", len(pkt), maxRelayDatagramSize)
	}
	if _, err := sender.WriteToUDP(pkt, srvAddr); err != nil {
		t.Fatal(err)
	}

	// 轮询等待 relay session 建立
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.relaySessionCount() == 1 {
			return // 上限包被接受并创建了 relay 状态
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("恰好上限的合法包未被接受，relaySessionCount=%d", s.relaySessionCount())
}

func TestRelayDatagramOverLimitRejected(t *testing.T) {
	s, srvAddr := startTestSTUN(t)
	defer s.Close()

	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	// maxRelayDatagramSize+1：应被丢弃，不入队、不创建 relay 状态
	pkt := buildRelayDatagram(maxSessionIDLen, maxUDPPayloadSize+1)
	if len(pkt) != maxRelayDatagramSize+1 {
		t.Fatalf("测试数据报尺寸=%d 期望 %d", len(pkt), maxRelayDatagramSize+1)
	}
	if _, err := sender.WriteToUDP(pkt, srvAddr); err != nil {
		t.Fatal(err)
	}

	// 给足处理时间后断言未创建任何 relay 状态
	time.Sleep(300 * time.Millisecond)
	if c := s.relaySessionCount(); c != 0 {
		t.Fatalf("超长包不应创建 relay 状态，relaySessionCount=%d", c)
	}
}
