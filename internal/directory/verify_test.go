package directory_test

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

func verify(t *testing.T, f *dirtest.Fixture, raw []byte, st directory.State) (*directory.Verified, error) {
	t.Helper()
	return directory.Verify(raw, f.Roots, st, f.Now)
}

func mustVerify(t *testing.T, f *dirtest.Fixture, raw []byte, st directory.State) *directory.Verified {
	t.Helper()
	v, err := verify(t, f, raw, st)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return v
}

// wantErr 断言 err 是 *VerifyError，哨兵错误为 sentinel，出错层为 layer（layer 为空则不查）。
func wantErr(t *testing.T, err error, sentinel error, layer string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Verify succeeded, want %v", sentinel)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Verify error = %v, want %v", err, sentinel)
	}
	var ve *directory.VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("Verify error %T is not *VerifyError", err)
	}
	if layer != "" && ve.Layer != layer {
		t.Fatalf("Verify error layer = %q, want %q (err: %v)", ve.Layer, layer, err)
	}
}

func skippedOnly(t *testing.T, v *directory.Verified, prefix string, sentinel error) {
	t.Helper()
	if len(v.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want exactly one (%s)", v.Skipped, prefix)
	}
	s := v.Skipped[0]
	if s.Prefix != prefix || !errors.Is(s.Err, sentinel) {
		t.Fatalf("skipped = {%s %d %v}, want prefix %s with %v", s.Prefix, s.Serial, s.Err, prefix, sentinel)
	}
	if v.NodeByPrefix(prefix) != nil {
		t.Fatalf("skipped node %s is still returned by NodeByPrefix", prefix)
	}
}

func TestVerifyValid(t *testing.T) {
	f := dirtest.New(t)
	a, b := f.Entry("A"), f.Entry("B")
	b.Status, b.Weight = directory.StatusDraining, 40
	body := f.Body(a, b)
	body.Policy = directory.PolicyFastest
	v := mustVerify(t, f, f.Sign(body), directory.State{})

	if v.Serial != 42 || v.Policy != directory.PolicyFastest || v.DefaultPrefix != "A" || v.StatusKeySerial != 1 {
		t.Fatalf("header = serial %d policy %q default %q status %d", v.Serial, v.Policy, v.DefaultPrefix, v.StatusKeySerial)
	}
	if !v.ExpiresAt.Equal(time.Unix(body.ExpiresAt, 0)) || !v.IssuedAt.Equal(time.Unix(body.IssuedAt, 0)) {
		t.Fatalf("times = %v %v", v.IssuedAt, v.ExpiresAt)
	}
	if len(v.Nodes) != 2 || len(v.Skipped) != 0 {
		t.Fatalf("nodes %d skipped %+v", len(v.Nodes), v.Skipped)
	}
	nb := v.NodeByPrefix("b") // 不区分大小写
	if nb == nil || nb.Name != "node-B" || nb.Status != directory.StatusDraining || nb.Weight != 40 {
		t.Fatalf("NodeByPrefix(b) = %+v", nb)
	}
	if d := v.DefaultNode(); d == nil || d.Prefix != "A" {
		t.Fatalf("DefaultNode = %+v", d)
	}
	if n := v.NodeByFingerprint(dirtest.Fingerprint("B")); n == nil || n.Prefix != "B" {
		t.Fatalf("NodeByFingerprint = %+v", n)
	}
	if v.NodeByFingerprint(dirtest.Fingerprint("Z")) != nil || v.NodeByPrefix("Z") != nil || v.NodeByPrefix("") != nil {
		t.Fatal("lookups of unknown nodes must return nil")
	}
	want := directory.State{StatusRoot: primaryFP, MaxSerial: 42, MaxStatusSerial: 1}
	if !stateEqual(v.NewState, want) {
		t.Fatalf("NewState = %+v, want %+v", v.NewState, want)
	}
}

// 夹具的根密钥由名字确定性派生，指纹在建夹具之前就能算出来（表驱动用例里要用）。
var (
	primaryFP = directory.RootFingerprint(dirtest.SeedKey("primary").Public().(ed25519.PublicKey))
	backupFP  = directory.RootFingerprint(dirtest.SeedKey("backup").Public().(ed25519.PublicKey))
)

func stateEqual(a, b directory.State) bool {
	return a.StatusRoot == b.StatusRoot && a.MaxSerial == b.MaxSerial && a.MaxStatusSerial == b.MaxStatusSerial &&
		slices.EqualFunc(a.Revocations, b.Revocations, func(x, y directory.RevocationRecord) bool {
			return x.Root == y.Root && x.MinStatusSerial == y.MinStatusSerial &&
				slices.Equal(x.NodeSerials, y.NodeSerials) && slices.Equal(x.Roots, y.Roots)
		})
}

// seen 返回「已接受过夹具状态密钥（主根授权，序号 1）签的、列表序号为 serial 的列表」后的状态。
func seen(serial uint64) directory.State {
	return directory.State{StatusRoot: primaryFP, MaxStatusSerial: 1, MaxSerial: serial}
}

// revokedBy 返回只含一条吊销记录的状态：root（指纹）签过的节点吊销与根吊销。
func revokedBy(root string, nodes []uint64, roots ...string) directory.State {
	return directory.State{Revocations: []directory.RevocationRecord{{Root: root, NodeSerials: nodes, Roots: roots}}}
}

func TestVerifyNilVerifiedLookups(t *testing.T) {
	var v *directory.Verified
	if v.NodeByPrefix("A") != nil || v.NodeByFingerprint("x") != nil || v.DefaultNode() != nil {
		t.Fatal("nil *Verified lookups must return nil")
	}
}

func TestVerifyNoRoots(t *testing.T) {
	f := dirtest.New(t)
	raw := f.Sign(f.Body(f.Entry("A")))
	for _, roots := range []directory.Roots{
		{},
		{Keys: map[string]ed25519.PublicKey{directory.SignerPrimary: make([]byte, 5)}},
	} {
		_, err := directory.Verify(raw, roots, directory.State{}, f.Now)
		wantErr(t, err, directory.ErrNoRoots, "")
	}
	if len(directory.BuiltinRoots().Keys) == 0 {
		_, err := directory.Verify(raw, directory.BuiltinRoots(), directory.State{}, f.Now)
		wantErr(t, err, directory.ErrNoRoots, "")
	}
}

