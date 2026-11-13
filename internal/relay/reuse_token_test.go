package relay

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// 固定协助码防冒领：持有证明（reuse token）相关用例。

// stickyRegisterTok 在 stickyRegister 基础上多带一个复用令牌。
func stickyRegisterTok(t *testing.T, srv *Server, id, clientID, requestCode, token string) (*ClientConn, *proto.RegisterResponse) {
	t.Helper()
	conn := &reuseProtocolConn{remoteAddr: "10.1.0.1:1001"}
	c := &ClientConn{ID: id, ClientID: clientID, Conn: conn, requestCode: requestCode, reuseToken: token, supportsStickyCode: true}
	if srv.handleRegister(c) {
		t.Fatalf("注册 %s 被要求关闭连接", id)
	}
	msgs := conn.messages(t)
	if len(msgs) == 0 || msgs[0].Type != proto.MsgRegisterResponse {
		t.Fatalf("注册响应缺失: %v", msgs)
	}
	var resp proto.RegisterResponse
	if err := proto.DecodePayload(&msgs[0], &resp); err != nil {
		t.Fatal(err)
	}
	return c, &resp
}

// expireAndCleanup 让 share 掉线、超过宽限并回收会话（模拟「真 share 离线超过宽限期」）。
func expireAndCleanup(t *testing.T, srv *Server, shareID string, grace time.Duration) {
	t.Helper()
	srv.sessions.DisconnectClient(shareID)
	time.Sleep(grace + 40*time.Millisecond)
	if n := len(srv.sessions.CleanupExpired()); n != 1 {
		t.Fatalf("应回收 1 个会话，得 %d", n)
	}
}

func sessionOf(t *testing.T, srv *Server, code string) *TunnelSession {
	t.Helper()
	srv.sessions.mu.RLock()
	defer srv.sessions.mu.RUnlock()
	s := srv.sessions.byCode[code]
	if s == nil {
		t.Fatalf("码 %s 没有会话", code)
	}
	return s
}

func TestReuseTokenIssuedOnRegister(t *testing.T) {
	srv := newStickyServer(t, Config{})
	_, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	if resp.ReuseToken == "" {
		t.Fatal("sticky relay 应在注册响应里下发 reuse_token")
	}
	if strings.Contains(resp.ReuseToken, resp.Code) {
		t.Fatal("令牌不得直接包含协助码")
	}
	// no-auth / legacy 不下发
	if _, r := stickyRegister(t, newStickyServer(t, Config{NoAuth: true}), "s", "c", "", true); r.ReuseToken != "" {
		t.Fatal("no-auth 不应下发令牌")
	}
	if _, r := stickyRegister(t, newStickyServer(t, Config{LegacyCodeExpiry: true}), "s", "c", "", true); r.ReuseToken != "" {
		t.Fatal("legacy 模式不应下发令牌")
	}
}

// 核心场景：真 share 离线超宽限被回收后，攻击者带泄露的码抢注册必须被拒（拿到的是新码）。
func TestAttackerCannotClaimLeakedCodeAfterGrace(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 50 * time.Millisecond})
	victim, resp := stickyRegisterTok(t, srv, "victim", "cid-victim", "", "")
	expireAndCleanup(t, srv, victim.ID, 50*time.Millisecond)

	// 攻击者：知道码，没有令牌（也不知道 ClientID），各种变体
	for i, tok := range []string{"", "garbage", resp.ReuseToken /* 令牌是别人的：ClientID 不符 */} {
		atk, aresp := stickyRegisterTok(t, srv, "atk"+string(rune('a'+i)), "cid-attacker", resp.Code, tok)
		if aresp.Code == resp.Code {
			t.Fatalf("变体 %d：攻击者不得占住泄露的码", i)
		}
		srv.sessions.DisconnectClient(atk.ID)
	}
	if _, err := srv.sessions.GetSessionByCode(resp.Code); err == nil {
		t.Fatal("攻击者注册后泄露的码不应已绑定到任何会话")
	}

	// 原持有者携令牌回来：拿回原码，help 连到的是原持有者
	v2, vresp := stickyRegisterTok(t, srv, "victim2", "cid-victim", resp.Code, resp.ReuseToken)
	if vresp.Code != resp.Code {
		t.Fatalf("原持有者应复用原码: %s vs %s", resp.Code, vresp.Code)
	}
	h, ok := stickyJoin(srv, "help", resp.Code)
	if !ok {
		t.Fatal("help 应可加入")
	}
	if got := sessionOf(t, srv, resp.Code); got.Share == nil || got.Share.ID != v2.ID || got.Help == nil || got.Help.ID != h.ID {
		t.Fatalf("会话 share 应是原持有者，得 %+v", got.Share)
	}
}

