package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/directory"
)

// nodeRequestCerts 在 dir 里生成一对自签证书（server.crt / server.key），返回两个路径。
func nodeRequestCerts(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	certPath, keyPath = filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := crypto.GenerateSelfSignedCert(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func nodeRequestFingerprint(t *testing.T, certPath, keyPath string) string {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return directory.CertFingerprint(pair.Certificate[0])
}

func execNodeRequest(args ...string) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	code = runNodeRequest(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// 申请文件能被 relay-admin sign-node 同一套校验接受，内容与参数一致，并且不含私钥。
func TestNodeRequestWritesVerifiableRequest(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := nodeRequestCerts(t, dir)
	out := filepath.Join(dir, "request.json")

	stdout, stderr, code := execNodeRequest(
		"--cert", certPath, "--key", keyPath,
		"--name", "example-node", "--operator", "official", "--region", "overseas",
		"--tcp", "relay.example.test:8443", "--quic", "relay.example.test:8443",
		"--out", out)
	if code != 0 {
		t.Fatalf("code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	v, err := directory.VerifyNodeRequest(raw)
	if err != nil {
		t.Fatalf("申请验不过: %v", err)
	}
	fp := nodeRequestFingerprint(t, certPath, keyPath)
	if v.Fingerprint != fp || v.PrevFingerprint != "" {
		t.Fatalf("证书指纹 = %q（换证书指纹 %q），应为 %q 且无换证书", v.Fingerprint, v.PrevFingerprint, fp)
	}
	if v.Name != "example-node" || v.Operator != "official" || v.Region != "overseas" ||
		strings.Join(v.TCP, ",") != "relay.example.test:8443" || strings.Join(v.QUIC, ",") != "relay.example.test:8443" {
		t.Fatalf("申请内容与参数不一致: %+v", v.NodeRequest)
	}
	if v.CreatedAt == 0 {
		t.Fatal("申请没有记录生成时间")
	}
	if problems := requestLeaks(raw, readKeyDER(t, keyPath)); len(problems) > 0 {
		t.Fatalf("申请里有私钥泄露的迹象: %v\n%s", problems, raw)
	}
	// 终端里告诉运营者证书指纹和实际用的证书文件，方便和管理员核对、也让人一眼看出用错了文件
	// （Windows 服务的证书目录不是前台默认的 ./certs）。
	if !strings.Contains(stderr, fp) {
		t.Fatalf("终端输出里应有证书指纹 %s:\n%s", fp, stderr)
	}
	if !strings.Contains(stderr, certPath) {
		t.Fatalf("终端输出里应有实际使用的证书文件 %s:\n%s", certPath, stderr)
	}
}

func readKeyDER(t *testing.T, keyPath string) []byte {
	t.Helper()
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("%s 不是 PEM", keyPath)
	}
	return block.Bytes
}

// requestLeaks 列出申请文件里能看出私钥泄露的迹象：私钥 DER（原样或 base64）出现在文件、request 载荷或签名里，
// 或者 request 载荷里有白名单之外的字段。申请里的 request、sig 都是 base64 的 []byte，
// 所以光在文件里找 "PRIVATE" 字样什么也找不到，必须解码后再查。
func requestLeaks(raw, keyDER []byte) []string {
	var env directory.SignedNodeRequest
	if err := json.Unmarshal(raw, &env); err != nil {
		return []string{"申请文件不是合法 JSON: " + err.Error()}
	}
	var problems []string
	b64 := []byte(base64.StdEncoding.EncodeToString(keyDER))
	for name, blob := range map[string][]byte{"文件": raw, "request 载荷": env.Request, "sig": env.Sig, "prev_sig": env.PrevSig} {
		if bytes.Contains(blob, keyDER) || bytes.Contains(blob, b64) {
			problems = append(problems, name+"里含有私钥")
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(env.Request, &fields); err != nil {
		return append(problems, "request 载荷不是合法 JSON: "+err.Error())
	}
	allowed := map[string]bool{"format": true, "name": true, "operator": true, "region": true, "tcp": true,
		"quic": true, "cert_der": true, "prev_cert_der": true, "created_at": true}
	for k := range fields {
		if !allowed[k] {
			problems = append(problems, "request 载荷里有不认识的字段 "+k)
		}
	}
	return problems
}

// requestLeaks 自己得真能查出问题，否则上面的「不含私钥」断言形同虚设。
func TestRequestLeaksCatchesLeaks(t *testing.T) {
	keyDER := []byte("not-a-real-key-der-but-long-enough-to-search-for")
	b64 := base64.StdEncoding.EncodeToString(keyDER)
	wrap := func(payload string) []byte {
		raw, err := json.Marshal(directory.SignedNodeRequest{Request: []byte(payload), Sig: []byte("sig")})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	leaky := map[string][]byte{
		"多出的字段带着私钥":   wrap(`{"format":1,"name":"n","extra":"` + b64 + `"}`),
		"允许的字段里塞了私钥":  wrap(`{"format":1,"name":"n","region":"` + b64 + `"}`),
		"多出的字段（内容无害）": wrap(`{"format":1,"name":"n","note":"x"}`),
	}
	for name, raw := range leaky {
		if len(requestLeaks(raw, keyDER)) == 0 {
			t.Errorf("%s: 没查出来", name)
		}
	}
	if p := requestLeaks(wrap(`{"format":1,"name":"n","operator":"o","tcp":["a:1"],"cert_der":"AAAA","created_at":1}`), keyDER); len(p) > 0 {
		t.Errorf("干净的申请被误报: %v", p)
	}
}

// 换证书：旧证书私钥共签，申请里带两个指纹。
func TestNodeRequestWithPreviousCertificate(t *testing.T) {
	dir := t.TempDir()
	newDir, oldDir := filepath.Join(dir, "new"), filepath.Join(dir, "old")
	for _, d := range []string{newDir, oldDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	newCert, newKey := nodeRequestCerts(t, newDir)
	oldCert, oldKey := nodeRequestCerts(t, oldDir)
	out := filepath.Join(dir, "request.json")

	stdout, stderr, code := execNodeRequest(
		"--cert", newCert, "--key", newKey, "--prev-cert", oldCert, "--prev-key", oldKey,
		"--name", "n", "--operator", "official", "--tcp", "relay.example.test:8443", "--out", out)
	if code != 0 {
		t.Fatalf("code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	v, err := directory.VerifyNodeRequest(raw)
	if err != nil {
		t.Fatalf("换证书申请验不过: %v", err)
	}
	if v.Fingerprint != nodeRequestFingerprint(t, newCert, newKey) || v.PrevFingerprint != nodeRequestFingerprint(t, oldCert, oldKey) {
		t.Fatalf("指纹不对: new=%s prev=%s", v.Fingerprint, v.PrevFingerprint)
	}
}

// 不给 --cert/--key 时按 --certs-dir 找 server.crt / server.key，与 relay 启动时的默认位置一致。
func TestNodeRequestFindsCertsInCertsDir(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := nodeRequestCerts(t, dir)
	out := filepath.Join(t.TempDir(), "request.json")

	stdout, stderr, code := execNodeRequest("--certs-dir", dir,
		"--name", "n", "--operator", "official", "--tcp", "relay.example.test:8443", "--out", out)
	if code != 0 {
		t.Fatalf("code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	v, err := directory.VerifyNodeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if v.Fingerprint != nodeRequestFingerprint(t, certPath, keyPath) {
		t.Fatalf("用的不是 --certs-dir 里的证书")
	}
}

// --out - 写到标准输出：Windows PowerShell 的 > 会把输出写成 UTF-16，所以默认要求显式 --out，但保留 stdout 选项。
func TestNodeRequestToStdout(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := nodeRequestCerts(t, dir)

	stdout, stderr, code := execNodeRequest("--cert", certPath, "--key", keyPath,
		"--name", "n", "--operator", "official", "--tcp", "relay.example.test:8443", "--out", "-")
	if code != 0 {
		t.Fatalf("code = %d\nstderr: %s", code, stderr)
	}
	// 整个 stdout 就是申请本身：终端提示走 stderr，混进任何文字都会让它解析失败。
	if _, err := directory.VerifyNodeRequest([]byte(stdout)); err != nil {
		t.Fatalf("标准输出里的申请验不过: %v\n%s", err, stdout)
	}
	if !strings.Contains(stderr, "stdout") {
		t.Fatalf("终端提示应说明写到了标准输出:\n%s", stderr)
	}
}

// 非法输入一律失败，且不留下输出文件；证书找不到时不能像 relay 启动那样自动生成一张。
func TestNodeRequestRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := nodeRequestCerts(t, dir)
	otherDir := t.TempDir()
	otherCert, otherKey := nodeRequestCerts(t, otherDir)
	out := filepath.Join(t.TempDir(), "request.json")

	base := func(extra ...string) []string {
		args := []string{"--cert", certPath, "--key", keyPath, "--name", "n", "--operator", "official",
			"--tcp", "relay.example.test:8443", "--out", out}
		return append(args, extra...)
	}
	without := func(flag string) []string {
		args := base()
		for i := 0; i < len(args); i++ {
			if args[i] == flag {
				return append(slicesClone(args[:i]), args[i+2:]...)
			}
		}
		t.Fatalf("base 里没有 %s", flag)
		return nil
	}
	cases := map[string][]string{
		"缺 --out":                  without("--out"),
		"缺 --name":                 without("--name"),
		"缺 --operator":             without("--operator"),
		"缺 --tcp":                  without("--tcp"),
		"tcp 写成 URL":               append(without("--tcp"), "--tcp", "https://relay.example.test:8443"),
		"tcp 缺端口":                  append(without("--tcp"), "--tcp", "relay.example.test"),
		"quic 缺端口":                 base("--quic", "relay.example.test"),
		"名称含空格":                    append(without("--name"), "--name", "node a"),
		"只给 --cert":                {"--cert", certPath, "--name", "n", "--operator", "o", "--tcp", "relay.example.test:8443", "--out", out},
		"证书与私钥不配对":                 {"--cert", certPath, "--key", otherKey, "--name", "n", "--operator", "o", "--tcp", "relay.example.test:8443", "--out", out},
		"证书文件不存在":                  {"--cert", filepath.Join(dir, "none.crt"), "--key", keyPath, "--name", "n", "--operator", "o", "--tcp", "relay.example.test:8443", "--out", out},
		"--prev-cert 缺 --prev-key": base("--prev-cert", otherCert),
		"--prev-key 缺 --prev-cert": base("--prev-key", otherKey),
		"旧证书与新证书相同":                base("--prev-cert", certPath, "--prev-key", keyPath),
		"旧证书私钥不配对":                 base("--prev-cert", otherCert, "--prev-key", keyPath),
		"多余的位置参数":                  base("extra"),
	}
	for name, args := range cases {
		if _, stderr, code := execNodeRequest(args...); code == 0 {
			t.Errorf("%s: 应当失败，却成功了\n%s", name, stderr)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: 失败后不应留下输出文件", name)
			os.Remove(out)
		}
	}

	// 找不到证书时不生成：目录里不能凭空多出文件。
	empty := t.TempDir()
	if _, _, code := execNodeRequest("--certs-dir", empty, "--name", "n", "--operator", "o",
		"--tcp", "relay.example.test:8443", "--out", out); code == 0 {
		t.Error("certs-dir 里没有证书时应当失败")
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("node-request 不应生成证书，目录里多了 %d 个文件", len(entries))
	}
}

// 缺参数时报错要直接说清楚缺什么，而不是落到一句系统的「找不到文件」上。
func TestNodeRequestExplainsMissingPieces(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := nodeRequestCerts(t, dir)
	common := []string{"--name", "n", "--operator", "o", "--tcp", "relay.example.test:8443"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"缺 --out", append([]string{"--cert", certPath, "--key", keyPath}, common...), "--out"},
		{"只给 --cert", append([]string{"--cert", certPath, "--out", "-"}, common...), "--key"},
		{"只给 --key", append([]string{"--key", keyPath, "--out", "-"}, common...), "--cert"},
		{"只给 --prev-key", append([]string{"--cert", certPath, "--key", keyPath, "--prev-key", keyPath, "--out", "-"}, common...), "--prev-cert"},
		{"只给 --prev-cert", append([]string{"--cert", certPath, "--key", keyPath, "--prev-cert", certPath, "--out", "-"}, common...), "--prev-key"},
	}
	for _, tc := range cases {
		_, stderr, code := execNodeRequest(tc.args...)
		if code == 0 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: code=%d，报错应提到 %q:\n%s", tc.name, code, tc.want, stderr)
		}
	}
}

// 已有文件不覆盖：申请文件是要发给管理员的，误覆盖会让人拿旧申请去签。
func TestNodeRequestNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := nodeRequestCerts(t, dir)
	out := filepath.Join(dir, "request.json")
	if err := os.WriteFile(out, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, code := execNodeRequest("--cert", certPath, "--key", keyPath, "--name", "n", "--operator", "o",
		"--tcp", "relay.example.test:8443", "--out", out)
	if code == 0 {
		t.Fatal("输出文件已存在时应当失败")
	}
	if got, _ := os.ReadFile(out); string(got) != "keep" {
		t.Fatalf("已有文件被改写: %q", got)
	}
}

// 子命令分发：node-request 由它接管；其余参数（含 run、普通旗标）一律不接管，仍走 relay 本身的流程。
func TestDispatchSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	handled, code := dispatchSubcommand([]string{"node-request", "-h"}, &out, &errOut)
	if !handled || code != 0 {
		t.Fatalf("node-request -h: handled=%v code=%d", handled, code)
	}
	if !strings.Contains(errOut.String(), "--out") {
		t.Fatalf("-h 应列出参数:\n%s", errOut.String())
	}
	for _, args := range [][]string{nil, {"run"}, {"--length", "12"}, {"service", "status"}} {
		if handled, _ := dispatchSubcommand(args, &out, &errOut); handled {
			t.Errorf("%q 不应由 dispatchSubcommand 接管", args)
		}
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }
