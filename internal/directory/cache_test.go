package directory_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

func cachePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "relays.json")
}

// offer 通过 UpdateCache 把 raw 写进 path 指向的缓存。
func offer(t *testing.T, f *dirtest.Fixture, path string, raw []byte) (changed bool, err error) {
	t.Helper()
	uerr := directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
		_, ch, e := c.Offer(raw, f.Roots, f.Now)
		changed, err = ch, e
		return ch, nil
	})
	if uerr != nil {
		t.Fatalf("UpdateCache: %v", uerr)
	}
	return changed, err
}

func load(t *testing.T, path string) *directory.Cache {
	t.Helper()
	c, err := directory.LoadCache(path)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	return c
}

func withSerial(f *dirtest.Fixture, serial uint64, nodes ...string) []byte {
	var entries []directory.NodeEntry
	for _, p := range nodes {
		entries = append(entries, f.Entry(p))
	}
	b := f.Body(entries...)
	b.Serial = serial
	return f.Sign(b)
}

func TestCacheMissingFile(t *testing.T) {
	f := dirtest.New(t)
	c := load(t, cachePath(t))
	if v, err := c.Current(f.Roots, f.Now); v != nil || err != nil {
		t.Fatalf("Current on empty cache = %v, %v; want nil, nil", v, err)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	if changed, err := offer(t, f, path, withSerial(f, 42, "A", "B")); err != nil || !changed {
		t.Fatalf("first offer: changed=%v err=%v", changed, err)
	}
	v, err := load(t, path).Current(f.Roots, f.Now)
	if err != nil || v == nil || v.Serial != 42 || len(v.Nodes) != 2 {
		t.Fatalf("Current after reload = %+v, %v", v, err)
	}
	if st := load(t, path).State; st.MaxSerial != 42 || st.MaxStatusSerial != 1 {
		t.Fatalf("persisted state = %+v", st)
	}
}

func TestCacheOfferOrdering(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 43, "A"))

	if changed, err := offer(t, f, path, withSerial(f, 42, "A", "B")); !errors.Is(err, directory.ErrRollback) || changed {
		t.Fatalf("older list: changed=%v err=%v, want rollback", changed, err)
	}
	if changed, err := offer(t, f, path, withSerial(f, 43, "A")); err != nil || changed {
		t.Fatalf("same serial: changed=%v err=%v, want unchanged", changed, err)
	}
	bad := withSerial(f, 50, "A")
	bad[len(bad)/2] ^= 1
	if changed, err := offer(t, f, path, bad); err == nil || changed {
		t.Fatalf("corrupted list: changed=%v err=%v", changed, err)
	}
	if changed, err := offer(t, f, path, withSerial(f, 44, "A", "B")); err != nil || !changed {
		t.Fatalf("newer list: changed=%v err=%v", changed, err)
	}
	v, err := load(t, path).Current(f.Roots, f.Now)
	if err != nil || v.Serial != 44 || len(v.Nodes) != 2 {
		t.Fatalf("Current = %+v, %v; want serial 44 with 2 nodes", v, err)
	}
}

// 泄露的状态密钥把缓存的列表序号推到极值后，换了新状态密钥的列表照样能替换进来。
func TestCacheAcceptsNewStatusKeyAfterPoisoning(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, math.MaxUint64, "A"))

	fresh := dirtest.SeedKey("status-2")
	b := f.Body(f.Entry("A"))
	b.StatusKey = f.IssueStatus(2, fresh, f.Primary, directory.SignerPrimary)
	b.Serial = 1
	raw, err := directory.SignDirectory(fresh, b)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := offer(t, f, path, raw); err != nil || !changed {
		t.Fatalf("offer under new status key: changed=%v err=%v", changed, err)
	}
	v, err := load(t, path).Current(f.Roots, f.Now)
	if err != nil || v.Serial != 1 || v.StatusKeySerial != 2 {
		t.Fatalf("Current = %+v, %v; want the list signed by the new status key", v, err)
	}
}