// relay 状态为空（重启，默认内存密钥）：任何人（含原持有者）都不能凭旧令牌复用，一律新码。
func TestRelayRestartWithoutStateDirDeniesReuse(t *testing.T) {
	srv1 := newStickyServer(t, Config{})
	_, resp := stickyRegisterTok(t, srv1, "s1", "cid-1", "", "")

	srv2 := newStickyServer(t, Config{}) // 全新 relay，随机内存密钥
	_, r2 := stickyRegisterTok(t, srv2, "s2", "cid-1", resp.Code, resp.ReuseToken)
	if r2.Code == resp.Code {
		t.Fatal("默认（无 state-dir）relay 重启后不得复用旧码")
	}
	_, r3 := stickyRegisterTok(t, srv2, "evil", "cid-evil", resp.Code, "")
	if r3.Code == resp.Code {
		t.Fatal("relay 状态为空时攻击者不得抢占泄露的码")
	}
}

// 配置 state-dir 后密钥落盘：重启后原持有者复用成功，CreatedAt 保持，攻击者仍被拒。
func TestRelayRestartWithStateDirKeepsCodeForHolderOnly(t *testing.T) {
	dir := t.TempDir()
	srv1 := newStickyServer(t, Config{StateDir: dir})
	_, resp := stickyRegisterTok(t, srv1, "s1", "cid-1", "", "")
	created := sessionOf(t, srv1, resp.Code).CreatedAt

	if info, err := os.Stat(filepath.Join(dir, reuseSecretFile)); err != nil {
		t.Fatalf("密钥文件未落盘: %v", err)
	} else if info.Size() != reuseSecretLen {
		t.Fatalf("密钥长度应为 %d，得 %d", reuseSecretLen, info.Size())
	}

	time.Sleep(20 * time.Millisecond)
	srv2 := newStickyServer(t, Config{StateDir: dir})
	_, evil := stickyRegisterTok(t, srv2, "evil", "cid-evil", resp.Code, "")
	if evil.Code == resp.Code {
		t.Fatal("攻击者不得抢占")
	}
	_, forged := stickyRegisterTok(t, srv2, "evil2", "cid-evil", resp.Code, resp.ReuseToken)
	if forged.Code == resp.Code {
		t.Fatal("把别人的令牌配自己的 ClientID 不得通过")
	}
	_, r2 := stickyRegisterTok(t, srv2, "s2", "cid-1", resp.Code, resp.ReuseToken)
	if r2.Code != resp.Code {
		t.Fatalf("落盘密钥后原持有者应复用原码: %s vs %s", resp.Code, r2.Code)
	}
	if got := sessionOf(t, srv2, resp.Code).CreatedAt; got.UnixNano() != created.UnixNano() {
		t.Fatalf("复用不得重置 CreatedAt: %v vs %v", created, got)
	}
	if r2.ReuseToken != resp.ReuseToken {
		t.Fatal("同 code/clientID/createdAt 应得到同一令牌")
	}
}

func TestStateDirCorruptSecretRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, reuseSecretFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(&Config{StateDir: dir, CodeLength: 12}); err == nil {
		t.Fatal("密钥文件损坏应让 NewServer 明确报错，而不是静默降级")
	}
}

