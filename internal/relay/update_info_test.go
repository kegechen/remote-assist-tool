package relay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
)

// captureConn 记录 relay 写给客户端的每一帧（换行分隔 JSON）。
type captureConn struct {
	addr string
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (c *captureConn) Read([]byte) (int, error) { return 0, nil }
func (c *captureConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}
func (c *captureConn) Close() error       { return nil }
func (c *captureConn) RemoteAddr() string { return c.addr }

// frame 取第一条指定类型的帧，返回原始 payload。
func (c *captureConn) frame(t *testing.T, typ proto.MessageType) json.RawMessage {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	sc := bufio.NewScanner(bytes.NewReader(c.buf.Bytes()))
	for sc.Scan() {
		var m proto.Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad frame %q: %v", sc.Text(), err)
		}
		if m.Type == typ {
			return m.Payload
		}
	}
	t.Fatalf("no %s frame in %q", typ, c.buf.String())
	return nil
}

func registerAndJoin(t *testing.T, cfg *Config) (shareConn, helpConn *captureConn) {
	t.Helper()
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	shareConn = &captureConn{addr: "203.0.113.7:1"}
	share := &ClientConn{ID: "share1", Conn: shareConn}
	reg, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{Version: "0.0.7", Host: "u@pc Windows 10 amd64"})
	srv.handleMessage(share, reg)

	var code string
	for c := range srv.sessions.byCode {
		code = c
	}
	helpConn = &captureConn{addr: "198.51.100.9:2"}
	help := &ClientConn{ID: "help1", Conn: helpConn}
	join, _ := proto.NewMessage(proto.MsgJoinRequest, &proto.JoinRequest{Code: code, Version: "v1.1.0", SupportsUpdateNotice: true})
	srv.handleMessage(help, join)
	return shareConn, helpConn
}

// 配置了最新版本时，注册与加入响应都带上 UpdateInfo。
func TestResponsesCarryUpdateInfo(t *testing.T) {
	want := proto.UpdateInfo{
		LatestVersion: "v1.1.0",
		DownloadURL:   "https://example.test/cli-{os}-{arch}{ext}",
		ReleasePage:   "https://example.test/latest",
	}
	shareConn, helpConn := registerAndJoin(t, &Config{ListenAddr: "127.0.0.1:0", Update: want})

	var reg proto.RegisterResponse
	if err := json.Unmarshal(shareConn.frame(t, proto.MsgRegisterResponse), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.UpdateInfo != want {
		t.Errorf("RegisterResponse.UpdateInfo = %+v, want %+v", reg.UpdateInfo, want)
	}

	var join proto.JoinResponse
	if err := json.Unmarshal(helpConn.frame(t, proto.MsgJoinResponse), &join); err != nil {
		t.Fatal(err)
	}
	if !join.Success || join.UpdateInfo != want {
		t.Errorf("JoinResponse = %+v, want success with %+v", join, want)
	}
	if join.PeerHost != "u@pc Windows 10 amd64" || join.PeerVersion != "0.0.7" {
		t.Errorf("peer fields lost: %+v", join)
	}
}

func TestResponsesNormalizeBuiltInReleaseTag(t *testing.T) {
	shareConn, helpConn := registerAndJoin(t, &Config{ListenAddr: "127.0.0.1:0", Update: proto.UpdateInfo{
		LatestVersion: "1.1.0", DownloadURL: "https://github.com/kegechen/remote-assist-tool/releases/download/{version}/remote-assist-cli-{os}-{arch}{ext}", ReleasePage: "https://github.com/kegechen/remote-assist-tool/releases/tag/{version}",
	}})
	var reg proto.RegisterResponse
	if err := json.Unmarshal(shareConn.frame(t, proto.MsgRegisterResponse), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.LatestVersion != "v1.1.0" {
		t.Fatalf("built-in release tag was not normalized: %+v", reg.UpdateInfo)
	}
	var join proto.JoinResponse
	if err := json.Unmarshal(helpConn.frame(t, proto.MsgJoinResponse), &join); err != nil {
		t.Fatal(err)
	}
	if join.LatestVersion != "v1.1.0" {
		t.Fatalf("join response release tag was not normalized: %+v", join.UpdateInfo)
	}
}

func TestNormalizeLiteralDownloadWithDefaultReleasePage(t *testing.T) {
	info := normalizeUpdateInfo(proto.UpdateInfo{
		LatestVersion: "1.1.0",
		DownloadURL:   "https://github.com/kegechen/remote-assist-tool/releases/download/v1.1.0/remote-assist-cli-{os}-{arch}{ext}",
		ReleasePage:   update.DefaultReleasePage,
	})
	if info.LatestVersion != "v1.1.0" {
		t.Fatalf("mixed literal/default announcement was not normalized: %+v", info)
	}
}

// 未配置时线上不出现这些字段，响应与旧 relay 字节级一致。
func TestResponsesOmitUpdateInfoWhenUnset(t *testing.T) {
	shareConn, helpConn := registerAndJoin(t, &Config{ListenAddr: "127.0.0.1:0"})
	for _, raw := range []json.RawMessage{
		shareConn.frame(t, proto.MsgRegisterResponse),
		helpConn.frame(t, proto.MsgJoinResponse),
	} {
		for _, k := range []string{"latest_version", "download_url", "release_page"} {
			if strings.Contains(string(raw), k) {
				t.Errorf("unset UpdateInfo leaked %q into %s", k, raw)
			}
		}
	}
}

// 旧客户端（0.0.7）的响应结构不认识新字段：解码必须成功且原有字段不受影响。
func TestOldClientDecodesResponseWithUpdateInfo(t *testing.T) {
	shareConn, helpConn := registerAndJoin(t, &Config{ListenAddr: "127.0.0.1:0", Update: proto.UpdateInfo{LatestVersion: "v1.1.0", DownloadURL: "x"}})

	var oldReg struct {
		Code      string `json:"code"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(shareConn.frame(t, proto.MsgRegisterResponse), &oldReg); err != nil || oldReg.Code == "" || oldReg.ExpiresAt == 0 {
		t.Errorf("old RegisterResponse decode: %+v err=%v", oldReg, err)
	}
	var oldJoin struct {
		Success   bool   `json:"success"`
		SessionID string `json:"session_id,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(helpConn.frame(t, proto.MsgJoinResponse), &oldJoin); err != nil || !oldJoin.Success || oldJoin.SessionID == "" {
		t.Errorf("old JoinResponse decode: %+v err=%v", oldJoin, err)
	}
}
