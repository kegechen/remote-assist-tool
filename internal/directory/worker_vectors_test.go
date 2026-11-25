package directory_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
)

// Go 与 CF Worker（Worker 项目的 relay-directory/）交叉验签用的固定向量（spec 第 11 节）。
//
// testdata/worker-vectors.json 由本测试按确定性的测试密钥生成（-update-worker-vectors 重新生成），Worker 仓库的
// relay-directory/test/vectors.json 是它的原样拷贝：Worker 的测试要验得过这里的根签名对象、拒收这里的坏向量，
// 并且用同样的输入签出与 directory.signed 逐字节相同的列表（Ed25519 签名是确定性的，正文全是 ASCII，
// JS 的 JSON.stringify 与 encoding/json 输出一致）。这里改了向量，要把文件拷过去再跑 Worker 的测试。
// 里面的密钥只是测试用的，地址都是文档保留地址。
var updateWorkerVectors = flag.Bool("update-worker-vectors", false, "regenerate testdata/worker-vectors.json")

const workerVectorsPath = "testdata/worker-vectors.json"

type workerVectors struct {
	Comment      string               `json:"comment"`
	Now          int64                `json:"now"`
	Roots        map[string]string    `json:"roots"`          // 签名者名字 → 根公钥 base64
	StatusKeyJWK map[string]string    `json:"status_key_jwk"` // 同 relay-admin status-key new 写出的 .secret.jwk
	StatusKey    directory.Envelope   `json:"status_key"`
	Revocations  directory.Envelope   `json:"revocations"`
	NodeCerts    []directory.Envelope `json:"node_certs"`
	Directory    vectorDirectory      `json:"directory"`
	Bad          []badVector          `json:"bad"`
}

type vectorDirectory struct {
	Serial        uint64            `json:"serial"`
	IssuedAt      int64             `json:"issued_at"`
	ExpiresAt     int64             `json:"expires_at"`
	Policy        string            `json:"policy"`
	DefaultPrefix string            `json:"default_prefix"`
	Nodes         []vectorNode      `json:"nodes"`   // 按这个顺序放 node_certs 里同前缀的证书
	Signed        string            `json:"signed"`  // SignDirectory 的输出原文，Worker 必须签出一模一样的
	Usable        []string          `json:"usable"`  // Verify 后可用的节点前缀
	Skipped       map[string]string `json:"skipped"` // 被丢弃的节点前缀 → 原因（见 badVector.Expect）
}

type vectorNode struct {
	Prefix string `json:"prefix"`
	Status string `json:"status"`
	Weight int    `json:"weight"`
}