// 缓存文件里的列表每次读取都重新验签，不盲信。
func TestCacheReverifiesOnRead(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A"))

	if _, err := load(t, path).Current(dirtest.New(t).Roots, f.Now); err != nil {
		t.Fatalf("same roots: %v", err)
	}
	other := f.Roots
	other.Keys = nil
	if _, err := load(t, path).Current(other, f.Now); !errors.Is(err, directory.ErrNoRoots) {
		t.Fatalf("Current with no roots = %v", err)
	}
	// 改动文件里签名正文中的一个字节（base64 字符），验签必须失败。
	data, _ := os.ReadFile(path)
	i := bytes.Index(data, []byte(`"payload":"`)) + len(`"payload":"`) + 40
	if data[i] == 'A' {
		data[i] = 'B'
	} else {
		data[i] = 'A'
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(t, path).Current(f.Roots, f.Now); err == nil {
		t.Fatal("tampered cache verified")
	}
}

// 已持久化的吊销在后续列表里即使不再出现也继续生效。
func TestCacheKeepsRevocations(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	a, b := f.Entry("A"), f.Entry("B")
	body := f.Body(a, b)
	body.Revocations = f.Revocations(1, []uint64{nodeSerial(t, b)}, nil, f.Primary, directory.SignerPrimary)
	offer(t, f, path, f.Sign(body))

	body = f.Body(a, b)
	body.Serial = 43
	if changed, err := offer(t, f, path, f.Sign(body)); err != nil || !changed {
		t.Fatalf("offer without revocations: changed=%v err=%v", changed, err)
	}
	v, err := load(t, path).Current(f.Roots, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, v, "B", directory.ErrRevoked)
}

// 位置相同的列表也可能带来新吊销（Worker 违约用同一序号重签了两份，或 relay 转来另一份同序号的列表）。
// 缓存里的列表不换，但吊销只增不减：要记进持久状态，不能只在这一次会话里生效。
func TestCacheSamePositionKeepsNewRevocations(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	a, b := f.Entry("A"), f.Entry("B")
	first := f.Sign(f.Body(a, b))
	offer(t, f, path, first)
	stored := load(t, path).Directory

	body := f.Body(a, b)
	body.Revocations = f.Revocations(1, []uint64{nodeSerial(t, b)}, nil, f.Primary, directory.SignerPrimary)
	revoking := f.Sign(body)
	if changed, err := offer(t, f, path, revoking); err != nil || !changed {
		t.Fatalf("same position with a new revocation: changed=%v err=%v, want it recorded", changed, err)
	}
	c := load(t, path)
	if !bytes.Equal(c.Directory, stored) {
		t.Fatal("cached list replaced by another list at the same position")
	}
	v, err := c.Current(f.Roots, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, v, "B", directory.ErrRevoked)

	// 再来一遍两份列表都不再有新东西，不必写盘；吊销照旧生效。
	for name, raw := range map[string][]byte{"revoking again": revoking, "first again": first} {
		if changed, err := offer(t, f, path, raw); err != nil || changed {
			t.Fatalf("%s: changed=%v err=%v, want unchanged", name, changed, err)
		}
	}
	v, err = load(t, path).Current(f.Roots, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, v, "B", directory.ErrRevoked)
}

func TestCacheCorruptFile(t *testing.T) {
	f := dirtest.New(t)
	for name, content := range map[string][]byte{
		"garbage": []byte("{not json"),
		// 合法 JSON 后接超过上限的空白：只截断读取会把它当成正常文件，必须按大小拒绝。
		"oversize": append([]byte(`{"version":1,"state":{}}`), bytes.Repeat([]byte(" "), 1<<20)...),
		// 超大文件只认开头的 version；第一个字段不是 version 时，哪怕它的值是个更大的数，也不能当成更新的版本。
		"oversize, version not first": append([]byte(`{"other":7,"version":1,"state":{}}`), bytes.Repeat([]byte(" "), 1<<20)...),
		"no version":                  []byte(`{"state":{}}`),
		"version 0":                   []byte(`{"version":0,"state":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			path := cachePath(t)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := directory.LoadCache(path)
			if !errors.Is(err, directory.ErrCacheCorrupt) || c == nil || len(c.Directory) != 0 {
				t.Fatalf("LoadCache = %+v, %v; want empty cache and ErrCacheCorrupt", c, err)
			}
			// 不能从空状态重来：那等于忘掉已记住的位置与吊销，之后旧列表就能回放。
			// 更新一律拒绝、文件保持原样，错误里给出路径，由人删掉文件重置。
			err = directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
				_, changed, err := c.Offer(withSerial(f, 42, "A"), f.Roots, f.Now)
				return changed, err
			})
			if !errors.Is(err, directory.ErrCacheCorrupt) || !strings.Contains(err.Error(), path) {
				t.Fatalf("UpdateCache over corrupt file = %v, want ErrCacheCorrupt naming the file", err)
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, content) {
				t.Fatal("corrupt cache file was overwritten")
			}
		})
	}
}

// 更新的版本写的缓存（版本号更高，格式可能完全不同）不是损坏：同样不用它、不覆盖它，
// 但错误要能和损坏区分开——调用方对损坏提示删除，对这种情况必须提示升级，删掉会丢掉新版本记住的位置和吊销。
func TestCacheNewerVersion(t *testing.T) {
	for name, content := range map[string][]byte{
		"other format": []byte(`{"version":2,"state":["a format this version cannot parse"]}`),
		// 更新的版本可能放宽了大小上限。本版本读不全，但版本号在最前面，照样认得出来。
		"larger than this version allows": []byte(`{"version":2,"state":{},"blob":"` + strings.Repeat("x", 1<<20) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			f := dirtest.New(t)
			path := cachePath(t)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := directory.LoadCache(path)
			if !errors.Is(err, directory.ErrCacheNewer) || errors.Is(err, directory.ErrCacheCorrupt) || c == nil || len(c.Directory) != 0 {
				t.Fatalf("LoadCache = %+v, %v; want empty cache and ErrCacheNewer (not ErrCacheCorrupt)", c, err)
			}
			called := false
			err = directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
				called = true
				_, changed, err := c.Offer(withSerial(f, 42, "A"), f.Roots, f.Now)
				return changed, err
			})
			if !errors.Is(err, directory.ErrCacheNewer) || errors.Is(err, directory.ErrCacheCorrupt) || !strings.Contains(err.Error(), path) || called {
				t.Fatalf("UpdateCache over newer file = %v (fn called %v), want ErrCacheNewer naming the file", err, called)
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, content) {
				t.Fatal("newer cache file was overwritten")
			}
		})
	}
}

