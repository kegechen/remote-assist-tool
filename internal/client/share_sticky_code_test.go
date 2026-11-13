package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/relay"
)

// 协助码在 share 进程存活期间固定：share 重连层用例，真实 relay（明文 TCP，本机回环）+ 真实 ShareMode。

func stubClipboard(t *testing.T) {
	t.Helper()
	orig := clipboardCopy
	clipboardCopy = func(string) error { return nil }
	t.Cleanup(func() { clipboardCopy = orig })
}

// startPlainRelayAt 在指定地址起明文 relay（addr 为空时挑一个空闲端口），返回地址与停止函数。
func startPlainRelayAt(t *testing.T, addr string, cfg relay.Config) (string, func()) {
	t.Helper()
	if addr == "" {
		addr = freePort(t)
	}
	cfg.ListenAddr = addr
	if cfg.CodeTTL == 0 {
		cfg.CodeTTL = time.Minute
	}
	cfg.CodeLength = 10
	srv, err := relay.NewServer(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- srv.StartWithContextReady(ctx, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("relay 启动失败: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("relay 启动超时")
	}
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	t.Cleanup(stop)
	return addr, stop
}

func newTestShare(addr, codeFile string) *ShareMode {
	cfg := &Config{ServerAddr: addr, UseTLS: false, Transport: "tcp"}
	return NewShareMode(cfg, "127.0.0.1:22", true, agent.SandboxConfig{}, codeFile, "")
}

type codeFileJSON struct {
	Code      string `json:"code"`
	Server    string `json:"server"`
	ExpiresAt int64  `json:"expiresAt"`
}

func readCodeFile(t *testing.T, path string) (codeFileJSON, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v codeFileJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v, string(raw)
}

// share 反复与 relay 断开重连（relay 内存态保留），码与 code.json 内容全程不变。
func TestShareReconnectKeepsCodeAndCodeFile(t *testing.T) {
	stubClipboard(t)
	addr, _ := startPlainRelayAt(t, "", relay.Config{})
	codeFile := filepath.Join(t.TempDir(), "code.json")
	s := newTestShare(addr, codeFile)
	defer s.client.Close()

	s.reconnectWithBackoff()
	code := s.GetCode()
	if code == "" {
		t.Fatal("首次注册应得到码")
	}
	if !s.GetExpiresAt().IsZero() {
		t.Fatalf("sticky relay 下应为无固定到期，得 %v", s.GetExpiresAt())
	}
	_, firstRaw := readCodeFile(t, codeFile)
	for i := 0; i < 5; i++ {
		s.client.Close() // 模拟网络抖动：连接断开
		time.Sleep(30 * time.Millisecond)
		s.reconnectWithBackoff()
		if s.GetCode() != code {
			t.Fatalf("第 %d 次重连码变了: %s -> %s", i+1, code, s.GetCode())
		}
	}
	if _, raw := readCodeFile(t, codeFile); raw != firstRaw {
		t.Fatalf("code.json 内容应全程不变:\n%s\n%s", firstRaw, raw)
	}
}

// relay 重启后（配置了 --state-dir，密钥落盘）share 重连，凭复用令牌拿回同一个码；过了原 TTL 也一样。
func TestShareReconnectAfterRelayRestartKeepsCode(t *testing.T) {
	stubClipboard(t)
	state := t.TempDir()
	addr, stop := startPlainRelayAt(t, "", relay.Config{CodeTTL: 30 * time.Millisecond, StateDir: state})
	s := newTestShare(addr, "")
	defer s.client.Close()
	s.reconnectWithBackoff()
	code := s.GetCode()
	if s.reuseToken == "" {
		t.Fatal("新 relay 应下发复用令牌，share 应缓存")
	}

	time.Sleep(100 * time.Millisecond) // 超过 TTL
	stop()
	time.Sleep(100 * time.Millisecond)
	startPlainRelayAt(t, addr, relay.Config{CodeTTL: 30 * time.Millisecond, StateDir: state})
	s.reconnectWithBackoff()
	if s.GetCode() != code {
		t.Fatalf("relay 重启（落盘密钥）后码应保持: %s -> %s", code, s.GetCode())
	}
}

// 默认（无 --state-dir）relay 重启后，原持有者也不能凭旧令牌复用：换新码，且提示讲清原因。
func TestShareReconnectAfterRelayRestartWithoutStateDirGetsNewCode(t *testing.T) {
	stubClipboard(t)
	addr, stop := startPlainRelayAt(t, "", relay.Config{})
	s := newTestShare(addr, "")
	defer s.client.Close()
	s.reconnectWithBackoff()
	code := s.GetCode()
	s.client.Close()
	stop()
	time.Sleep(100 * time.Millisecond)
	startPlainRelayAt(t, addr, relay.Config{})
	s.reconnectWithBackoff()
	if s.GetCode() == code || s.GetCode() == "" {
		t.Fatalf("无落盘密钥的 relay 重启后应换新码: %s -> %s", code, s.GetCode())
	}
	if n := codeChangeNotice(code, s.GetCode(), s.codeSticky); !strings.Contains(n, "--state-dir") {
		t.Fatalf("提示应指明 --state-dir: %q", n)
	}
}

// 进程重启（新建 ShareMode）换新码。
func TestNewShareProcessGetsNewCode(t *testing.T) {
	stubClipboard(t)
	addr, _ := startPlainRelayAt(t, "", relay.Config{})
	s1 := newTestShare(addr, "")
	s1.reconnectWithBackoff()
	code1 := s1.GetCode()
	s1.client.Close()

	s2 := newTestShare(addr, "")
	defer s2.client.Close()
	s2.reconnectWithBackoff()
	if s2.GetCode() == code1 || s2.GetCode() == "" {
		t.Fatalf("新进程应得新码，旧=%s 新=%s", code1, s2.GetCode())
	}
}

// 防冒领：relay 重启后，知道泄露的码的另一个 share（没有复用令牌）抢先注册，拿不到该码；
// 随后原持有者回来仍能拿回原码，协助端连到的是原持有者。
func TestShareThiefCannotClaimCodeAfterRelayRestart(t *testing.T) {
	stubClipboard(t)
	state := t.TempDir()
	addr, stop := startPlainRelayAt(t, "", relay.Config{StateDir: state})
	codeFile := filepath.Join(t.TempDir(), "code.json")
	s := newTestShare(addr, codeFile)
	defer s.client.Close()
	s.reconnectWithBackoff()
	code := s.GetCode()
	s.client.Close()
	stop()
	time.Sleep(100 * time.Millisecond)
	startPlainRelayAt(t, addr, relay.Config{StateDir: state})

	// 攻击者抢先用泄露的码注册（既无令牌，ClientID 也不同）
	thief := newTestShare(addr, "")
	defer thief.client.Close()
	thief.code = code
	thief.reconnectWithBackoff()
	if thief.GetCode() == code || thief.GetCode() == "" {
		t.Fatalf("攻击者不得拿到泄露的码，得 %s", thief.GetCode())
	}

	s.reconnectWithBackoff()
	if s.GetCode() != code {
		t.Fatalf("原持有者应拿回原码，得 %s（原 %s）", s.GetCode(), code)
	}
	if v, _ := readCodeFile(t, codeFile); v.Code != code {
		t.Fatalf("code.json 应仍是原码: %+v", v)
	}
}

// 旧 relay（不理解新字段、不下发 code_sticky；以 LegacyCodeExpiry 模拟其行为）：
// share 仍可工作，relay 丢状态后换码，且有如实提示。
func TestShareAgainstLegacyRelay(t *testing.T) {
	stubClipboard(t)
	addr, stop := startPlainRelayAt(t, "", relay.Config{LegacyCodeExpiry: true})
	s := newTestShare(addr, "")
	defer s.client.Close()
	s.reconnectWithBackoff()
	code := s.GetCode()
	if s.codeSticky || s.GetExpiresAt().IsZero() {
		t.Fatalf("旧 relay 不应被识别为 sticky，且有固定到期时刻: sticky=%v exp=%v", s.codeSticky, s.GetExpiresAt())
	}
	s.client.Close()
	stop()
	time.Sleep(100 * time.Millisecond)
	startPlainRelayAt(t, addr, relay.Config{LegacyCodeExpiry: true})
	s.reconnectWithBackoff()
	if s.GetCode() == code {
		t.Fatal("旧 relay 丢状态后不可能复用旧码（请求码被忽略）")
	}
	if n := codeChangeNotice(code, s.GetCode(), s.codeSticky); !strings.Contains(n, "不支持固定协助码") {
		t.Fatalf("应提示中转服务不支持固定码，得 %q", n)
	}
}

func TestCodeChangeNotice(t *testing.T) {
	if codeChangeNotice("", "ABCD", true) != "" || codeChangeNotice("ABCD", "ABCD", false) != "" {
		t.Fatal("首次注册或码未变不应提示")
	}
	if n := codeChangeNotice("AAAA", "BBBB", false); !strings.Contains(n, "不支持固定协助码") || !strings.Contains(n, "BBBB") {
		t.Fatalf("旧 relay 文案不对: %q", n)
	}
	if n := codeChangeNotice("AAAA", "BBBB", true); !strings.Contains(n, "已被占用") {
		t.Fatalf("码被占文案不对: %q", n)
	}
}

// 无固定到期时，协助信息不应展示 1970 年的时间，code.json 的 expiresAt 为 0。
func TestShareInstructionsAndCodeFileWithoutExpiry(t *testing.T) {
	txt := formatShareInstructions("ABCDEFGHJK", "u@h", &Config{ServerAddr: "x:1"}, time.Time{})
	if !strings.Contains(txt, "本进程存活期间一直有效") || strings.Contains(txt, "1970") || strings.Contains(txt, "0001") {
		t.Fatalf("无到期文案不对:\n%s", txt)
	}
	path := filepath.Join(t.TempDir(), "code.json")
	s := NewShareMode(&Config{ServerAddr: "x:1"}, "127.0.0.1:22", true, agent.SandboxConfig{}, path, "")
	s.code = "ABCDEFGHJK"
	s.writeCodeFile()
	if v, _ := readCodeFile(t, path); v.ExpiresAt != 0 || v.Code != "ABCDEFGHJK" {
		t.Fatalf("无到期时 expiresAt 应为 0: %+v", v)
	}
}
