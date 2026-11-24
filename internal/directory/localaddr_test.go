package directory_test

import (
	"os"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// setLocal 通过 UpdateCache 记下一个本机专用地址。
func setLocal(t *testing.T, path, prefix, addr string) (changed bool, err error) {
	t.Helper()
	uerr := directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
		changed, err = c.SetLocalAddr(prefix, addr)
		return changed, nil
	})
	if uerr != nil {
		t.Fatalf("UpdateCache: %v", uerr)
	}
	return changed, err
}

func TestCacheLocalAddrRoundTrip(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A", "K"))
	if changed, err := setLocal(t, path, "K", "192.0.2.10:8443"); err != nil || !changed {
		t.Fatalf("SetLocalAddr: changed=%v err=%v", changed, err)
	}
	if changed, err := setLocal(t, path, "K", "192.0.2.10:8443"); err != nil || changed {
		t.Fatalf("记下同一个地址不该算变化: changed=%v err=%v", changed, err)
	}

	c := load(t, path)
	v, err := c.Current(f.Roots, f.Now)
	if err != nil || v == nil {
		t.Fatalf("Current: %v, %v", v, err)
	}
	if got := c.LocalAddr(v, "K"); got != "192.0.2.10:8443" {
		t.Fatalf("LocalAddr(K) = %q, want 192.0.2.10:8443", got)
	}
	if got := c.LocalAddr(v, "A"); got != "" {
		t.Fatalf("LocalAddr(A) = %q, want empty (没记过)", got)
	}
	if got := c.LocalAddr(nil, "K"); got != "" {
		t.Fatalf("没有列表时不该给出本机专用地址, got %q", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"version":`) {
		t.Fatalf("version 必须仍是文件里的第一个字段: %.40s", raw)
	}
	if n := strings.Count(string(raw), `"local_addrs"`); n != 1 {
		t.Fatalf(`"local_addrs" 在文件里出现 %d 次, want 1:\n%s`, n, raw)
	}
	if v2, err := c.Current(f.Roots, f.Now); err != nil || v2.Serial != 42 {
		t.Fatalf("记本机专用地址不该动列表和状态: %+v %v", v2, err)
	}
}

// 本机专用地址只在列表里还有这个节点时才使用：节点被丢弃或列表里没有它，就不再给出。
func TestCacheLocalAddrOnlyWhileNodeListed(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A", "K"))
	setLocal(t, path, "K", "192.0.2.10:8443")

	offer(t, f, path, withSerial(f, 43, "A")) // K 从列表里消失
	c := load(t, path)
	v, err := c.Current(f.Roots, f.Now)
	if err != nil || v == nil {
		t.Fatalf("Current: %v, %v", v, err)
	}
	if got := c.LocalAddr(v, "K"); got != "" {
		t.Fatalf("节点已不在列表里, LocalAddr = %q, want empty", got)
	}

	// 节点回到列表里，记过的地址还在（只是暂时不用）。
	offer(t, f, path, withSerial(f, 44, "A", "K"))
	c = load(t, path)
	v, _ = c.Current(f.Roots, f.Now)
	if got := c.LocalAddr(v, "K"); got != "192.0.2.10:8443" {
		t.Fatalf("节点回到列表后 LocalAddr = %q, want 192.0.2.10:8443", got)
	}
}

func TestCacheLocalAddrRejectsBadInput(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A", "K"))
	cases := []struct{ name, prefix, addr string }{
		{"带 URL scheme", "K", "https://192.0.2.10:8443"},
		{"没有端口", "K", "192.0.2.10"},
		{"端口 0", "K", "192.0.2.10:0"},
		{"含空格", "K", "192.0.2.10 :8443"},
		{"控制字符", "K", "192.0.2.10:8443\n"},
		{"空地址", "K", ""},
		{"前缀不在码字符集里", "I", "192.0.2.10:8443"},
		{"前缀是小写", "k", "192.0.2.10:8443"},
		{"前缀是两个字符", "KA", "192.0.2.10:8443"},
		{"前缀为空", "", "192.0.2.10:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := setLocal(t, path, tc.prefix, tc.addr)
			if err == nil || changed {
				t.Fatalf("SetLocalAddr(%q, %q): changed=%v err=%v, want error", tc.prefix, tc.addr, changed, err)
			}
		})
	}
	c := load(t, path)
	v, _ := c.Current(f.Roots, f.Now)
	if got := c.LocalAddr(v, "K"); got != "" {
		t.Fatalf("被拒绝的输入不该留下记录, got %q", got)
	}
}

func TestCacheLocalAddrDelete(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A", "K"))
	setLocal(t, path, "K", "192.0.2.10:8443")
	setLocal(t, path, "A", "192.0.2.11:8443")

	var changed, again bool
	if err := directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
		changed = c.DeleteLocalAddr("K", "192.0.2.10:8443")
		again = c.DeleteLocalAddr("K", "192.0.2.10:8443")
		return changed, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !changed || again {
		t.Fatalf("DeleteLocalAddr: first=%v second=%v, want true then false", changed, again)
	}
	c := load(t, path)
	v, _ := c.Current(f.Roots, f.Now)
	if got := c.LocalAddr(v, "K"); got != "" {
		t.Fatalf("删除后 LocalAddr(K) = %q", got)
	}
	if got := c.LocalAddr(v, "A"); got != "192.0.2.11:8443" {
		t.Fatalf("删除 K 不该动 A, got %q", got)
	}
}

// 只删「连不上的那个地址」：长时间运行的 share 读到的是旧地址 A，期间别的进程已把这个节点改记为 B，
// share 拨 A 失败后去删，不能把刚记下的、能用的 B 一起删掉（多个进程共用这个缓存文件）。
func TestCacheLocalAddrDeleteOnlyMatchingAddr(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A", "K"))
	setLocal(t, path, "K", "192.0.2.10:8443")
	setLocal(t, path, "K", "192.0.2.20:8443") // 别的进程改记了新地址

	var stale, current bool
	if err := directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
		stale = c.DeleteLocalAddr("K", "192.0.2.10:8443") // 旧进程要删的是旧地址
		return stale, nil
	}); err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("当前记的已经是别的地址，按旧地址删应当什么都不做")
	}
	c := load(t, path)
	v, _ := c.Current(f.Roots, f.Now)
	if got := c.LocalAddr(v, "K"); got != "192.0.2.20:8443" {
		t.Fatalf("新记的地址被误删: LocalAddr = %q, want 192.0.2.20:8443", got)
	}
	if err := directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
		current = c.DeleteLocalAddr("K", "192.0.2.20:8443")
		return current, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !current {
		t.Fatal("按当前记的地址删应当成功")
	}
}

// 前缀大小写：NodeByPrefix 不区分大小写，查本机专用地址也必须一致，
// 否则调用方拿用户输入的小写码首位去查，会悄悄跳过本机专用地址。
func TestCacheLocalAddrPrefixCaseInsensitive(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A", "K"))
	setLocal(t, path, "K", "192.0.2.10:8443")
	c := load(t, path)
	v, _ := c.Current(f.Roots, f.Now)
	if got := c.LocalAddr(v, "k"); got != "192.0.2.10:8443" {
		t.Fatalf("LocalAddr(v, %q) = %q, want 192.0.2.10:8443", "k", got)
	}
}

// 新列表替换旧列表时，本机专用地址不能被一并清掉。
func TestCacheLocalAddrSurvivesOffer(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A", "K"))
	setLocal(t, path, "K", "192.0.2.10:8443")
	offer(t, f, path, withSerial(f, 43, "A", "K"))
	c := load(t, path)
	v, _ := c.Current(f.Roots, f.Now)
	if got := c.LocalAddr(v, "K"); got != "192.0.2.10:8443" {
		t.Fatalf("换列表后 LocalAddr = %q, want 192.0.2.10:8443", got)
	}
}

// local_addrs 里的内容不被信任，也不能因为它写坏了就让整个缓存变成「损坏」：
// 缓存里还记着位置与吊销，损坏会让后续更新全部被拒。读出来时只留下合法的条目。
func TestCacheLocalAddrsToleratesGarbage(t *testing.T) {
	f := dirtest.New(t)
	for name, garbage := range map[string]string{
		"不是对象":       `"oops"`,
		"null":       `null`,
		"值不是字符串":     `{"K": 5, "A": ["x"]}`,
		"夹着非法条目":     `{"K": "https://evil.example.test", "A": "192.0.2.11:8443", "i": "192.0.2.12:8443", "II": "192.0.2.13:8443"}`,
		"数组":         `["K"]`,
		"值里带控制字符":    `{"K": "192.0.2.10:8443\u001b[2J"}`,
		"空对象":        `{}`,
		"嵌套对象":       `{"K": {"addr": "192.0.2.10:8443"}}`,
		"过长的地址":      `{"K": "` + strings.Repeat("a", 300) + `:8443"}`,
		"键含不可见字符":    `{"K\u200b": "192.0.2.10:8443"}`,
		"键是码字符集外的字母": `{"O": "192.0.2.10:8443"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := cachePath(t)
			offer(t, f, path, withSerial(f, 42, "A", "K"))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			patched := strings.TrimSuffix(strings.TrimSpace(string(raw)), "}") + `,"local_addrs":` + garbage + "}"
			if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := directory.LoadCache(path)
			if err != nil {
				t.Fatalf("local_addrs 写坏不该让缓存变成损坏: %v", err)
			}
			v, err := c.Current(f.Roots, f.Now)
			if err != nil || v == nil {
				t.Fatalf("Current: %v, %v", v, err)
			}
			// 列表里只有 A、K：被写坏的 K 条目不能读出来。（I、O 不在列表里，查它们永远是空，没有检验价值；
			// 它们这类非法键有没有被丢掉，由下面写回文件后的断言检验。）
			if got := c.LocalAddr(v, "K"); got != "" {
				t.Fatalf("LocalAddr(K) = %q, want empty", got)
			}
			want := ""
			if name == "夹着非法条目" {
				want = "192.0.2.11:8443"
			}
			if got := c.LocalAddr(v, "A"); got != want {
				t.Fatalf("LocalAddr(A) = %q, want %q", got, want)
			}
			// 之后照常能更新缓存，而且写回去的只有合法条目：写坏的部分不能被原样带回文件。
			if changed, err := setLocal(t, path, "K", "192.0.2.10:8443"); err != nil || !changed {
				t.Fatalf("写坏后再记地址: changed=%v err=%v", changed, err)
			}
			out, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wantField := `"local_addrs":{"K":"192.0.2.10:8443"}`
			if name == "夹着非法条目" {
				wantField = `"local_addrs":{"A":"192.0.2.11:8443","K":"192.0.2.10:8443"}`
			}
			if !strings.Contains(string(out), wantField) || strings.Count(string(out), `"local_addrs"`) != 1 {
				t.Fatalf("写回的 local_addrs 应当只含合法条目 %s:\n%s", wantField, out)
			}
		})
	}
}

// 本程序不认识的顶层字段仍然原样保留（记本机专用地址不能破坏这条规则）。
func TestCacheLocalAddrKeepsUnknownFields(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A", "K"))
	raw, _ := os.ReadFile(path)
	patched := strings.TrimSuffix(strings.TrimSpace(string(raw)), "}") + `,"future_field":{"x":1}}`
	os.WriteFile(path, []byte(patched), 0o600)
	setLocal(t, path, "K", "192.0.2.10:8443")
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), `"future_field":{"x":1}`) {
		t.Fatalf("未知字段丢了:\n%s", out)
	}
	if !strings.Contains(string(out), `"local_addrs":{"K":"192.0.2.10:8443"}`) {
		t.Fatalf("本机专用地址没写进去:\n%s", out)
	}
}
