package relay

// 按设备封禁（设计 2026-11-23 §3、§7 第 5 步）：join 失败按设备计分，设备键优先取 join 里的 client_id，
// 旧 help 没有就用 Host 身份串加来源 IP，两者都没有照旧按 IP。已识别设备的失败每 5 次给来源 IP 记 1 分兜底。
// 被封设备的 join 在最前面拦下、回「访问受限」后断开，不再计分；register 不受影响。

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/ratelimit"
	"github.com/remote-assist/tool/internal/relay/blocklist"
)

const (
	devIP    = "198.51.100.7"
	devCID   = "cid_00112233445566778899aabbccddeeff"
	devCID2  = "cid_ffeeddccbbaa99887766554433221100"
	devHost  = "alice@alice-pc windows amd64"
	devHost2 = "bob@bob-pc linux amd64"
	wrongPIN = "WRONGCODE0"
)

type devEnv struct {
	srv       *Server
	ip, dev   *blocklist.List
	connCount int
}

func newDevEnv(t *testing.T, mutate func(ip, dev *blocklist.Config, cfg *Config)) *devEnv {
	t.Helper()
	ipCfg, devCfg := blocklist.DefaultConfig(), blocklist.DefaultConfig()
	devCfg.Keyer = blocklist.DeviceKeyer
	cfg := &Config{CodeTTL: 5 * time.Minute, CodeLength: 10}
	if mutate != nil {
		mutate(&ipCfg, &devCfg, cfg)
	}
	ipl, err := blocklist.Open(ipCfg)
	if err != nil {
		t.Fatal(err)
	}
	devl, err := blocklist.Open(devCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devl.Close(); _ = ipl.Close() })
	cfg.Blocklist, cfg.DeviceBlocklist = ipl, devl
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 本组用例逐次数计分，不测限流：限流拒绝不计分，会让「第 N 次」对不上。
	srv.joinLimiterPerIP, srv.joinLimiterGlobal = nil, nil
	return &devEnv{srv: srv, ip: ipl, dev: devl}
}

// join 用一条新连接发一次 join：每条连接只失败一次，不触发单连接失败上限（那会另记 5 分）。
// payload 用 map 拼，钉住线上字段名 client_id。
func (e *devEnv) join(t *testing.T, ip, code, clientID, host string) (*proto.JoinResponse, bool) {
	t.Helper()
	e.connCount++
	conn := &reuseProtocolConn{remoteAddr: net.JoinHostPort(ip, "40000")}
	c := &ClientConn{ID: fmt.Sprintf("dev-test-%d", e.connCount), Conn: conn}
	payload := map[string]interface{}{"code": code, "version": "v1.3.0", "supports_canonical_code": true}
	if clientID != "" {
		payload["client_id"] = clientID
	}
	if host != "" {
		payload["host"] = host
	}
	m, err := proto.NewMessage(proto.MsgJoinRequest, payload)
	if err != nil {
		t.Fatal(err)
	}
	closed := e.srv.handleMessage(c, m)
	frames := conn.messages(t)
	if len(frames) == 0 {
		t.Fatal("join 没有响应")
	}
	last := frames[len(frames)-1]
	if last.Type != proto.MsgJoinResponse {
		t.Fatalf("应回 JoinResponse，实际 %s", last.Type)
	}
	var resp proto.JoinResponse
	if err := proto.DecodePayload(&last, &resp); err != nil {
		t.Fatal(err)
	}
	return &resp, closed
}

// failN 用 n 条连接各失败一次。
func (e *devEnv) failN(t *testing.T, n int, ip, clientID, host string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if resp, _ := e.join(t, ip, wrongPIN, clientID, host); resp.Success || resp.Error == proto.BlockedMessage {
			t.Fatalf("第 %d 次错码 join 应得到普通失败，实际 %+v", i+1, resp)
		}
	}
}

