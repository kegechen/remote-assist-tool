package client

import (
	"net"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

// blackholeListener 起一个「只接受连接、永不应答」的 relay：TCP 三次握手成功，之后一个
// 字节都不回。这正是进程假死 / 被中间盒吞包的现场表现——比连不上更难处理，因为内核层面
// 一切正常，没有任何错误会冒出来。
func blackholeListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, conn) // 攥住不放，也不读不写
		}
	}()
	return ln
}

// shrinkJoinTimeout 把 joinTimeout 调到毫秒级，免得回归测试真等 15 秒。
func shrinkJoinTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := joinTimeout
	joinTimeout = d
	t.Cleanup(func() { joinTimeout = orig })
}

// mustFinishWithin 在超时前拿到 fn 的返回值；超时即判定「永久挂死」。
// 直接在测试 goroutine 里同步调用的话，回归失败的表现是整个 go test 挂到 panic，
// 看不出是哪一条、也拖垮同包其它用例。
func mustFinishWithin(t *testing.T, budget time.Duration, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		t.Fatalf("%s 在 %v 内没有返回：黑洞 relay 会让它永久挂死", what, budget)
		return nil
	}
}

// join 等 JoinResponse 时必须带读超时。
//
// MCP 里这一步在 connectMu 之内（help_bootstrap.doConnect 入口就持锁），一次挂死会让
// 之后所有 connect 排在后面一起卡住，整个 MCP server 只能重启。
func TestHelpJoinTimesOutAgainstBlackholeRelay(t *testing.T) {
	ln := blackholeListener(t)
	shrinkJoinTimeout(t, 200*time.Millisecond)

	h := NewHelpMode(&Config{ServerAddr: ln.Addr().String()}, "ABC123", "127.0.0.1:0")
	if err := h.client.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer h.client.Close()

	err := mustFinishWithin(t, 5*time.Second, "HelpMode.join", func() error {
		_, err := h.join()
		return err
	})
	if err == nil {
		t.Fatal("黑洞 relay 不可能返回 JoinResponse，join 却成功了")
	}
	if !isNetTimeout(err) {
		t.Fatalf("期望超时错误，实际: %v", err)
	}
}

// register 等 RegisterResponse 时同样必须带读超时：share 的 reconnectWithBackoff 靠
// register 返回 error 才进下一轮，卡在这里等于重连循环停摆、协助码再也不刷新。
func TestShareRegisterTimesOutAgainstBlackholeRelay(t *testing.T) {
	ln := blackholeListener(t)
	shrinkJoinTimeout(t, 200*time.Millisecond)

	// newInstance=true：registrationClientID 走进程内随机 ID，测试不去碰用户家目录。
	s := NewShareMode(&Config{ServerAddr: ln.Addr().String()}, "127.0.0.1:0", true, agent.SandboxConfig{}, "", "")
	if err := s.client.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer s.client.Close()

	err := mustFinishWithin(t, 5*time.Second, "ShareMode.register", func() error {
		return s.register()
	})
	if err == nil {
		t.Fatal("黑洞 relay 不可能返回 RegisterResponse，register 却成功了")
	}
	if !isNetTimeout(err) {
		t.Fatalf("期望超时错误，实际: %v", err)
	}
}

// Connect 本身也要带 dial 超时。这里用一个已 Close 的监听端口拿不到「永久挂起」的现场
// （内核会直接 RST），所以只校验 dialer 确实带上了 timeout——防止后续重构悄悄换回裸
// net.Dial / tls.Dial。
func TestDialTimeoutIsConfigured(t *testing.T) {
	if dialTimeout <= 0 {
		t.Fatal("dialTimeout 必须为正：裸 net.Dial 会一路等到内核放弃（Linux 约 130s）")
	}
	if joinTimeout <= 0 {
		t.Fatal("joinTimeout 必须为正")
	}
}
