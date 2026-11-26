package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// joinClientID 让 help 在当前主目录下加入 f 一次，返回这次加入请求里的 client_id。
func joinClientID(t *testing.T, f *joinRelay) string {
	t.Helper()
	before := len(f.requests())
	h := NewHelpModeMCP(nodeHelpConfig(t, f.addr), "W234-5678-9ABC")
	if _, err := join5s(t, h); err != nil {
		t.Fatal(err)
	}
	h.client.Close()
	reqs := f.requests()
	if len(reqs) != before+1 {
		t.Fatalf("应新收到 1 个加入请求，实际 %d", len(reqs)-before)
	}
	return reqs[len(reqs)-1].ClientID
}

// joinTwiceClientIDs 在主目录 home 下对同一个 relay 加入两次，返回两次的 client_id。
func joinTwiceClientIDs(t *testing.T, home string) []string {
	t.Helper()
	setHome(t, home)
	f := startJoinRelay(t, joinOK(nil))
	return []string{joinClientID(t, f), joinClientID(t, f)}
}

func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// help 加入时带一个由本机持久 ClientID 单向派生的设备标识，relay 据此按设备计分、封禁（设计 2026-11-23 §3.1）。
// 不能发 ClientID 原文：relay 凭 ClientID 就能接管同一台机器上 share 的会话并拿到当前协助码，
// help 连上的 relay 不一定可信，原文发过去等于把 share 的会话交给对方。
func TestHelpJoinSendsDerivedDeviceID(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	cid, err := GetOrCreateClientID()
	if err != nil || cid == "" {
		t.Fatalf("GetOrCreateClientID: %q %v", cid, err)
	}

	f := startJoinRelay(t, joinOK(nil))
	got := []string{joinClientID(t, f), joinClientID(t, f)}
	for i, v := range got {
		if v == "" {
			t.Fatalf("第 %d 次加入没有带设备标识", i+1)
		}
		if v == cid || strings.Contains(v, strings.TrimPrefix(cid, "cid_")) {
			t.Fatalf("第 %d 次加入带的是 ClientID 原文 %q（ClientID %q）", i+1, v, cid)
		}
		if len(v) > 64 {
			t.Fatalf("设备标识 %d 字节，超过 relay 接受的 64 字节：%q", len(v), v)
		}
	}
	if got[0] != got[1] {
		t.Fatalf("同一台机器两次加入的设备标识不同：%q / %q", got[0], got[1])
	}

	// 另一台机器（另一个 ClientID）加入同一个 relay，得到另一个设备标识。
	setHome(t, t.TempDir())
	if other := joinClientID(t, f); other == got[0] {
		t.Fatalf("不同 ClientID 派生出相同的设备标识 %q", got[0])
	}
}

// 同一台机器发给不同 relay 的设备标识不同：恶意 relay 拿到的值拿到别的 relay 上冒名制造失败，
// 封不掉这台设备在那里的设备键；几个 relay 之间也没法按它把同一个用户对上。
func TestHelpDeviceIDBoundToRelay(t *testing.T) {
	setHome(t, t.TempDir())
	a, b := startJoinRelay(t, joinOK(nil)), startJoinRelay(t, joinOK(nil))
	idA, idB := joinClientID(t, a), joinClientID(t, b)
	if idA == "" || idB == "" {
		t.Fatalf("应带设备标识：%q / %q", idA, idB)
	}
	if idA == idB {
		t.Fatalf("两个 relay（证书不同）收到同一个设备标识 %q", idA)
	}
	if again := joinClientID(t, a); again != idA {
		t.Fatalf("同一个 relay 两次收到的设备标识不同：%q / %q", idA, again)
	}
	// 绑的是证书指纹，不是地址：同一台 relay 换个地址（域名/IP、列表地址/本机专用地址）访问仍是同一台设备，
	// 否则换个地址就能绕过设备封禁。
	if want := helpDeviceID(a.fp); idA != want {
		t.Fatalf("设备标识 %q 不是按 relay 证书指纹派生的（应为 %q）", idA, want)
	}
}

// ClientID 落不了盘（主目录不存在）时 GetOrCreateClientID 每次都返回新值，带上去 relay 也只会把每次加入
// 当成新设备，不如不带，让 relay 退回按身份串加来源计。
func TestHelpJoinOmitsDeviceIDWhenClientIDNotPersisted(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	for i, v := range joinTwiceClientIDs(t, missing) {
		if v != "" {
			t.Fatalf("ClientID 落不了盘时第 %d 次加入仍带了设备标识 %q", i+1, v)
		}
	}
}

// 拿不到主目录时同样不带。
func TestHelpJoinOmitsDeviceIDWithoutHome(t *testing.T) {
	f := startJoinRelay(t, joinOK(nil))
	setHome(t, "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("清空 HOME/USERPROFILE 后仍能拿到主目录，构造不出这种情况")
	}
	if v := joinClientID(t, f); v != "" {
		t.Fatalf("拿不到主目录时仍带了设备标识 %q", v)
	}
}