// register 注册一个 share，返回协助码。
func (e *devEnv) register(t *testing.T, ip, clientID string) string {
	t.Helper()
	conn := &reuseProtocolConn{remoteAddr: net.JoinHostPort(ip, "50000")}
	m, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{ClientID: clientID, Version: "v1.3.0", Host: "share@share-pc windows amd64"})
	if e.srv.handleMessage(&ClientConn{ID: "share-" + clientID + ip, Conn: conn}, m) {
		t.Fatal("register 不应关闭连接")
	}
	frames := conn.messages(t)
	if len(frames) == 0 || frames[0].Type != proto.MsgRegisterResponse {
		t.Fatalf("register 应成功，实际 %+v", frames)
	}
	var r proto.RegisterResponse
	if err := proto.DecodePayload(&frames[0], &r); err != nil || r.Code == "" {
		t.Fatalf("register 响应不对：%+v %v", r, err)
	}
	return r.Code
}

func (e *devEnv) ipPoints(t *testing.T, ip string) int {
	t.Helper()
	v, ok := e.ip.Detail(ip)
	if !ok {
		return 0
	}
	return v.Points
}

func wantBlocked(t *testing.T, resp *proto.JoinResponse, closed bool, what string) {
	t.Helper()
	if resp.Success || resp.Error != proto.BlockedMessage || !closed {
		t.Fatalf("%s：应回「%s」并断开，实际 success=%v error=%q closed=%v", what, proto.BlockedMessage, resp.Success, resp.Error, closed)
	}
}

// 同一 ClientID 20 次错码：该设备被封，IP 只记兜底分（20/5=4）不被封；同 IP 另一台设备照常加入；
// 被封设备再来，即使码是对的也在最前面拦下，不进会话、不再计分。
func TestDeviceBanSameClientID(t *testing.T) {
	e := newDevEnv(t, nil)
	key := e.dev.DeviceKey(blocklist.DeviceByClientID, devCID)
	e.failN(t, 19, devIP, devCID, devHost)
	if e.dev.IsBanned(key) {
		t.Fatal("19 次不应封设备")
	}
	e.failN(t, 1, devIP, devCID, devHost)
	if !e.dev.IsBanned(key) {
		t.Fatal("同一 ClientID 第 20 次错码应封设备")
	}
	if e.ip.IsBanned(devIP) {
		t.Fatal("设备被封不应连带封 IP")
	}
	if got := e.ipPoints(t, devIP); got != 4 {
		t.Fatalf("IP 兜底分应为 20/5=4，实际 %d", got)
	}
	d, _ := e.dev.Detail(key)
	if d.Via != devIP || len(d.Clients) == 0 || d.Clients[0].Host != devHost {
		t.Fatalf("设备条目应记下来源 IP 与身份串：%+v", d)
	}
	events := len(d.Events)

	code := e.register(t, "203.0.113.50", "cid_share")
	resp, closed := e.join(t, devIP, code, devCID, devHost)
	wantBlocked(t, resp, closed, "被封设备拿正确的码")
	if d, _ := e.dev.Detail(key); len(d.Events) != events || d.Rejected == 0 {
		t.Fatalf("被拒不应再计分、应计一次被拒：events %d→%d rejected=%d", events, len(d.Events), d.Rejected)
	}
	if got := e.ipPoints(t, devIP); got != 4 {
		t.Fatalf("被拒不应累计 IP 兜底，IP 分值 %d", got)
	}
	resp, closed = e.join(t, devIP, code, devCID2, devHost2)
	if !resp.Success || closed {
		t.Fatalf("同 IP 的另一台设备应能加入（被封设备不该占住会话）：%+v closed=%v", resp, closed)
	}
}

// 被封设备在 join 限流之前拦下：限流把所有 join 都拒掉时，被封设备仍然得到「访问受限」，别人得到普通失败。
func TestDeviceBanCheckedBeforeJoinLimiter(t *testing.T) {
	e := newDevEnv(t, nil)
	e.failN(t, 20, devIP, devCID, "")
	e.srv.joinLimiterPerIP = ratelimit.NewKeyedLimiter(0, 0, 100, time.Minute)
	resp, closed := e.join(t, devIP, wrongPIN, devCID, "")
	wantBlocked(t, resp, closed, "限流前的被封设备")
	if resp, _ := e.join(t, devIP, wrongPIN, devCID2, ""); resp.Error == proto.BlockedMessage {
		t.Fatal("未被封的设备不应收到「访问受限」")
	}
}