// 令牌伪造/篡改/换绑：全部被拒。
func TestReuseTokenForgeryRejected(t *testing.T) {
	signer, err := newReuseSigner("")
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(1_700_000_000, 123)
	good := signer.issue("ABCDEFGHJKMN", "cid-1", created)
	if got, ok := signer.verify(good, "ABCDEFGHJKMN", "cid-1"); !ok || got.UnixNano() != created.UnixNano() {
		t.Fatalf("正常令牌应通过且还原 createdAt: %v %v", got, ok)
	}

	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(good, "v1."))
	flip := func(i int) string {
		b := append([]byte(nil), raw...)
		b[i] ^= 0x01
		return "v1." + base64.RawURLEncoding.EncodeToString(b)
	}
	other, _ := newReuseSigner("")
	cases := map[string]struct{ tok, code, cid string }{
		"换码":       {good, "ZZZZZZZZZZZZ", "cid-1"},
		"换 ClientID": {good, "ABCDEFGHJKMN", "cid-2"},
		"空 ClientID": {good, "ABCDEFGHJKMN", ""},
		"改时间戳":      {flip(0), "ABCDEFGHJKMN", "cid-1"},
		"改 MAC":     {flip(len(raw) - 1), "ABCDEFGHJKMN", "cid-1"},
		"截断":        {good[:len(good)-4], "ABCDEFGHJKMN", "cid-1"},
		"空令牌":       {"", "ABCDEFGHJKMN", "cid-1"},
		"版本前缀":      {"v2." + strings.TrimPrefix(good, "v1."), "ABCDEFGHJKMN", "cid-1"},
		"超长":        {"v1." + strings.Repeat("A", 4096), "ABCDEFGHJKMN", "cid-1"},
		"别的密钥签发":    {other.issue("ABCDEFGHJKMN", "cid-1", created), "ABCDEFGHJKMN", "cid-1"},
		"非 base64":   {"v1.!!!!", "ABCDEFGHJKMN", "cid-1"},
	}
	for name, c := range cases {
		if _, ok := signer.verify(c.tok, c.code, c.cid); ok {
			t.Errorf("%s：应被拒绝", name)
		}
	}
	// ClientID 为空的会话不签发令牌
	if tok := signer.issue("ABCDEFGHJKMN", "", created); tok != "" {
		t.Error("空 ClientID 不应签发令牌")
	}
}

// 复用不重置 CreatedAt，max-age 计时连续；到期后原持有者也不能续。
func TestReuseKeepsCreatedAtAndMaxAgeNotExtended(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 40 * time.Millisecond, CodeMaxAge: 400 * time.Millisecond})
	s1, resp := stickyRegisterTok(t, srv, "s1", "cid-1", "", "")
	created := sessionOf(t, srv, resp.Code).CreatedAt
	t0 := time.Now()

	expireAndCleanup(t, srv, s1.ID, 40*time.Millisecond)
	s2, r2 := stickyRegisterTok(t, srv, "s2", "cid-1", resp.Code, resp.ReuseToken)
	if r2.Code != resp.Code {
		t.Fatal("寿命内应复用成功")
	}
	if got := sessionOf(t, srv, resp.Code).CreatedAt; got.UnixNano() != created.UnixNano() {
		t.Fatalf("复用后 CreatedAt 被重置: %v vs %v", created, got)
	}
	if want := created.Add(400 * time.Millisecond).Unix(); r2.ExpiresAt != want {
		t.Fatalf("下发到期应仍是原始签发起算: %d vs %d", r2.ExpiresAt, want)
	}

	// 睡过原始签发 + max-age：虽然是「复用」的，share 在线也应到期，help 不能再加入。
	time.Sleep(time.Until(t0.Add(380 * time.Millisecond)))
	time.Sleep(60 * time.Millisecond)
	if _, ok := stickyJoin(srv, "h", resp.Code); ok {
		t.Fatal("复用不得延长 max-age：超过原始寿命后码必须失效")
	}
	srv.sessions.CleanupExpired()

	// 到期后原持有者不能续：回收后再带令牌，只能拿新码
	srv.sessions.DisconnectClient(s2.ID)
	time.Sleep(60 * time.Millisecond)
	srv.sessions.CleanupExpired()
	_, r3 := stickyRegisterTok(t, srv, "s3", "cid-1", resp.Code, resp.ReuseToken)
	if r3.Code == resp.Code {
		t.Fatal("超过 max-age 后原持有者也不能凭旧令牌续用旧码")
	}
	if r3.ReuseToken == resp.ReuseToken || r3.ReuseToken == "" {
		t.Fatal("新码应有新令牌")
	}
}