func TestVerifyRejectsMalformedInput(t *testing.T) {
	f := dirtest.New(t)
	clean := f.Sign(f.Body(f.Entry("A")))
	cases := map[string][]byte{
		"empty":     nil,
		"not json":  []byte("<html>"),
		"null":      []byte("null"),
		"too large": append(slices.Clone(clean), make([]byte, directory.MaxRawSize)...),
		// 信封里只有 kind 和 payload 参与签名：多出来的字段、信封后面跟着的内容都不是签名保护的，一律拒收，
		// 不能指望缓存再编码时顺手丢掉。
		"unknown field":          append([]byte(`{"extra":"x",`), clean[1:]...),
		"trailing object":        append(slices.Clone(clean), []byte(`{"kind":"directory"}`)...),
		"trailing garbage":       append(slices.Clone(clean), []byte(" garbage")...),
		"two envelopes in a row": append(append(slices.Clone(clean), '\n'), clean...),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := verify(t, f, raw, directory.State{})
			wantErr(t, err, directory.ErrMalformed, "directory")
		})
	}
	// 末尾的空白（HTTP 响应常带一个换行）不算多余内容。
	mustVerify(t, f, append(slices.Clone(clean), " \r\n"...), directory.State{})
}

// 验签之前就会出的错（这里是信封里的未知字段）也会打到终端、写进 relay 每次刷新的日志，而里面的文本由对方决定：
// 要有长度上限，也不能带控制字符。
func TestVerifyErrorTextIsBounded(t *testing.T) {
	f := dirtest.New(t)
	clean := f.Sign(f.Body(f.Entry("A")))
	name := strings.Repeat(`\u001b`, 30000) // JSON 转义，解出来是控制字符
	raw := append([]byte(`{"`+name+`":"x",`), clean[1:]...)
	_, err := verify(t, f, raw, directory.State{})
	wantErr(t, err, directory.ErrMalformed, "directory")
	msg := err.Error()
	if len(msg) > 4096 {
		t.Errorf("error text is %d bytes, want it clipped", len(msg))
	}
	if !strings.Contains(msg, "truncated") || strings.Contains(msg, "\x1b") {
		t.Errorf("error %.200q... should be escaped and say it was truncated", msg)
	}
}

// tamper 改写已签名列表的正文但保留原签名，模拟传输中被篡改。
func tamper(t *testing.T, raw []byte, mod func(*directory.Body)) []byte {
	t.Helper()
	var env directory.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var b directory.Body
	if err := json.Unmarshal(env.Payload, &b); err != nil {
		t.Fatal(err)
	}
	mod(&b)
	env.Payload = dirtest.MustJSON(t, b)
	return dirtest.MustJSON(t, env)
}

func TestVerifyDirectorySignature(t *testing.T) {
	f := dirtest.New(t)
	good := f.Sign(f.Body(f.Entry("A")))

	t.Run("tampered body", func(t *testing.T) {
		_, err := verify(t, f, tamper(t, good, func(b *directory.Body) { b.Serial++ }), directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "directory")
	})
	t.Run("signed by unauthorized key", func(t *testing.T) {
		raw, err := directory.SignDirectory(dirtest.SeedKey("evil-status"), f.Body(f.Entry("A")))
		if err != nil {
			t.Fatal(err)
		}
		_, err = verify(t, f, raw, directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "directory")
	})
	t.Run("wrong kind", func(t *testing.T) {
		// 状态密钥对同一正文签了别的 kind：签名域分离必须让它失效。
		env := directory.Sign(f.Status, directory.KindNodeCert, directory.SignerStatus, dirtest.MustJSON(t, f.Body(f.Entry("A"))))
		_, err := verify(t, f, dirtest.MustJSON(t, env), directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "directory")
		env.Kind = directory.KindDirectory // 改 kind 字段也没用：kind 参与签名
		_, err = verify(t, f, dirtest.MustJSON(t, env), directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "directory")
	})
	t.Run("wrong signer name", func(t *testing.T) {
		var env directory.Envelope
		_ = json.Unmarshal(good, &env)
		env.Signer = directory.SignerPrimary
		_, err := verify(t, f, dirtest.MustJSON(t, env), directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "directory")
	})
}