// 轮换 ClientID 爆破：每个设备只失败一次、谁也封不到，IP 兜底在第 100 次触发。
func TestDeviceBanRotatingClientIDFallsBackToIP(t *testing.T) {
	e := newDevEnv(t, nil)
	cid := func(i int) string { return fmt.Sprintf("cid_rotating_%04d", i) }
	for i := 0; i < 99; i++ {
		e.failN(t, 1, devIP, cid(i), devHost)
	}
	if e.ip.IsBanned(devIP) {
		t.Fatal("99 次不应封 IP")
	}
	e.failN(t, 1, devIP, cid(99), devHost)
	if !e.ip.IsBanned(devIP) {
		t.Fatal("轮换 ClientID 第 100 次失败应触发 IP 兜底封禁")
	}
	for _, i := range []int{0, 50, 99} {
		if e.dev.IsBanned(e.dev.DeviceKey(blocklist.DeviceByClientID, cid(i))) {
			t.Fatalf("只失败一次的设备 %d 不应被封", i)
		}
	}
}

// 旧 help 没有 client_id：按 Host 身份串加来源 IP 计设备。同一身份串换个来源、同来源换个身份串都是别的设备。
func TestDeviceBanLegacyHelpByHostAndIP(t *testing.T) {
	e := newDevEnv(t, nil)
	e.failN(t, 20, devIP, "", devHost)
	resp, closed := e.join(t, devIP, wrongPIN, "", devHost)
	wantBlocked(t, resp, closed, "同身份串同来源")
	if e.ip.IsBanned(devIP) {
		t.Fatal("设备被封不应连带封 IP")
	}
	if resp, _ := e.join(t, "203.0.113.9", wrongPIN, "", devHost); resp.Error == proto.BlockedMessage {
		t.Fatal("同一身份串换了来源 IP 是另一台设备，不应被拒")
	}
	if resp, _ := e.join(t, devIP, wrongPIN, "", devHost2); resp.Error == proto.BlockedMessage {
		t.Fatal("同来源的另一个身份串是另一台设备，不应被拒")
	}
	if resp, _ := e.join(t, devIP, wrongPIN, devCID, devHost); resp.Error == proto.BlockedMessage {
		t.Fatal("带了 client_id 的按 ClientID 计设备，不应命中按身份串封的设备")
	}
}

// 既没有 client_id 也没有 Host：不建设备键，按 IP 计分，第 20 次封 IP（与改动前相同）。
func TestDeviceBanNoIdentityScoresIP(t *testing.T) {
	e := newDevEnv(t, nil)
	e.failN(t, 19, devIP, "", "")
	if e.ip.IsBanned(devIP) {
		t.Fatal("19 次不应封 IP")
	}
	e.failN(t, 1, devIP, "", "")
	if !e.ip.IsBanned(devIP) {
		t.Fatal("没有设备标识时第 20 次错码应封 IP")
	}
}

