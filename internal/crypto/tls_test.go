package crypto

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 回归：skipVerify 与 caFile 同时给出时必须报错，而不是静默丢弃 caFile。
// crypto/tls 里 InsecureSkipVerify 的优先级高于 RootCAs——修复前这个组合会返回一个
// 什么都不校验的 config，用户以为自己钉了 CA，实际连链都没验，是最坏的一种失败方式。
func TestNewTLSClientConfigRejectsSkipVerifyWithCA(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewTLSClientConfig(true, ca)
	if err == nil {
		t.Fatalf("skipVerify+caFile 应报错，实际返回 config=%+v", cfg)
	}
	if !strings.Contains(err.Error(), "--ca") {
		t.Errorf("错误信息应点明是 --ca 被忽略，实际: %v", err)
	}
}

// 只给 skipVerify（默认用法）应正常返回，且确实跳过校验。
func TestNewTLSClientConfigSkipVerifyOnly(t *testing.T) {
	cfg, err := NewTLSClientConfig(true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify 应为 true")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion=%x 期望 TLS1.3", cfg.MinVersion)
	}
}

// 只给 caFile（--insecure=false --ca x，作者预期的校验用法）应装载 RootCAs。
func TestNewTLSClientConfigCAOnly(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewTLSClientConfig(false, certFile)
	if err != nil {
		t.Fatalf("--insecure=false --ca 应可用: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify 应为 false")
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs 应被装载")
	}
}

// caFile 无法解析成证书时必须报错，不能悄悄退回“无 RootCAs + 不跳过校验”的状态。
func TestNewTLSClientConfigRejectsBadCA(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTLSClientConfig(false, bad); err == nil {
		t.Fatal("非法 CA 文件应报错")
	}
}