func TestVerifyStatusKey(t *testing.T) {
	sign := func(f *dirtest.Fixture, sk directory.Envelope) []byte {
		b := f.Body(f.Entry("A"))
		b.StatusKey = sk
		return f.Sign(b)
	}

	t.Run("authorized by backup root", func(t *testing.T) {
		f := dirtest.New(t)
		mustVerify(t, f, sign(f, f.IssueStatus(1, f.Status, f.Backup, directory.SignerBackup)), directory.State{})
	})
	t.Run("authorized by unknown root", func(t *testing.T) {
		f := dirtest.New(t)
		raw := sign(f, f.IssueStatus(1, f.Status, dirtest.SeedKey("evil-root"), directory.SignerPrimary))
		_, err := verify(t, f, raw, directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "status_key")
	})
	t.Run("signer name not a root", func(t *testing.T) {
		f := dirtest.New(t)
		raw := sign(f, f.IssueStatus(1, f.Status, f.Status, directory.SignerStatus))
		_, err := verify(t, f, raw, directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "status_key")
	})
	t.Run("node cert replayed as status key", func(t *testing.T) {
		f := dirtest.New(t)
		sk := f.IssueStatus(1, f.Status, f.Primary, directory.SignerPrimary)
		forged := directory.Sign(f.Primary, directory.KindNodeCert, directory.SignerPrimary, sk.Payload)
		forged.Kind = directory.KindStatusKey
		_, err := verify(t, f, sign(f, forged), directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "status_key")
	})
	t.Run("pubkey does not match signing key", func(t *testing.T) {
		f := dirtest.New(t)
		raw := sign(f, f.IssueStatus(1, dirtest.SeedKey("other-status"), f.Primary, directory.SignerPrimary))
		_, err := verify(t, f, raw, directory.State{})
		wantErr(t, err, directory.ErrBadSignature, "directory")
	})

	validity := func(t *testing.T, nb, na time.Time, sentinel error) {
		t.Helper()
		f := dirtest.New(t)
		payload := dirtest.MustJSON(t, directory.StatusKeyCert{
			Serial: 1, PubKey: f.Status.Public().(ed25519.PublicKey), NotBefore: nb.Unix(), NotAfter: na.Unix(),
		})
		raw := sign(f, directory.Sign(f.Primary, directory.KindStatusKey, directory.SignerPrimary, payload))
		_, err := verify(t, f, raw, directory.State{})
		if sentinel == nil {
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			return
		}
		wantErr(t, err, sentinel, "status_key")
	}
	now := dirtest.Epoch
	day := 24 * time.Hour
	t.Run("expired", func(t *testing.T) {
		validity(t, now.Add(-10*day), now.Add(-directory.ClockSkew-time.Second), directory.ErrExpired)
	})
	t.Run("expired within skew", func(t *testing.T) { validity(t, now.Add(-10*day), now.Add(-directory.ClockSkew+time.Second), nil) })
	t.Run("not yet valid", func(t *testing.T) {
		validity(t, now.Add(directory.ClockSkew+time.Second), now.Add(10*day), directory.ErrNotYetValid)
	})
	t.Run("not yet valid within skew", func(t *testing.T) { validity(t, now.Add(directory.ClockSkew-time.Second), now.Add(10*day), nil) })

	t.Run("serial rollback against state", func(t *testing.T) {
		f := dirtest.New(t)
		_, err := verify(t, f, sign(f, f.StatusCert), directory.State{MaxStatusSerial: 2})
		wantErr(t, err, directory.ErrRollback, "status_key")
	})
	t.Run("serial below builtin minimum", func(t *testing.T) {
		f := dirtest.New(t)
		f.Roots.MinStatusSerial = 2
		_, err := verify(t, f, sign(f, f.StatusCert), directory.State{})
		wantErr(t, err, directory.ErrRollback, "status_key")
	})
	t.Run("newer serial raises state", func(t *testing.T) {
		f := dirtest.New(t)
		v := mustVerify(t, f, sign(f, f.IssueStatus(7, f.Status, f.Primary, directory.SignerPrimary)), directory.State{MaxStatusSerial: 3})
		if v.NewState.MaxStatusSerial != 7 || v.StatusKeySerial != 7 {
			t.Fatalf("status serial = %d / %d, want 7", v.NewState.MaxStatusSerial, v.StatusKeySerial)
		}
	})
}

func TestVerifyDirectoryHeader(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name     string
		mod      func(f *dirtest.Fixture, b *directory.Body)
		st       directory.State
		sentinel error // nil 表示应当通过
	}{
		{"serial rollback", nil, seen(43), directory.ErrRollback},
		{"same serial again", nil, seen(42), nil},
		// 列表序号只在同一把状态密钥下比较：见过的是更早的状态密钥时，列表序号从头算。
		{"lower serial under a newer status key", nil, directory.State{StatusRoot: primaryFP, MaxSerial: 43}, nil},
		{"zero serial", func(f *dirtest.Fixture, b *directory.Body) { b.Serial = 0 }, directory.State{}, directory.ErrMalformed},
		{"unknown format", func(f *dirtest.Fixture, b *directory.Body) { b.Format = directory.FormatVersion + 1 }, directory.State{}, directory.ErrMalformed},
		{"expired", func(f *dirtest.Fixture, b *directory.Body) {
			b.IssuedAt = f.Now.Add(-20 * day).Unix()
			b.ExpiresAt = f.Now.Add(-directory.ClockSkew - time.Second).Unix()
		}, directory.State{}, directory.ErrExpired},
		{"expired within skew", func(f *dirtest.Fixture, b *directory.Body) {
			b.IssuedAt = f.Now.Add(-20 * day).Unix()
			b.ExpiresAt = f.Now.Add(-directory.ClockSkew + time.Second).Unix()
		}, directory.State{}, nil},
		{"issued in the future", func(f *dirtest.Fixture, b *directory.Body) {
			b.IssuedAt = f.Now.Add(directory.ClockSkew + time.Second).Unix()
		}, directory.State{}, directory.ErrNotYetValid},
		{"issued in the future within skew", func(f *dirtest.Fixture, b *directory.Body) {
			b.IssuedAt = f.Now.Add(directory.ClockSkew - time.Second).Unix()
		}, directory.State{}, nil},
		{"lifetime too long", func(f *dirtest.Fixture, b *directory.Body) {
			b.ExpiresAt = b.IssuedAt + int64((directory.MaxListLifetime+time.Second)/time.Second)
		}, directory.State{}, directory.ErrMalformed},
		{"expires before issued", func(f *dirtest.Fixture, b *directory.Body) { b.ExpiresAt = b.IssuedAt }, directory.State{}, directory.ErrMalformed},
		{"default prefix unknown", func(f *dirtest.Fixture, b *directory.Body) { b.DefaultPrefix = "Z" }, directory.State{}, directory.ErrMalformed},
		{"default prefix invalid", func(f *dirtest.Fixture, b *directory.Body) { b.DefaultPrefix = "AB" }, directory.State{}, directory.ErrMalformed},
		{"no nodes", func(f *dirtest.Fixture, b *directory.Body) { b.Nodes, b.DefaultPrefix = nil, "" }, directory.State{}, directory.ErrMalformed},
		{"duplicate prefix", func(f *dirtest.Fixture, b *directory.Body) {
			// 序号、指纹、地址都不同，只有前缀相同，确保拦下它的是前缀检查。
			dup := f.Cert("A", func(n *directory.NodeCert) {
				n.TLSFPs = []string{dirtest.Fingerprint("A-other")}
				n.TCP, n.QUIC = []string{"relay-a2.example.test:8443"}, nil
			})
			b.Nodes = append(b.Nodes, directory.NodeEntry{Cert: f.SignCert(dup), Status: directory.StatusActive, Weight: 100})
		}, directory.State{}, directory.ErrMalformed},
		{"duplicate cert serial", func(f *dirtest.Fixture, b *directory.Body) {
			var c directory.NodeCert
			_ = json.Unmarshal(b.Nodes[0].Cert.Payload, &c)
			dup := f.Cert("B", func(n *directory.NodeCert) { n.Serial = c.Serial })
			b.Nodes = append(b.Nodes, directory.NodeEntry{Cert: f.SignCert(dup), Status: directory.StatusActive, Weight: 100})
		}, directory.State{}, directory.ErrMalformed},
		{"tls fingerprint shared by two nodes", func(f *dirtest.Fixture, b *directory.Body) {
			dup := f.Cert("B", func(n *directory.NodeCert) { n.TLSFPs = []string{dirtest.Fingerprint("A")} })
			b.Nodes = append(b.Nodes, directory.NodeEntry{Cert: f.SignCert(dup), Status: directory.StatusActive, Weight: 100})
		}, directory.State{}, directory.ErrMalformed},
		// 多出来的项都会被丢弃（状态不认识），拦下整份列表的只能是条数上限，不是前缀重复之类的检查：
		// 被丢弃的项也要验签，上限限制的是每份列表的验证开销。
		{"too many nodes", func(f *dirtest.Fixture, b *directory.Body) {
			for len(b.Nodes) <= directory.MaxNodes {
				e := f.Entry("B")
				e.Status = "paused"
				b.Nodes = append(b.Nodes, e)
			}
		}, directory.State{}, directory.ErrMalformed},
		{"max nodes", func(f *dirtest.Fixture, b *directory.Body) {
			for len(b.Nodes) < directory.MaxNodes {
				e := f.Entry("B")
				e.Status = "paused"
				b.Nodes = append(b.Nodes, e)
			}
		}, directory.State{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := dirtest.New(t)
			b := f.Body(f.Entry("A"))
			if tc.mod != nil {
				tc.mod(f, &b)
			}
			_, err := verify(t, f, f.Sign(b), tc.st)
			if tc.sentinel == nil {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			wantErr(t, err, tc.sentinel, "directory")
		})
	}
}