// 单连接失败到上限的 5 分记到设备：一条连接 5 次错码 = 5 + 5 分，两条连接封设备；IP 只记兜底分。
func TestDeviceBanJoinFailuresExceededScoresDevice(t *testing.T) {
	e := newDevEnv(t, nil)
	key := e.dev.DeviceKey(blocklist.DeviceByClientID, devCID)
	for conn := 0; conn < 2; conn++ {
		c := &ClientConn{ID: fmt.Sprintf("exceed-%d", conn), Conn: &reuseProtocolConn{remoteAddr: net.JoinHostPort(devIP, "40000")}}
		for i := 0; i < 5; i++ {
			m, _ := proto.NewMessage(proto.MsgJoinRequest, map[string]interface{}{"code": wrongPIN, "client_id": devCID})
			closed := e.srv.handleMessage(c, m)
			if closed != (i == 4) {
				t.Fatalf("第 %d 条连接第 %d 次：closed=%v", conn, i+1, closed)
			}
		}
		if conn == 0 && e.dev.IsBanned(key) {
			t.Fatal("一条连接（10 分）不应封设备")
		}
	}
	if !e.dev.IsBanned(key) {
		t.Fatal("两条连接各失败到上限（20 分）应封设备")
	}
	if e.ip.IsBanned(devIP) || e.ipPoints(t, devIP) != 2 {
		t.Fatalf("IP 只应记兜底分 10/5=2，实际 banned=%v points=%d", e.ip.IsBanned(devIP), e.ipPoints(t, devIP))
	}
}

// 兜底只数协助码失败本身，单连接失败到上限的加重分不折算：5 条连接（各换一个 ClientID）各失败 5 次 = 25 次 → 5 分。
func TestDeviceFallbackIgnoresJoinFailuresExceeded(t *testing.T) {
	e := newDevEnv(t, nil)
	for conn := 0; conn < 5; conn++ {
		c := &ClientConn{ID: fmt.Sprintf("exceed-rot-%d", conn), Conn: &reuseProtocolConn{remoteAddr: net.JoinHostPort(devIP, "40000")}}
		for i := 0; i < 5; i++ {
			m, _ := proto.NewMessage(proto.MsgJoinRequest, map[string]interface{}{"code": wrongPIN, "client_id": fmt.Sprintf("cid_exceed_%d", conn)})
			e.srv.handleMessage(c, m)
		}
	}
	if got := e.ipPoints(t, devIP); got != 5 {
		t.Fatalf("IP 兜底分应为 25/5=5，实际 %d", got)
	}
}

// 超长的 client_id 当作没带：退回按 Host+IP 计设备（不同的超长值落在同一台设备上）。
func TestDeviceBanOversizedClientIDIgnored(t *testing.T) {
	e := newDevEnv(t, nil)
	for i := 0; i < 20; i++ {
		long := strings.Repeat(string(rune('a'+i%2)), maxJoinClientIDLen+1)
		e.failN(t, 1, devIP, long, devHost)
	}
	resp, closed := e.join(t, devIP, wrongPIN, "", devHost)
	wantBlocked(t, resp, closed, "超长 client_id 应按 Host+IP 计设备")
	if e.dev.IsBanned(e.dev.DeviceKey(blocklist.DeviceByClientID, strings.Repeat("a", maxJoinClientIDLen+1))) {
		t.Fatal("超长 client_id 不应建 ClientID 设备键")
	}
}

// register 不受设备封禁影响：被封设备的 ClientID 照常注册为被协助端。
func TestDeviceBanDoesNotAffectRegister(t *testing.T) {
	e := newDevEnv(t, nil)
	e.failN(t, 20, devIP, devCID, devHost)
	if !e.dev.IsBanned(e.dev.DeviceKey(blocklist.DeviceByClientID, devCID)) {
		t.Fatal("前置条件：设备应已被封")
	}
	if code := e.register(t, devIP, devCID); code == "" {
		t.Fatal("被封设备的 ClientID 应能注册")
	}
}

// --blocklist-allow 放行的来源：设备失败不计分、被封设备从这里来也不拦（放行优先，与 IP 名单一致）。
func TestDeviceBanRespectsStaticAllow(t *testing.T) {
	allowed := "203.0.113.20"
	e := newDevEnv(t, func(ip, _ *blocklist.Config, _ *Config) {
		p, err := blocklist.ParseStaticAllow("203.0.113.0/24")
		if err != nil {
			t.Fatal(err)
		}
		ip.StaticAllow = p
	})
	key := e.dev.DeviceKey(blocklist.DeviceByClientID, devCID)
	e.failN(t, 40, allowed, devCID, devHost)
	if e.dev.IsBanned(key) {
		t.Fatal("放行来源上的设备失败不应计分")
	}
	if _, ok := e.dev.Detail(key); ok {
		t.Fatal("放行来源上的设备失败不应建设备条目")
	}
	e.failN(t, 20, devIP, devCID, devHost)
	if !e.dev.IsBanned(key) {
		t.Fatal("前置条件：从别处失败 20 次应封设备")
	}
	if resp, _ := e.join(t, allowed, wrongPIN, devCID, devHost); resp.Error == proto.BlockedMessage {
		t.Fatal("被封设备从放行来源来不应被拦")
	}
}