// 本版本不认识的顶层字段（更新的版本加的）写回时原样保留，旧版本更新缓存不会抹掉它们。
func TestCacheKeepsUnknownFields(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A"))
	var top map[string]json.RawMessage
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	future := json.RawMessage(`{"addrs":["192.0.2.1:443"],"n":1}`)
	top["future_field"] = future
	if data, err = json.Marshal(top); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if changed, err := offer(t, f, path, withSerial(f, 43, "A")); err != nil || !changed {
		t.Fatalf("offer: changed=%v err=%v", changed, err)
	}
	if v, err := load(t, path).Current(f.Roots, f.Now); err != nil || v.Serial != 43 {
		t.Fatalf("Current = %+v, %v; want the newer list", v, err)
	}
	data, _ = os.ReadFile(path)
	top = nil
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(top["future_field"], future) {
		t.Fatalf("unknown field after rewrite = %s, want %s kept as is", top["future_field"], future)
	}
	// version 必须写在最前面：文件大到读不全时，只能从开头认出版本号（见 TestCacheNewerVersion）。
	if !bytes.HasPrefix(data, []byte(`{"version":`)) {
		t.Fatalf("rewritten cache starts with %.40q, want version first", data)
	}
}

// padEnvelope 往信封里加一个不参与签名的字段，用不转义 HTML 的编码器写出（非 Go 的转发者就是这样）。
func padEnvelope(t *testing.T, raw []byte, n int) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["x"] = strings.Repeat("<", n)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 转发者（镜像、中间人）往信封里塞不参与签名的内容，不能弄坏缓存：存下的列表仍然可用、体积不膨胀，
// 已记住的位置和吊销也不能因此丢掉。
func TestCachePaddedEnvelope(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	a, b := f.Entry("A"), f.Entry("B")
	body := f.Body(a, b)
	body.Revocations = f.Revocations(1, []uint64{nodeSerial(t, b)}, nil, f.Primary, directory.SignerPrimary)
	first := f.Sign(body)
	offer(t, f, path, first)

	// 6 万个 '<'：转义后超过 MaxRawSize；20 万个：转义后整个文件超过 1 MiB。
	for i, n := range []int{60_000, 200_000} {
		next := f.Body(a, b)
		next.Serial = uint64(43 + i)
		clean := f.Sign(next)
		padded := padEnvelope(t, clean, n)
		if len(padded) > directory.MaxRawSize {
			t.Fatalf("padded list is %d bytes; the test needs it under MaxRawSize", len(padded))
		}
		_, _ = offer(t, f, path, padded) // 接受或拒绝都可以，要紧的是缓存仍然完好
		c, err := directory.LoadCache(path)
		if err != nil {
			t.Fatalf("padding %d: LoadCache: %v", n, err)
		}
		// 缓存里只能是某一份签名列表本身（拒收时留着上一份），不能带着填充物。
		if limit := max(len(first), len(clean)); len(c.Directory) > limit {
			t.Fatalf("padding %d: cached directory is %d bytes, the signed lists only %d", n, len(c.Directory), limit)
		}
		if _, err := c.Current(f.Roots, f.Now); err != nil {
			t.Fatalf("padding %d: cached list unusable: %v", n, err)
		}
	}
	old := f.Body(a, b)
	old.Serial = 10
	if changed, err := offer(t, f, path, f.Sign(old)); !errors.Is(err, directory.ErrRollback) || changed {
		t.Fatalf("old list after padding attempts: changed=%v err=%v, want rollback", changed, err)
	}
	v, err := load(t, path).Current(f.Roots, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	skippedOnly(t, v, "B", directory.ErrRevoked)
}

// 信封里重复的键：encoding/json 取最后一个，前面的可以塞任意内容而不影响验签。缓存只能存规范化后的信封。
func TestCacheStoresCanonicalEnvelope(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	clean := withSerial(f, 42, "A")
	junk := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 100_000))
	padded := append([]byte(`{"payload":"`+junk+`",`), clean[1:]...)
	if changed, err := offer(t, f, path, padded); err != nil || !changed {
		t.Fatalf("offer with a duplicate key: changed=%v err=%v", changed, err)
	}
	c := load(t, path)
	if len(c.Directory) > len(clean) {
		t.Fatalf("cached directory is %d bytes, the signed list only %d", len(c.Directory), len(clean))
	}
	if v, err := c.Current(f.Roots, f.Now); err != nil || v.Serial != 42 {
		t.Fatalf("Current = %+v, %v", v, err)
	}
}