func TestVerifyUnknownPolicyFallsBackToPreferDefault(t *testing.T) {
	f := dirtest.New(t)
	b := f.Body(f.Entry("A"))
	b.Policy = "something-newer"
	if v := mustVerify(t, f, f.Sign(b), directory.State{}); v.Policy != directory.PolicyPreferDefault {
		t.Fatalf("Policy = %q, want %q", v.Policy, directory.PolicyPreferDefault)
	}
}

// 单个节点的问题只丢弃该节点，列表其余部分照常可用。
func TestVerifySkipsBadNodes(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name     string
		entry    func(f *dirtest.Fixture) directory.NodeEntry
		sentinel error
	}{
		{"cert signed by unknown root", func(f *dirtest.Fixture) directory.NodeEntry {
			return directory.NodeEntry{Cert: f.SignCertWith(f.Cert("B", nil), dirtest.SeedKey("evil-root"), directory.SignerPrimary), Status: directory.StatusActive, Weight: 100}
		}, directory.ErrBadSignature},
		{"cert signed by status key", func(f *dirtest.Fixture) directory.NodeEntry {
			return directory.NodeEntry{Cert: f.SignCertWith(f.Cert("B", nil), f.Status, directory.SignerStatus), Status: directory.StatusActive, Weight: 100}
		}, directory.ErrBadSignature},
		{"status key cert replayed as node cert", func(f *dirtest.Fixture) directory.NodeEntry {
			env := f.SignCert(f.Cert("B", nil))
			env.Sig = directory.Sign(f.Primary, directory.KindStatusKey, directory.SignerPrimary, env.Payload).Sig
			return directory.NodeEntry{Cert: env, Status: directory.StatusActive, Weight: 100}
		}, directory.ErrBadSignature},
		{"cert expired", func(f *dirtest.Fixture) directory.NodeEntry {
			c := f.Cert("B", func(c *directory.NodeCert) {
				c.NotBefore = f.Now.Add(-400 * day).Unix()
				c.NotAfter = f.Now.Add(-directory.ClockSkew - time.Second).Unix()
			})
			return directory.NodeEntry{Cert: f.SignCert(c), Status: directory.StatusActive, Weight: 100}
		}, directory.ErrExpired},
		{"cert not yet valid", func(f *dirtest.Fixture) directory.NodeEntry {
			c := f.Cert("B", func(c *directory.NodeCert) { c.NotBefore = f.Now.Add(directory.ClockSkew + time.Second).Unix() })
			return directory.NodeEntry{Cert: f.SignCert(c), Status: directory.StatusActive, Weight: 100}
		}, directory.ErrNotYetValid},
		{"terminal escape in name", func(f *dirtest.Fixture) directory.NodeEntry {
			c := f.Cert("B", func(c *directory.NodeCert) { c.Name = "evil\x1b]0;pwned\x07" })
			return directory.NodeEntry{Cert: f.SignCert(c), Status: directory.StatusActive, Weight: 100}
		}, directory.ErrMalformed},
		{"url instead of host:port", func(f *dirtest.Fixture) directory.NodeEntry {
			c := f.Cert("B", func(c *directory.NodeCert) { c.TCP = []string{"https://relay.example.test:8443"} })
			return directory.NodeEntry{Cert: f.SignCert(c), Status: directory.StatusActive, Weight: 100}
		}, directory.ErrMalformed},
		{"unknown status", func(f *dirtest.Fixture) directory.NodeEntry {
			e := f.Entry("B")
			e.Status = "paused"
			return e
		}, directory.ErrMalformed},
		{"weight above 100", func(f *dirtest.Fixture) directory.NodeEntry {
			e := f.Entry("B")
			e.Weight = 101
			return e
		}, directory.ErrMalformed},
		{"negative weight", func(f *dirtest.Fixture) directory.NodeEntry {
			e := f.Entry("B")
			e.Weight = -1
			return e
		}, directory.ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := dirtest.New(t)
			v := mustVerify(t, f, f.Sign(f.Body(f.Entry("A"), tc.entry(f))), directory.State{})
			skippedOnly(t, v, "B", tc.sentinel)
			if len(v.Nodes) != 1 || v.Nodes[0].Prefix != "A" {
				t.Fatalf("nodes = %+v, want only A", v.Nodes)
			}
		})
	}
}