// 源 IP 不可信（--trust-source-ip=false）时设备名单与 IP 名单一起停用：旧 help 的设备键依赖来源 IP，IP 兜底也没了。
func TestDeviceBanDisabledWithUntrustedSourceIP(t *testing.T) {
	e := newDevEnv(t, func(_, _ *blocklist.Config, cfg *Config) { cfg.DisableSourceIPLimits = true })
	if e.srv.Blocklist() != nil || e.srv.DeviceBlocklist() != nil {
		t.Fatal("源 IP 不可信时两本名单都应停用")
	}
	e.failN(t, 20, devIP, devCID, devHost)
	if e.dev.IsBanned(e.dev.DeviceKey(blocklist.DeviceByClientID, devCID)) {
		t.Fatal("停用后不应再计分")
	}
}

// IP 兜底计数只在内存里，要有上界：空闲超过窗口的零头作废，跟踪的来源数封顶，IPv6 按 /64 归到同一来源。
func TestDeviceFallbackBounded(t *testing.T) {
	var f deviceFallback
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < deviceFallbackEvery-1; i++ {
		if f.hit(devIP, now) {
			t.Fatalf("第 %d 次不应凑满", i+1)
		}
	}
	if !f.hit(devIP, now) {
		t.Fatalf("第 %d 次应凑满、给 IP 记 1 分", deviceFallbackEvery)
	}

	for i := 0; i < deviceFallbackEvery-1; i++ {
		f.hit(devIP, now)
	}
	if f.hit(devIP, now.Add(deviceFallbackIdle+time.Second)) {
		t.Fatal("空闲超过窗口后，之前的零头不应再算")
	}

	for i := 0; i < deviceFallbackEvery-1; i++ {
		f.hit(fmt.Sprintf("2001:db8:0:1::%x", i+1), now)
	}
	if !f.hit("2001:db8:0:1::ffff", now) {
		t.Fatal("同一 /64 里的不同地址应累计到同一来源")
	}

	for i := 0; i < deviceFallbackMaxKeys+100; i++ {
		f.hit(fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff), now)
	}
	if n := len(f.m); n > deviceFallbackMaxKeys {
		t.Fatalf("跟踪的来源数应封顶 %d，实际 %d", deviceFallbackMaxKeys, n)
	}
	f.prune(now.Add(2*deviceFallbackIdle + 2*time.Second)) // 最晚的一次计数在 now+idle+1s
	if n := len(f.m); n != 0 {
		t.Fatalf("空闲超过窗口的来源应被清掉，剩 %d", n)
	}
	if f.hit("not-an-ip", now) {
		t.Fatal("不是合法地址不计")
	}
}

// 只配了 IP 名单（没有设备名单）：行为与改动前完全相同，带 client_id 的失败也按 IP 计分。
func TestDeviceBanWithoutDeviceListScoresIP(t *testing.T) {
	ipl, err := blocklist.Open(blocklist.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ipl.Close()
	srv, err := NewServer(&Config{CodeTTL: 5 * time.Minute, CodeLength: 10, Blocklist: ipl})
	if err != nil {
		t.Fatal(err)
	}
	srv.joinLimiterPerIP, srv.joinLimiterGlobal = nil, nil
	e := &devEnv{srv: srv, ip: ipl}
	e.failN(t, 20, devIP, devCID, devHost)
	if !ipl.IsBanned(devIP) {
		t.Fatal("没有设备名单时应照旧按 IP 计分")
	}
}