// 缓存里同一位置的列表坏了（被改过、被截断），再送来同一份干净的列表要能修好，而不是因为「位置没变」被挡掉。
func TestCacheOfferRepairsSamePosition(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	raw := withSerial(f, 42, "A")
	offer(t, f, path, raw)
	c := load(t, path)
	c.Directory = tamper(t, raw, func(b *directory.Body) { b.Policy = directory.PolicyFastest })
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(t, path).Current(f.Roots, f.Now); err == nil {
		t.Fatal("tampered cache verified")
	}
	if changed, err := offer(t, f, path, raw); err != nil || !changed {
		t.Fatalf("re-offering the clean list: changed=%v err=%v, want it to replace the broken copy", changed, err)
	}
	if v, err := load(t, path).Current(f.Roots, f.Now); err != nil || v.Serial != 42 {
		t.Fatalf("Current after repair = %+v, %v", v, err)
	}
}

// 读缓存也要等正在写的进程：Windows 上读者开着文件时，写入方的 rename 会失败（Access is denied）。
func TestLoadCacheWaitsForWriter(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 41, "A"))
	inUpdate, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
			close(inUpdate)
			<-release
			_, changed, err := c.Offer(withSerial(f, 42, "A"), f.Roots, f.Now)
			return changed, err
		})
	}()
	<-inUpdate
	loaded := make(chan *directory.Cache, 1)
	go func() {
		c, _ := directory.LoadCache(path)
		loaded <- c
	}()
	var early *directory.Cache
	select {
	case early = <-loaded:
		t.Error("LoadCache read the file while another process was updating it")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if early != nil {
		return
	}
	if v, err := (<-loaded).Current(f.Roots, f.Now); err != nil || v.Serial != 42 {
		t.Fatalf("LoadCache after the update = %+v, %v; want the updated list", v, err)
	}
}