// signEditedNodes 把 b 编码后交给 edit 改 nodes 数组，再用状态密钥签名。用来构造 NodeEntry 结构体表达不出来的
// 列表项（字段类型不对、根本不是对象）：Fixture.Sign 走结构体编码，做不到。
func signEditedNodes(t *testing.T, f *dirtest.Fixture, b directory.Body, edit func(nodes []any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(dirtest.MustJSON(t, b), &m); err != nil {
		t.Fatal(err)
	}
	edit(m["nodes"].([]any))
	env := directory.Sign(f.Status, directory.KindDirectory, directory.SignerStatus, dirtest.MustJSON(t, m))
	return dirtest.MustJSON(t, env)
}

// 列表项的字段类型不对（例如 weight 写成小数、status 写成数字），或者这一项根本不是对象，也只丢弃这一项，
// 不连累整份列表（设计文档第 4 节）。证书还读得出来时，丢弃记录里带上前缀。
func TestVerifyNodeEntryWrongType(t *testing.T) {
	setB := func(key string, val any) func([]any) {
		return func(nodes []any) { nodes[1].(map[string]any)[key] = val }
	}
	replaceB := func(val any) func([]any) {
		return func(nodes []any) { nodes[1] = val }
	}
	// msg：丢弃原因里要有的字样。证书读不出来时报的是这一项本身的类型错误，而不是读空证书得到的「unexpected end」。
	cases := []struct {
		name   string
		edit   func([]any)
		prefix string
		msg    string
	}{
		{"fractional weight", setB("weight", 12.5), "B", "weight"},
		{"weight as string", setB("weight", "100"), "B", "weight"},
		{"weight overflows int", setB("weight", 1e30), "B", "weight"},
		{"status as number", setB("status", 5), "B", "status"},
		{"cert as string", setB("cert", "B"), "", "cannot unmarshal string"},
		{"entry is a number", replaceB(5), "", "cannot unmarshal number"},
		{"entry is an array", replaceB([]any{}), "", "cannot unmarshal array"},
		{"entry is null", replaceB(nil), "", "no cert"},
		{"entry is an empty object", replaceB(map[string]any{}), "", "no cert"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := dirtest.New(t)
			v := mustVerify(t, f, signEditedNodes(t, f, f.Body(f.Entry("A"), f.Entry("B")), tc.edit), directory.State{})
			if len(v.Nodes) != 1 || v.Nodes[0].Prefix != "A" {
				t.Fatalf("nodes = %+v, want only A", v.Nodes)
			}
			if len(v.Skipped) != 1 || v.Skipped[0].Prefix != tc.prefix || !errors.Is(v.Skipped[0].Err, directory.ErrMalformed) {
				t.Fatalf("skipped = %+v, want one malformed entry with prefix %q", v.Skipped, tc.prefix)
			}
			if !strings.Contains(v.Skipped[0].Err.Error(), tc.msg) {
				t.Fatalf("skip reason %q does not mention %q", v.Skipped[0].Err, tc.msg)
			}
		})
	}
	t.Run("default node", func(t *testing.T) {
		// 同 TestVerifyDefaultNodeSkipped：默认节点被丢弃时列表仍然有效，只是 DefaultNode 返回 nil。
		f := dirtest.New(t)
		edit := func(nodes []any) { nodes[0].(map[string]any)["weight"] = 12.5 }
		v := mustVerify(t, f, signEditedNodes(t, f, f.Body(f.Entry("A"), f.Entry("B")), edit), directory.State{})
		if v.DefaultPrefix != "A" || v.DefaultNode() != nil {
			t.Fatalf("DefaultPrefix %q DefaultNode %+v", v.DefaultPrefix, v.DefaultNode())
		}
		skippedOnly(t, v, "A", directory.ErrMalformed)
	})
	// 默认节点那一项坏到读不出前缀（不是对象），或者证书正文本身类型不对，同样只丢弃它：不能因为
	// 「default_prefix 没列出」拒掉整份列表。证书正文读得出前缀的，丢弃记录照样带上。
	badCert := func(f *dirtest.Fixture) any {
		var m map[string]any
		if err := json.Unmarshal(dirtest.MustJSON(t, f.Cert("A", nil)), &m); err != nil {
			t.Fatal(err)
		}
		m["official"] = "yes"
		return directory.Sign(f.Primary, directory.KindNodeCert, directory.SignerPrimary, dirtest.MustJSON(t, m))
	}
	for _, tc := range []struct {
		name   string
		edit   func(f *dirtest.Fixture, nodes []any)
		prefix string
		msg    string
	}{
		{"default entry is a number", func(_ *dirtest.Fixture, nodes []any) { nodes[0] = 5 }, "", "cannot unmarshal number"},
		{"default entry is null", func(_ *dirtest.Fixture, nodes []any) { nodes[0] = nil }, "", "no cert"},
		{"default entry is an array", func(_ *dirtest.Fixture, nodes []any) { nodes[0] = []any{} }, "", "cannot unmarshal array"},
		{"default cert payload type error", func(f *dirtest.Fixture, nodes []any) { nodes[0].(map[string]any)["cert"] = badCert(f) }, "A", "official"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := dirtest.New(t)
			v := mustVerify(t, f, signEditedNodes(t, f, f.Body(f.Entry("A"), f.Entry("B")), func(nodes []any) { tc.edit(f, nodes) }), directory.State{})
			if v.DefaultPrefix != "A" || v.DefaultNode() != nil || len(v.Nodes) != 1 || v.Nodes[0].Prefix != "B" {
				t.Fatalf("DefaultPrefix %q DefaultNode %+v nodes %+v, want only B", v.DefaultPrefix, v.DefaultNode(), v.Nodes)
			}
			if len(v.Skipped) != 1 || v.Skipped[0].Prefix != tc.prefix || !errors.Is(v.Skipped[0].Err, directory.ErrMalformed) {
				t.Fatalf("skipped = %+v, want one malformed entry with prefix %q", v.Skipped, tc.prefix)
			}
			if !strings.Contains(v.Skipped[0].Err.Error(), tc.msg) {
				t.Fatalf("skip reason %q does not mention %q", v.Skipped[0].Err, tc.msg)
			}
		})
	}
	// 读不出前缀的项可能就是默认节点，所以放过 default_prefix 核对；但所有项都读得出前缀时照样核对。
	t.Run("default prefix still checked", func(t *testing.T) {
		f := dirtest.New(t)
		b := f.Body(f.Entry("A"), f.Entry("B"))
		b.DefaultPrefix = "C"
		_, err := verify(t, f, f.Sign(b), directory.State{})
		wantErr(t, err, directory.ErrMalformed, directory.LayerDirectory)
		// 有被丢弃的项、但它读得出前缀（这里 B 的 weight 类型不对）：仍然知道 C 不在列表里。
		raw := signEditedNodes(t, f, b, func(nodes []any) { nodes[1].(map[string]any)["weight"] = 12.5 })
		_, err = verify(t, f, raw, directory.State{})
		wantErr(t, err, directory.ErrMalformed, directory.LayerDirectory)
	})
}