// 旧 share：发 request_code 但不带令牌（以及不声明 sticky）——不复用别人空出来的码。
func TestLegacyShareRequestCodeWithoutTokenGetsNewCode(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 50 * time.Millisecond})
	s1, resp := stickyRegister(t, srv, "s1", "cid-1", "", true)
	expireAndCleanup(t, srv, s1.ID, 50*time.Millisecond)
	_, r2 := stickyRegister(t, srv, "s2", "cid-1", resp.Code, true)
	if r2.Code == resp.Code {
		t.Fatal("无令牌的 request_code 不得复用")
	}
	// 宽限内的 ClientID 复用（旧路径）不受影响
	s3, r3 := stickyRegister(t, srv, "s3", "cid-3", "", false)
	srv.sessions.DisconnectClient(s3.ID)
	_, r4 := stickyRegister(t, srv, "s4", "cid-3", "", false)
	if r4.Code != r3.Code {
		t.Fatal("ClientID 复用（宽限内）应保持同码")
	}
}

// 并发抢注册：原持有者与一批攻击者同时带泄露的码注册，只有持有者拿到该码。
func TestConcurrentClaimOnlyHolderWins(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 50 * time.Millisecond, DisableSourceIPLimits: true})
	s1, resp := stickyRegisterTok(t, srv, "s1", "cid-victim", "", "")
	expireAndCleanup(t, srv, s1.ID, 50*time.Millisecond)

	const attackers = 16
	var wg sync.WaitGroup
	got := make([]string, attackers+1)
	start := make(chan struct{})
	for i := 0; i < attackers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			conn := &reuseProtocolConn{remoteAddr: "10.2.0.1:1"}
			c := &ClientConn{ID: "atk" + string(rune('A'+i)), ClientID: "cid-atk-" + string(rune('A'+i)), Conn: conn,
				requestCode: resp.Code, reuseToken: resp.ReuseToken, supportsStickyCode: true}
			srv.handleRegister(c)
			got[i] = firstCode(t, conn)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		conn := &reuseProtocolConn{remoteAddr: "10.3.0.1:1"}
		c := &ClientConn{ID: "victim", ClientID: "cid-victim", Conn: conn, requestCode: resp.Code, reuseToken: resp.ReuseToken, supportsStickyCode: true}
		srv.handleRegister(c)
		got[attackers] = firstCode(t, conn)
	}()
	close(start)
	wg.Wait()
	holders := 0
	for _, c := range got {
		if c == "" {
			t.Fatal("有注册未得到响应（被限流？），并发用例失去意义")
		}
		if c == resp.Code {
			holders++
		}
	}
	if holders != 1 || got[attackers] != resp.Code {
		t.Fatalf("只有原持有者能拿到该码：holders=%d victim=%s", holders, got[attackers])
	}
}

func firstCode(t *testing.T, conn *reuseProtocolConn) string {
	msgs := conn.messages(t)
	if len(msgs) == 0 || msgs[0].Type != proto.MsgRegisterResponse {
		return ""
	}
	var r proto.RegisterResponse
	_ = proto.DecodePayload(&msgs[0], &r)
	return r.Code
}

// 保护不放宽：复用后仍只允许一个控制端。
func TestReusedCodeStillSingleHelper(t *testing.T) {
	srv := newStickyServer(t, Config{CodeTTL: 50 * time.Millisecond})
	s1, resp := stickyRegisterTok(t, srv, "s1", "cid-1", "", "")
	expireAndCleanup(t, srv, s1.ID, 50*time.Millisecond)
	_, r2 := stickyRegisterTok(t, srv, "s2", "cid-1", resp.Code, resp.ReuseToken)
	if r2.Code != resp.Code {
		t.Fatal("应复用")
	}
	if _, ok := stickyJoin(srv, "h1", resp.Code); !ok {
		t.Fatal("第一个控制端应可加入")
	}
	if _, ok := stickyJoin(srv, "h2", resp.Code); ok {
		t.Fatal("第二个控制端必须被拒")
	}
}