// badVector 是两边都必须拒收的对象。Raw 是信封原文（含严格解析要挡的未知字段、尾随内容）。
type badVector struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`   // 冒充的对象种类
	Raw    string `json:"raw"`    //
	Expect string `json:"expect"` // bad-signature、malformed、revoked
}

var expectErr = map[string]error{
	"bad-signature": directory.ErrBadSignature,
	"malformed":     directory.ErrMalformed,
	"revoked":       directory.ErrRevoked,
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func withBOM(b []byte) []byte { return append([]byte{0xef, 0xbb, 0xbf}, b...) }

func envJSON(t *testing.T, env directory.Envelope) string {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// buildWorkerVectors 生成向量。全部由 dirtest 的确定性密钥和固定时间决定，重复生成逐字节相同。
func buildWorkerVectors(t *testing.T) ([]byte, workerVectors) {
	t.Helper()
	f := dirtest.New(t)
	now := f.Now
	pub := func(k ed25519.PrivateKey) []byte { return k.Public().(ed25519.PublicKey) }

	certA := f.Cert("A", func(c *directory.NodeCert) {
		c.Name, c.Operator, c.Region = "overseas-1", "official", "overseas"
		c.TCP, c.QUIC = []string{"192.0.2.10:8443"}, []string{"192.0.2.10:8443"}
	})
	certB := f.Cert("B", func(c *directory.NodeCert) {
		c.Name, c.Operator, c.Official, c.Region = "节点B·cn", "Example-Ops", false, "cn"
		c.TCP, c.QUIC = []string{"relay-b.example.test:8443", "[2001:db8::1]:8443"}, nil
		c.TLSFPs = []string{dirtest.Fingerprint("B"), dirtest.Fingerprint("B-next")}
	})
	certC := f.Cert("C", nil)
	envA, envB := f.SignCert(certA), f.SignCertWith(certB, f.Backup, directory.SignerBackup)
	envC := f.SignCert(certC)
	rev := f.Revocations(1, []uint64{certC.Serial}, nil, f.Primary, directory.SignerPrimary)

	in := vectorDirectory{
		Serial:        7,
		IssuedAt:      now.Unix(),
		ExpiresAt:     now.Add(30 * 24 * time.Hour).Unix(),
		Policy:        directory.PolicyPreferDefault,
		DefaultPrefix: "A",
		Nodes: []vectorNode{
			{"A", directory.StatusActive, 100},
			{"B", directory.StatusDraining, 50},
			{"C", directory.StatusDisabled, 0},
		},
		Usable:  []string{"A", "B"},
		Skipped: map[string]string{"C": "revoked"},
	}
	body := directory.Body{
		Format: directory.FormatVersion, Serial: in.Serial, IssuedAt: in.IssuedAt, ExpiresAt: in.ExpiresAt,
		Policy: in.Policy, DefaultPrefix: in.DefaultPrefix, StatusKey: f.StatusCert, Revocations: rev,
		Nodes: []directory.NodeEntry{
			{Cert: envA, Status: in.Nodes[0].Status, Weight: in.Nodes[0].Weight},
			{Cert: envB, Status: in.Nodes[1].Status, Weight: in.Nodes[1].Weight},
			{Cert: envC, Status: in.Nodes[2].Status, Weight: in.Nodes[2].Weight},
		},
	}
	in.Signed = string(f.Sign(body))

	// 坏向量。节点证书类的都是 A 的变体：放进列表后只有这一项被丢弃。
	payloadA := dirtest.MustJSON(t, certA)
	tampered := bytes.Replace(payloadA, []byte("overseas-1"), []byte("overseas-2"), 1)
	signedCert := func(c directory.NodeCert) string { return envJSON(t, f.SignCert(c)) }
	modA := func(mod func(*directory.NodeCert)) directory.NodeCert {
		c := certA
		c.TCP, c.QUIC, c.TLSFPs = slices.Clone(c.TCP), slices.Clone(c.QUIC), slices.Clone(c.TLSFPs)
		mod(&c)
		return c
	}
	crossKind := directory.Sign(f.Primary, directory.KindStatusKey, directory.SignerPrimary, payloadA)
	crossKind.Kind = directory.KindNodeCert
	shortKey := directory.Sign(f.Primary, directory.KindStatusKey, directory.SignerPrimary, dirtest.MustJSON(t, directory.StatusKeyCert{
		Serial: 2, PubKey: pub(f.Status)[:31], NotBefore: now.Add(-time.Hour).Unix(), NotAfter: now.Add(time.Hour).Unix(),
	}))
	dirEnv := map[string]any{}
	if err := json.Unmarshal([]byte(in.Signed), &dirEnv); err != nil {
		t.Fatal(err)
	}
	dirEnv["comment"] = "not signed"
	withUnknown, _ := json.Marshal(dirEnv)

	bad := []badVector{
		{"node-cert payload changed after signing", "node-cert", envJSON(t, directory.Envelope{
			Kind: directory.KindNodeCert, Payload: tampered, Sig: envA.Sig, Signer: envA.Signer}), "bad-signature"},
		{"node-cert signed as a status-key (domain separation)", "node-cert", envJSON(t, crossKind), "bad-signature"},
		{"node-cert signed by the status key", "node-cert", envJSON(t, directory.Sign(f.Status, directory.KindNodeCert, directory.SignerStatus, payloadA)), "bad-signature"},
		{"node-cert signed by an unknown key named primary", "node-cert", envJSON(t, directory.Sign(dirtest.SeedKey("intruder"), directory.KindNodeCert, directory.SignerPrimary, payloadA)), "bad-signature"},
		{"node-cert signed by backup but labeled primary", "node-cert", envJSON(t, directory.Sign(f.Backup, directory.KindNodeCert, directory.SignerPrimary, payloadA)), "bad-signature"},
		{"node-cert fingerprint in uppercase hex", "node-cert", signedCert(modA(func(c *directory.NodeCert) {
			c.TLSFPs = []string{"ABCDEF" + dirtest.Fingerprint("A")[6:]}
		})), "malformed"},
		{"node-cert port with a leading zero", "node-cert", signedCert(modA(func(c *directory.NodeCert) { c.TCP = []string{"192.0.2.10:08443"} })), "malformed"},
		{"node-cert address with a path", "node-cert", signedCert(modA(func(c *directory.NodeCert) { c.TCP = []string{"192.0.2.10:8443/x"} })), "malformed"},
		{"node-cert name with a zero-width space", "node-cert", signedCert(modA(func(c *directory.NodeCert) { c.Name = "overseas​1" })), "malformed"},
		{"node-cert name with a space", "node-cert", signedCert(modA(func(c *directory.NodeCert) { c.Name = "overseas 1" })), "malformed"},
		{"node-cert name with mathematical bold letters", "node-cert", signedCert(modA(func(c *directory.NodeCert) { c.Name = "\U0001D400-1" })), "malformed"},
		{"node-cert prefix not a code character", "node-cert", signedCert(modA(func(c *directory.NodeCert) { c.Prefix = "I" })), "malformed"},
		{"node-cert not_after before not_before", "node-cert", signedCert(modA(func(c *directory.NodeCert) { c.NotAfter = c.NotBefore - 1 })), "malformed"},
		{"node-cert serial 0", "node-cert", signedCert(modA(func(c *directory.NodeCert) { c.Serial = 0 })), "malformed"},
		{"revocations by primary revoking a root", "revocations", envJSON(t, *f.Revocations(2, nil, []string{directory.SignerBackup}, f.Primary, directory.SignerPrimary)), "malformed"},
		{"revocations by backup revoking itself", "revocations", envJSON(t, *f.Revocations(2, nil, []string{directory.SignerBackup}, f.Backup, directory.SignerBackup)), "malformed"},
		{"revocations with node serial 0", "revocations", envJSON(t, *f.Revocations(2, []uint64{0}, nil, f.Primary, directory.SignerPrimary)), "malformed"},
		{"status-key with a 31-byte public key", "status-key", envJSON(t, shortKey), "malformed"},
		// encoding/json 不认 UTF-8 BOM；JS 的 TextDecoder 默认会吞掉它，Worker 必须同样拒收。
		{"status-key payload starting with a UTF-8 BOM", "status-key", envJSON(t, directory.Sign(f.Primary, directory.KindStatusKey, directory.SignerPrimary, withBOM(f.StatusCert.Payload))), "malformed"},
		{"node-cert payload starting with a UTF-8 BOM", "node-cert", envJSON(t, directory.Sign(f.Primary, directory.KindNodeCert, directory.SignerPrimary, withBOM(payloadA))), "malformed"},
		{"directory envelope with an unknown field", "directory", string(withUnknown), "malformed"},
		{"directory envelope with trailing data", "directory", in.Signed + `{}`, "malformed"},
	}

	v := workerVectors{
		Comment: "Cross-implementation test vectors for internal/directory (Go) and the relay-directory Worker. " +
			"Generated by internal/directory/worker_vectors_test.go; test keys only.",
		Now: now.Unix(),
		Roots: map[string]string{
			directory.SignerPrimary: base64.StdEncoding.EncodeToString(pub(f.Primary)),
			directory.SignerBackup:  base64.StdEncoding.EncodeToString(pub(f.Backup)),
		},
		StatusKeyJWK: map[string]string{"kty": "OKP", "crv": "Ed25519", "d": b64url(f.Status.Seed()), "x": b64url(pub(f.Status))},
		StatusKey:    f.StatusCert,
		Revocations:  *rev,
		NodeCerts:    []directory.Envelope{envA, envB, envC},
		Directory:    in,
		Bad:          bad,
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n'), v
}

func TestWorkerVectors(t *testing.T) {
	want, v := buildWorkerVectors(t)
	if *updateWorkerVectors {
		if err := os.MkdirAll(filepath.Dir(workerVectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(workerVectorsPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(workerVectorsPath)
	if err != nil {
		t.Fatalf("%v（用 -update-worker-vectors 生成）", err)
	}
	// 签出时 git 可能把换行改成 CRLF；内容按 LF 比。
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatalf("%s 与生成结果不一致：改了生成逻辑就用 -update-worker-vectors 重新生成，并拷到 Worker 仓库", workerVectorsPath)
	}

	f := dirtest.New(t)
	verified, err := directory.Verify([]byte(v.Directory.Signed), f.Roots, directory.State{}, time.Unix(v.Now, 0))
	if err != nil {
		t.Fatalf("Verify(directory.signed): %v", err)
	}
	var usable []string
	for _, n := range verified.Nodes {
		usable = append(usable, n.Prefix)
	}
	if !slices.Equal(usable, v.Directory.Usable) {
		t.Fatalf("usable nodes = %v, want %v (skipped %+v)", usable, v.Directory.Usable, verified.Skipped)
	}
	if len(verified.Skipped) != len(v.Directory.Skipped) {
		t.Fatalf("skipped = %+v, want %v", verified.Skipped, v.Directory.Skipped)
	}
	for _, s := range verified.Skipped {
		if !errors.Is(s.Err, expectErr[v.Directory.Skipped[s.Prefix]]) {
			t.Fatalf("skipped %s: %v, want %s", s.Prefix, s.Err, v.Directory.Skipped[s.Prefix])
		}
	}

	for _, b := range v.Bad {
		t.Run(b.Name, func(t *testing.T) { checkBadVector(t, f, v, b) })
	}
}

// checkBadVector 把坏向量放进一份其余都合法的列表里，确认 Go 的验证端按 Expect 拒收（节点证书只丢弃这一项）。
func checkBadVector(t *testing.T, f *dirtest.Fixture, v workerVectors, b badVector) {
	t.Helper()
	want, ok := expectErr[b.Expect]
	if !ok {
		t.Fatalf("unknown expect %q", b.Expect)
	}
	now := time.Unix(v.Now, 0)
	if b.Kind == "directory" {
		_, err := directory.Verify([]byte(b.Raw), f.Roots, directory.State{}, now)
		wantErr(t, err, want, directory.LayerDirectory)
		return
	}
	var env directory.Envelope
	if err := json.Unmarshal([]byte(b.Raw), &env); err != nil {
		t.Fatalf("raw: %v", err)
	}
	body := directory.Body{
		Format: directory.FormatVersion, Serial: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(24 * time.Hour).Unix(),
		Policy: directory.PolicyPreferDefault, DefaultPrefix: "A", StatusKey: v.StatusKey,
		Nodes: []directory.NodeEntry{{Cert: v.NodeCerts[0], Status: directory.StatusActive, Weight: 100}},
	}
	switch b.Kind {
	case "node-cert":
		body.Nodes = append(body.Nodes, directory.NodeEntry{Cert: env, Status: directory.StatusActive, Weight: 100})
		verified, err := directory.Verify(f.Sign(body), f.Roots, directory.State{}, now)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if len(verified.Skipped) != 1 || !errors.Is(verified.Skipped[0].Err, want) {
			t.Fatalf("skipped = %+v, want one node dropped with %v", verified.Skipped, want)
		}
	case "revocations":
		body.Revocations = &env
		_, err := directory.Verify(f.Sign(body), f.Roots, directory.State{}, now)
		wantErr(t, err, want, directory.LayerRevocations)
	case "status-key":
		body.StatusKey = env
		_, err := directory.Verify(f.Sign(body), f.Roots, directory.State{}, now)
		wantErr(t, err, want, directory.LayerStatusKey)
	default:
		t.Fatalf("unknown kind %q", b.Kind)
	}
}