// 默认节点自己被丢弃（例如证书过期）时，列表仍然有效，只是 DefaultNode 返回 nil。
func TestVerifyDefaultNodeSkipped(t *testing.T) {
	f := dirtest.New(t)
	expired := f.Cert("A", func(c *directory.NodeCert) {
		c.NotBefore = f.Now.Add(-400 * 24 * time.Hour).Unix()
		c.NotAfter = f.Now.Add(-48 * time.Hour).Unix()
	})
	b := f.Body(directory.NodeEntry{Cert: f.SignCert(expired), Status: directory.StatusActive, Weight: 100}, f.Entry("B"))
	v := mustVerify(t, f, f.Sign(b), directory.State{})
	if v.DefaultPrefix != "A" || v.DefaultNode() != nil {
		t.Fatalf("DefaultPrefix %q DefaultNode %+v", v.DefaultPrefix, v.DefaultNode())
	}
	skippedOnly(t, v, "A", directory.ErrExpired)
}

func nodeSerial(t *testing.T, e directory.NodeEntry) uint64 {
	t.Helper()
	var c directory.NodeCert
	if err := json.Unmarshal(e.Cert.Payload, &c); err != nil {
		t.Fatal(err)
	}
	return c.Serial
}

func TestVerifyNodeRevocation(t *testing.T) {
	t.Run("in this list", func(t *testing.T) {
		f := dirtest.New(t)
		a, b := f.Entry("A"), f.Entry("B")
		body := f.Body(a, b)
		body.Revocations = f.Revocations(3, []uint64{nodeSerial(t, b)}, nil, f.Primary, directory.SignerPrimary)
		v := mustVerify(t, f, f.Sign(body), directory.State{})
		skippedOnly(t, v, "B", directory.ErrRevoked)
		want := seen(42)
		want.Revocations = []directory.RevocationRecord{{Root: primaryFP, NodeSerials: []uint64{nodeSerial(t, b)}}}
		if !slices.Equal(v.RevokedNodes, []uint64{nodeSerial(t, b)}) || !stateEqual(v.NewState, want) {
			t.Fatalf("RevokedNodes %v, NewState %+v", v.RevokedNodes, v.NewState)
		}
	})
	t.Run("remembered from state", func(t *testing.T) {
		// 新列表不再带吊销（或带了更早的版本），已持久化的吊销仍然生效。
		f := dirtest.New(t)
		a, b := f.Entry("A"), f.Entry("B")
		body := f.Body(a, b)
		body.Revocations = f.Revocations(3, []uint64{nodeSerial(t, b)}, nil, f.Primary, directory.SignerPrimary)
		st := mustVerify(t, f, f.Sign(body), directory.State{}).NewState
		v := mustVerify(t, f, f.Sign(f.Body(a, b)), st)
		skippedOnly(t, v, "B", directory.ErrRevoked)
		if !stateEqual(v.NewState, st) {
			t.Fatalf("NewState lost persisted revocations: %+v", v.NewState)
		}
	})
	t.Run("builtin", func(t *testing.T) {
		f := dirtest.New(t)
		b := f.Entry("B")
		f.Roots.RevokedNodes = []uint64{nodeSerial(t, b)}
		v := mustVerify(t, f, f.Sign(f.Body(f.Entry("A"), b)), directory.State{})
		skippedOnly(t, v, "B", directory.ErrRevoked)
	})
	t.Run("union with state", func(t *testing.T) {
		// 吊销列表的序号不参与判断：哪怕它比以前见过的都旧，内容也照样并进同一把根的记录。
		f := dirtest.New(t)
		a, b := f.Entry("A"), f.Entry("B")
		body := f.Body(a, b)
		body.Revocations = f.Revocations(1, []uint64{nodeSerial(t, b)}, nil, f.Primary, directory.SignerPrimary)
		st := revokedBy(primaryFP, []uint64{7})
		v := mustVerify(t, f, f.Sign(body), st)
		want := []uint64{7, nodeSerial(t, b)}
		if got := v.NewState.Revocations; len(got) != 1 || got[0].Root != primaryFP || !slices.Equal(got[0].NodeSerials, want) ||
			!slices.Equal(v.RevokedNodes, want) {
			t.Fatalf("RevokedNodes %v, NewState %+v, want %v from the primary root", v.RevokedNodes, v.NewState, want)
		}
		if !slices.Equal(st.Revocations[0].NodeSerials, []uint64{7}) {
			t.Fatalf("Verify mutated the input state: %+v", st)
		}
	})
	t.Run("capped per root", func(t *testing.T) {
		// 同一把根的节点吊销取并集后不能超过上限：分多份列表也灌不爆本地缓存。另一把根不受影响。
		f := dirtest.New(t)
		serials := func(base uint64) []uint64 {
			out := make([]uint64, directory.MaxRevokedNodes)
			for i := range out {
				out[i] = base + uint64(i)
			}
			return out
		}
		list := func(root ed25519.PrivateKey, signer string, base uint64) []byte {
			b := f.Body(f.Entry("A"))
			b.Revocations = f.Revocations(1, serials(base), nil, root, signer)
			return f.Sign(b)
		}
		st := mustVerify(t, f, list(f.Primary, directory.SignerPrimary, 1<<40), directory.State{}).NewState
		_, err := verify(t, f, list(f.Primary, directory.SignerPrimary, 1<<41), st)
		wantErr(t, err, directory.ErrMalformed, "revocations")
		v := mustVerify(t, f, list(f.Backup, directory.SignerBackup, 1<<42), st)
		if len(v.RevokedNodes) != 2*directory.MaxRevokedNodes {
			t.Fatalf("%d revoked nodes, want both roots' %d", len(v.RevokedNodes), directory.MaxRevokedNodes)
		}
	})
}

