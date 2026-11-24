package blocklist

// 封禁相关审计事件的 message 要自成一句：终端输出 / 服务日志只打 "[级别] 事件: message"，不带 details，
// 所以 IP、时长、第几次、操作人必须写进 message，运维只看日志就能知道封了谁、为什么、封多久、怎么结束的。

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/logger"
)

// auditTestPath 放在 t.TempDir() 之外：审计日志文件句柄整个测试进程都开着，Windows 删不掉它所在的临时目录。
var auditTestPath string

func TestMain(m *testing.M) {
	f, err := os.CreateTemp("", "blocklist-audit-*.log")
	if err != nil {
		panic("TestMain: " + err.Error())
	}
	auditTestPath = f.Name()
	_ = f.Close()
	if err := logger.InitAuditLogger(auditTestPath); err != nil {
		panic("TestMain: InitAuditLogger: " + err.Error())
	}
	code := m.Run()
	_ = os.Remove(auditTestPath)
	os.Exit(code)
}

type auditLine struct {
	Level   string                 `json:"level"`
	Event   string                 `json:"event"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details"`
}

// auditFor 返回审计日志里 details.ip 等于 ip 的所有事件（按写入顺序）。
func auditFor(t *testing.T, ip string) []auditLine {
	t.Helper()
	f, err := os.Open(auditTestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []auditLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var a auditLine
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			continue
		}
		if a.Details["ip"] == ip {
			out = append(out, a)
		}
	}
	return out
}

func auditMsg(t *testing.T, ip, event string) string {
	t.Helper()
	for _, a := range auditFor(t, ip) {
		if a.Event == event {
			return a.Message
		}
	}
	t.Fatalf("审计日志里没有 %s 的 %s 事件：%+v", ip, event, auditFor(t, ip))
	return ""
}

func mustContain(t *testing.T, what, msg string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(msg, w) {
			t.Errorf("%s 的 message 缺少 %q：%q", what, w, msg)
		}
	}
}

// 自动封禁：对象、时长、第几次、触发的累计分值与最后一类事件。
func TestAuditMessageAutoBan(t *testing.T) {
	const ip = "198.51.100.171"
	l := openMem(t, newFakeClock())
	if !hit(l, ip, KindJoinFailed, l.Config().Threshold) {
		t.Fatal("应被自动封禁")
	}
	mustContain(t, "ip_banned(auto)", auditMsg(t, ip, "ip_banned"), ip, "自动封禁", "1 小时", "第 1 次", "累计 20 分", string(KindJoinFailed))
}

// 再犯时长翻倍、次数递增，message 要如实反映。
func TestAuditMessageAutoBanRepeat(t *testing.T) {
	const ip = "198.51.100.172"
	clk := newFakeClock()
	l := openMem(t, clk)
	autoBan(t, l, ip)
	clk.Advance(2 * time.Hour) // 第一次封禁（1 小时）已到期
	l.CheckAccept(ip)
	autoBan(t, l, ip)
	var msgs []string
	for _, a := range auditFor(t, ip) {
		if a.Event == "ip_banned" {
			msgs = append(msgs, a.Message)
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("应有两条自动封禁事件：%q", msgs)
	}
	mustContain(t, "第二次自动封禁", msgs[1], ip, "2 小时", "第 2 次")
}

// 手动封禁、解禁：带上操作人和时长。
func TestAuditMessageManualBanUnban(t *testing.T) {
	const ip = "198.51.100.173"
	l := openMem(t, newFakeClock())
	if _, err := l.Ban(ip, 3*time.Hour, false, "op-alice", "误操作测试"); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "ip_banned(manual)", auditMsg(t, ip, "ip_banned"), ip, "手动封禁", "3 小时", "op-alice")
	if err := l.Unban(ip, "op-bob", "", nil); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "ip_unbanned", auditMsg(t, ip, "ip_unbanned"), ip, "手动解禁", "op-bob")
}

// 到期解除：写明是哪一类封禁到期。
func TestAuditMessageBanExpired(t *testing.T) {
	const ip = "198.51.100.174"
	clk := newFakeClock()
	l := openMem(t, clk)
	autoBan(t, l, ip)
	clk.Advance(2 * time.Hour)
	l.Sweep()
	mustContain(t, "ip_ban_expired", auditMsg(t, ip, "ip_ban_expired"), ip, "自动", "到期")
}

// 白名单到期移除：同样要有 IP。
func TestAuditMessageAllowExpired(t *testing.T) {
	const ip = "198.51.100.176"
	clk := newFakeClock()
	l := openMem(t, clk)
	if err := l.Allow(ip, time.Hour, false, "op-dave", ""); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Hour)
	l.Sweep()
	mustContain(t, "ip_allow_expired", auditMsg(t, ip, "ip_allow_expired"), ip, "白名单", "到期")
}

// 白名单：加入、移出都带 IP 与操作人。
func TestAuditMessageAllowlist(t *testing.T) {
	const ip = "198.51.100.175"
	l := openMem(t, newFakeClock())
	if err := l.Allow(ip, 0, true, "op-carol", ""); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "ip_allowlisted", auditMsg(t, ip, "ip_allowlisted"), ip, "白名单", "永久", "op-carol")
	if err := l.Disallow(ip, "op-carol", ""); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "ip_disallowed", auditMsg(t, ip, "ip_disallowed"), ip, "白名单", "op-carol")
}