// 握有根私钥的人（包括泄露后、被吊销前的主根）不能靠分多份列表灌吊销把缓存撑过上限，
// 否则下一次读缓存就会当成损坏。每份都不超过单份上限、合起来超过的，靠每把根累计的上限挡住：
// 灌满之后再多一条，那份列表整份拒收；两把根都灌满时缓存照样读得出、吊销都在。
func TestCacheRevocationsBounded(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	a := f.Entry("A")
	next := uint64(1) << 52 // 16 位十进制，每条约 17 字节
	batch := func(n int) []uint64 {
		out := make([]uint64, n)
		for i := range out {
			out[i] = next
			next++
		}
		return out
	}
	var serial uint64 = 100
	send := func(key ed25519.PrivateKey, signer string, nodes []uint64) error {
		serial++
		body := f.Body(a)
		body.Serial = serial
		body.Revocations = f.Revocations(serial, nodes, nil, key, signer)
		_, err := offer(t, f, path, f.Sign(body))
		return err
	}
	half := directory.MaxRevokedNodes / 2
	for _, r := range []struct {
		key    ed25519.PrivateKey
		signer string
	}{{f.Primary, directory.SignerPrimary}, {f.Backup, directory.SignerBackup}} {
		for i := range 2 {
			if err := send(r.key, r.signer, batch(half)); err != nil {
				t.Fatalf("%s list %d (%d serials, %d in total) rejected: %v", r.signer, i+1, half, (i+1)*half, err)
			}
		}
		if err := send(r.key, r.signer, batch(1)); !errors.Is(err, directory.ErrMalformed) {
			t.Fatalf("%s list pushing past %d serials: err=%v, want ErrMalformed", r.signer, directory.MaxRevokedNodes, err)
		}
	}
	c, err := directory.LoadCache(path)
	if err != nil {
		fi, _ := os.Stat(path)
		t.Fatalf("cache (%d bytes) unreadable: %v", fi.Size(), err)
	}
	v, err := c.Current(f.Roots, f.Now)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if len(v.RevokedNodes) != 2*directory.MaxRevokedNodes {
		t.Fatalf("%d revoked node serials, want both roots' %d", len(v.RevokedNodes), directory.MaxRevokedNodes)
	}
}

func TestUpdateCacheWritesOnlyOnChange(t *testing.T) {
	path := cachePath(t)
	if err := directory.UpdateCache(path, func(*directory.Cache) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file written although fn reported no change: %v", err)
	}
	wantErr := errors.New("boom")
	if err := directory.UpdateCache(path, func(*directory.Cache) (bool, error) { return true, wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("UpdateCache error = %v, want fn's error", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file written although fn failed: %v", err)
	}
}

// 每次更新都基于磁盘上的最新内容：另一个进程刚写入的新列表不会被旧数据覆盖。
func TestUpdateCacheUsesLatestOnDisk(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	stale := load(t, path) // 「进程 B」很早就读过一次，此时还是空的
	offer(t, f, path, withSerial(f, 43, "A"))

	if _, _, err := stale.Offer(withSerial(f, 42, "A"), f.Roots, f.Now); err != nil {
		t.Fatalf("stale in-memory offer: %v", err) // 内存里的旧副本不知道 43，自己会接受 42
	}
	if changed, err := offer(t, f, path, withSerial(f, 42, "A")); !errors.Is(err, directory.ErrRollback) || changed {
		t.Fatalf("UpdateCache with older list: changed=%v err=%v, want rollback against disk", changed, err)
	}
	if v, _ := load(t, path).Current(f.Roots, f.Now); v == nil || v.Serial != 43 {
		t.Fatalf("disk lost the newer list: %+v", v)
	}
}

// 多个进程同时更新缓存时必须互斥：A 读到旧内容、还没写回时，B 不能插进来写，
// 否则 A 写回时会把 B 刚写入的新列表（连同其中的吊销）覆盖掉。
func TestUpdateCacheSerializesWriters(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 41, "A"))
	raw42, raw43 := withSerial(f, 42, "A"), withSerial(f, 43, "A")
	update := func(raw []byte, before func()) error {
		return directory.UpdateCache(path, func(c *directory.Cache) (bool, error) {
			before()
			_, changed, err := c.Offer(raw, f.Roots, f.Now)
			return changed, err
		})
	}

	inA, releaseA := make(chan struct{}), make(chan struct{})
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() { doneA <- update(raw42, func() { close(inA); <-releaseA }) }()
	<-inA
	go func() { doneB <- update(raw43, func() {}) }()
	select {
	case err := <-doneB:
		t.Errorf("second writer finished while the first was still updating (err %v)", err)
		doneB <- err
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseA)
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
	if v, err := load(t, path).Current(f.Roots, f.Now); err != nil || v.Serial != 43 {
		t.Fatalf("Current = %+v, %v; the newer list was overwritten", v, err)
	}
}

func TestUpdateCacheFileHygiene(t *testing.T) {
	f := dirtest.New(t)
	path := cachePath(t)
	offer(t, f, path, withSerial(f, 42, "A"))
	offer(t, f, path, withSerial(f, 43, "A"))
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{filepath.Base(path), filepath.Base(path) + ".lock"}; !slices.Equal(names, want) {
		t.Fatalf("directory holds %v, want %v (no temp leftovers)", names, want)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("cache file mode = %o, want 600", perm)
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"version":1`) {
		t.Fatalf("cache file lacks version: %s", data)
	}
}