func TestVerifyRevocationsEnvelope(t *testing.T) {
	cases := []struct {
		name     string
		rev      func(f *dirtest.Fixture) *directory.Envelope
		st       directory.State
		sentinel error
	}{
		{"zero serial", func(f *dirtest.Fixture) *directory.Envelope {
			return f.Revocations(0, nil, nil, f.Primary, directory.SignerPrimary)
		}, directory.State{}, directory.ErrMalformed},
		{"signed by unknown root", func(f *dirtest.Fixture) *directory.Envelope {
			return f.Revocations(1, []uint64{1}, nil, dirtest.SeedKey("evil-root"), directory.SignerPrimary)
		}, directory.State{}, directory.ErrBadSignature},
		{"signed by status key", func(f *dirtest.Fixture) *directory.Envelope {
			return f.Revocations(1, []uint64{1}, nil, f.Status, directory.SignerStatus)
		}, directory.State{}, directory.ErrBadSignature},
		{"root revokes itself", func(f *dirtest.Fixture) *directory.Envelope {
			return f.Revocations(1, nil, []string{directory.SignerBackup}, f.Backup, directory.SignerBackup)
		}, directory.State{}, directory.ErrMalformed},
		{"root given by name, not fingerprint", func(f *dirtest.Fixture) *directory.Envelope {
			return f.SignRevocations(directory.Revocations{Serial: 1, Roots: []string{directory.SignerPrimary}}, f.Backup, directory.SignerBackup)
		}, directory.State{}, directory.ErrMalformed},
		{"primary revokes backup", func(f *dirtest.Fixture) *directory.Envelope {
			return f.Revocations(1, nil, []string{directory.SignerBackup}, f.Primary, directory.SignerPrimary)
		}, directory.State{}, directory.ErrMalformed},
		{"primary revokes an unrelated key", func(f *dirtest.Fixture) *directory.Envelope {
			return f.Revocations(1, nil, []string{dirtest.Fingerprint("other-root")}, f.Primary, directory.SignerPrimary)
		}, directory.State{}, directory.ErrMalformed},
		{"too many node serials", func(f *dirtest.Fixture) *directory.Envelope {
			nodes := make([]uint64, directory.MaxRevokedNodes+1)
			for i := range nodes {
				nodes[i] = uint64(i + 1)
			}
			return f.Revocations(1, nodes, nil, f.Primary, directory.SignerPrimary)
		}, directory.State{}, directory.ErrMalformed},
		{"too many roots", func(f *dirtest.Fixture) *directory.Envelope {
			var roots []string
			for i := range directory.MaxRevokedRoots + 1 {
				roots = append(roots, dirtest.Fingerprint(fmt.Sprint("root-", i)))
			}
			return f.Revocations(1, nil, roots, f.Backup, directory.SignerBackup)
		}, directory.State{}, directory.ErrMalformed},
		{"signed by a root already revoked", func(f *dirtest.Fixture) *directory.Envelope {
			return f.Revocations(2, []uint64{1}, nil, f.Primary, directory.SignerPrimary)
		}, revokedBy(backupFP, nil, primaryFP), directory.ErrRevoked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := dirtest.New(t)
			b := f.Body(f.Entry("A"))
			b.Revocations = tc.rev(f)
			_, err := verify(t, f, f.Sign(b), tc.st)
			wantErr(t, err, tc.sentinel, "revocations")
		})
	}
}