// 首次运行时 share 和几个 help 可能同时创建 ClientID 文件：大家必须拿到同一个值，也就是最后留在文件里的值。
// 否则先注册的 share 下次重连读到被别人覆盖的新值，ClientID 一变，relay 复用不了原会话，协助码跟着变。
func TestClientIDConcurrentCreateConverges(t *testing.T) {
	for round := 0; round < 20; round++ {
		home := t.TempDir()
		setHome(t, home)
		const n = 16
		ids := make([]string, n)
		var start, done sync.WaitGroup
		start.Add(1)
		for i := 0; i < n; i++ {
			done.Add(1)
			go func(i int) {
				defer done.Done()
				start.Wait()
				id, persisted, err := loadOrCreateClientID()
				if err != nil || !persisted {
					t.Errorf("loadOrCreateClientID: %q persisted=%v err=%v", id, persisted, err)
				}
				ids[i] = id
			}(i)
		}
		start.Done()
		done.Wait()
		data, err := os.ReadFile(filepath.Join(home, clientIDFileName))
		if err != nil {
			t.Fatal(err)
		}
		for i, id := range ids {
			if id != string(data) {
				t.Fatalf("第 %d 轮：第 %d 个调用方拿到 %q，文件里留下的是 %q", round+1, i+1, id, data)
			}
		}
	}
}

// 钉住派生算法：改成可逆编码会把 ClientID 交给 relay；改前缀、截断长度或拼接方式会让升级后 relay 上
// 已有的设备计分和封禁全部对不上。
func TestHelpDeviceIDGolden(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	if err := os.WriteFile(filepath.Join(home, clientIDFileName), []byte("cid_00112233445566778899aabbccddeeff"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ fp, want string }{
		// 期望值按定义独立算出：hex(SHA-256("remote-assist/help-device-id/v1\x00" + fp + "\x00" + cid)[:16])
		{"", "hd1_c7a27a4fd6abc47c75c54a742a713379"},
		{"0123456789abcdef", "hd1_4f5aae98870f3ed205cd3d91f81a648b"},
	} {
		if got := helpDeviceID(c.fp); got != c.want {
			t.Fatalf("helpDeviceID(%q) = %q，应为 %q", c.fp, got, c.want)
		}
	}
}

// 上次写到一半退出留下的空文件：等一会儿没人写完就自己写，之后稳定返回这个值。
func TestClientIDRecoversFromEmptyFile(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, clientIDFileName)
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	id, persisted, err := loadOrCreateClientID()
	if err != nil || !persisted || id == "" {
		t.Fatalf("loadOrCreateClientID: %q persisted=%v err=%v", id, persisted, err)
	}
	if data, _ := os.ReadFile(path); string(data) != id {
		t.Fatalf("文件里是 %q，返回的是 %q", data, id)
	}
	if again, _, _ := loadOrCreateClientID(); again != id {
		t.Fatalf("第二次拿到 %q，应为 %q", again, id)
	}
}

// 文件存在但为空，多半是另一个进程刚建好、还没写完：等它写完用它的值，不抢先覆盖。
func TestClientIDWaitsForWriterOfEmptyFile(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, clientIDFileName)
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	const theirs = "cid_written_by_the_creator"
	done := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		done <- os.WriteFile(path, []byte(theirs), 0600)
	}()
	id, persisted, err := loadOrCreateClientID()
	if werr := <-done; werr != nil {
		t.Fatal(werr)
	}
	if err != nil || !persisted || id != theirs {
		t.Fatalf("loadOrCreateClientID = %q persisted=%v err=%v，应等建文件的一方写完并返回 %q", id, persisted, err, theirs)
	}
}

// 文件在但读不了（权限、被占用）：这次用临时 ID，不能把原有的 ClientID 覆盖掉——share 的会话复用全靠它。
func TestClientIDUnreadableFileNotOverwritten(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, clientIDFileName)
	const orig = "cid_original_value"
	if err := os.WriteFile(path, []byte(orig), 0600); err != nil {
		t.Fatal(err)
	}
	restore := makeUnreadable(t, path)
	if _, err := os.ReadFile(path); err == nil {
		restore()
		t.Skip("构造不出读不了的文件（可能以 root/管理员身份运行）")
	}
	id, persisted, err := loadOrCreateClientID()
	restore()
	if err != nil || persisted || id == orig || id == "" {
		t.Fatalf("loadOrCreateClientID = %q persisted=%v err=%v，应返回未落盘的临时 ID", id, persisted, err)
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Fatalf("原有 ClientID 被覆盖成 %q", data)
	}
}

// makeUnreadable 让 path 读不了但仍可写，返回恢复函数。
func makeUnreadable(t *testing.T, path string) (restore func()) {
	t.Helper()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0200); err != nil {
			t.Fatal(err)
		}
		return func() { _ = os.Chmod(path, 0600) }
	}
	u, err := user.Current()
	if err != nil {
		t.Skip("取不到当前用户：", err)
	}
	// 只拒绝「读数据」：打开读失败，打开写仍然成功。
	if out, err := exec.Command("icacls", path, "/deny", u.Username+":(RD)").CombinedOutput(); err != nil {
		t.Skipf("icacls 设置失败：%v %s", err, out)
	}
	return func() { _ = exec.Command("icacls", path, "/remove:d", u.Username).Run() }
}

// 线上字段名是 client_id，没有值时不出现（旧 relay 照常解码，抓包里也不多一个空字段）。
func TestJoinRequestClientIDWireName(t *testing.T) {
	raw, err := json.Marshal(&proto.JoinRequest{Code: "W234-5678-9ABC", ClientID: "cid_x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"client_id":"cid_x"`) {
		t.Fatalf("应带 client_id：%s", raw)
	}
	raw, _ = json.Marshal(&proto.JoinRequest{Code: "W234-5678-9ABC"})
	if strings.Contains(string(raw), "client_id") {
		t.Fatalf("没有 ClientID 时不应出现该字段：%s", raw)
	}
}