// 主根泄露的处置：备用根签发吊销主根。之后主根签的状态密钥授权与节点证书全部失效，并被持久记住。
func TestVerifyRootRevocation(t *testing.T) {
	revokePrimary := func(f *dirtest.Fixture) *directory.Envelope {
		return f.Revocations(1, nil, []string{directory.SignerPrimary}, f.Backup, directory.SignerBackup)
	}

	t.Run("status key from revoked root", func(t *testing.T) {
		f := dirtest.New(t)
		b := f.Body(f.Entry("A"))
		b.Revocations = revokePrimary(f)
		_, err := verify(t, f, f.Sign(b), directory.State{})
		wantErr(t, err, directory.ErrRevoked, "status_key")
	})
	t.Run("node certs from revoked root are skipped", func(t *testing.T) {
		f := dirtest.New(t)
		good := directory.NodeEntry{Cert: f.SignCertWith(f.Cert("A", nil), f.Backup, directory.SignerBackup), Status: directory.StatusActive, Weight: 100}
		b := f.Body(good, f.Entry("B")) // B 由主根签发
		b.StatusKey = f.IssueStatus(2, f.Status, f.Backup, directory.SignerBackup)
		b.Revocations = revokePrimary(f)
		v := mustVerify(t, f, f.Sign(b), directory.State{})
		skippedOnly(t, v, "B", directory.ErrRevoked)
		if !slices.Equal(v.RevokedRoots, []string{primaryFP}) || v.NewState.StatusRoot != backupFP {
			t.Fatalf("RevokedRoots %v, NewState %+v", v.RevokedRoots, v.NewState)
		}
	})
	t.Run("remembered from state", func(t *testing.T) {
		f := dirtest.New(t) // 状态密钥由主根授权
		_, err := verify(t, f, f.Sign(f.Body(f.Entry("A"))), revokedBy(backupFP, nil, primaryFP))
		wantErr(t, err, directory.ErrRevoked, "status_key")
	})
	t.Run("kept when a later revocation list omits it", func(t *testing.T) {
		// 列表带的吊销只列了节点、没列主根（例如在另一台没有最新台账的机器上签的）：
		// 已记住的根吊销仍要留在新状态里，不能被这份吊销「覆盖」掉。
		f := dirtest.New(t)
		a := directory.NodeEntry{Cert: f.SignCertWith(f.Cert("A", nil), f.Backup, directory.SignerBackup), Status: directory.StatusActive, Weight: 100}
		b := f.Body(a)
		b.StatusKey = f.IssueStatus(2, f.Status, f.Backup, directory.SignerBackup)
		b.Revocations = f.Revocations(2, []uint64{99}, nil, f.Backup, directory.SignerBackup)
		v := mustVerify(t, f, f.Sign(b), revokedBy(backupFP, nil, primaryFP))
		want := revokedBy(backupFP, []uint64{99}, primaryFP).Revocations
		if !slices.Equal(v.RevokedRoots, []string{primaryFP}) || !slices.Equal(v.RevokedNodes, []uint64{99}) ||
			!stateEqual(directory.State{Revocations: v.NewState.Revocations}, directory.State{Revocations: want}) {
			t.Fatalf("RevokedRoots %v, RevokedNodes %v, NewState %+v; want primary still revoked plus node 99", v.RevokedRoots, v.RevokedNodes, v.NewState)
		}
	})
	t.Run("builtin", func(t *testing.T) {
		f := dirtest.New(t)
		f.Roots.RevokedRoots = []string{primaryFP}
		_, err := verify(t, f, f.Sign(f.Body(f.Entry("A"))), directory.State{})
		wantErr(t, err, directory.ErrRevoked, "status_key")
	})
	t.Run("replaced primary key is not covered", func(t *testing.T) {
		// 吊销认的是公钥，不是「primary」这个名字：发版换上新的主根公钥后，旧主根的吊销不波及它。
		f := dirtest.New(t)
		newPrimary := dirtest.SeedKey("primary-2")
		f.Roots.Keys[directory.SignerPrimary] = newPrimary.Public().(ed25519.PublicKey)
		b := f.Body(directory.NodeEntry{Cert: f.SignCertWith(f.Cert("A", nil), newPrimary, directory.SignerPrimary), Status: directory.StatusActive, Weight: 100})
		b.StatusKey = f.IssueStatus(2, f.Status, newPrimary, directory.SignerPrimary)
		v := mustVerify(t, f, f.Sign(b), revokedBy(backupFP, nil, primaryFP))
		if len(v.Nodes) != 1 {
			t.Fatalf("nodes %+v skipped %+v", v.Nodes, v.Skipped)
		}
	})
	t.Run("status floor kept when a later revocation list omits it", func(t *testing.T) {
		// 同一把根后来签的吊销列表没带序号下限：已记住的下限不能因合并而降低。
		f := dirtest.New(t)
		a := directory.NodeEntry{Cert: f.SignCertWith(f.Cert("A", nil), f.Backup, directory.SignerBackup), Status: directory.StatusActive, Weight: 100}
		st := directory.State{Revocations: []directory.RevocationRecord{{Root: backupFP, Roots: []string{primaryFP}, MinStatusSerial: 50}}}
		b := f.Body(a)
		b.StatusKey = f.IssueStatus(2, f.Status, f.Backup, directory.SignerBackup)
		b.Revocations = revokePrimary(f)
		_, err := verify(t, f, f.Sign(b), st)
		wantErr(t, err, directory.ErrRollback, "status_key")
	})
	t.Run("revocations signed by a revoked root no longer count", func(t *testing.T) {
		// 主根被吊销后，它签过的节点吊销和状态密钥序号下限作废：泄露的主根灌进来的东西卡不住恢复。
		f := dirtest.New(t)
		a := directory.NodeEntry{Cert: f.SignCertWith(f.Cert("A", nil), f.Backup, directory.SignerBackup), Status: directory.StatusActive, Weight: 100}
		st := directory.State{Revocations: []directory.RevocationRecord{{Root: primaryFP, NodeSerials: []uint64{nodeSerial(t, a)}, MinStatusSerial: 1 << 62}}}
		b := f.Body(a)
		b.StatusKey = f.IssueStatus(2, f.Status, f.Backup, directory.SignerBackup)
		_, err := verify(t, f, f.Sign(b), st)
		wantErr(t, err, directory.ErrRollback, "status_key") // 主根没被吊销时，它声明的下限照样生效
		b.Revocations = revokePrimary(f)
		v := mustVerify(t, f, f.Sign(b), st)
		if len(v.Nodes) != 1 || len(v.RevokedNodes) != 0 {
			t.Fatalf("nodes %d, revoked nodes %v; want the primary's revocations void", len(v.Nodes), v.RevokedNodes)
		}
	})
}

func TestVerifyErrorMessageNamesLayer(t *testing.T) {
	f := dirtest.New(t)
	_, err := verify(t, f, f.Sign(f.Body(f.Entry("A"))), seen(99))
	if err == nil || !strings.Contains(err.Error(), "directory") || !strings.Contains(err.Error(), "99") {
		t.Fatalf("error %v should name the layer and the seen serial", err)
	}
}
