package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/relay"
)

func TestParseRelayOptionsDefaults(t *testing.T) {
	t.Setenv("REMOTE_RELAY_LIMITS_FILE", "")
	opts, err := parseRelayOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.listenAddr != ":8443" || opts.codeTTL != 30*time.Minute || opts.codeLength != 12 {
		t.Fatalf("unexpected defaults: %+v", opts)
	}
	if !opts.trustSourceIP || opts.plain || opts.noAuth {
		t.Fatalf("unexpected security defaults: %+v", opts)
	}
}

// 防冒领：码绝对寿命默认非零（24h）；--code-max-age 0 可显式关闭；--state-dir 默认空（密钥只在内存）。
func TestParseRelayOptionsCodeMaxAgeDefaultsNonZero(t *testing.T) {
	t.Setenv("REMOTE_RELAY_LIMITS_FILE", "")
	opts, err := parseRelayOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.codeMaxAge != 24*time.Hour || defaultCodeMaxAge != 24*time.Hour {
		t.Fatalf("code-max-age 默认应为 24h，得 %v", opts.codeMaxAge)
	}
	if opts.stateDir != "" {
		t.Fatalf("state-dir 默认应为空，得 %q", opts.stateDir)
	}
	opts, err = parseRelayOptions([]string{"--code-max-age", "0", "--state-dir", "/var/lib/relay"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.codeMaxAge != 0 || opts.stateDir != "/var/lib/relay" {
		t.Fatalf("显式值应生效: %+v", opts)
	}
}

func TestParseRelayOptionsRejectsInvalidValues(t *testing.T) {
	tests := [][]string{
		{"--ttl", "0"},
		{"--length", "0"},
		{"--cert", "server.crt"},
		{"--key", "server.key"},
		{"unexpected"},
		{"--plain", "--quic-listen", ":8443"},
		{"--advertise-stun", "no-port"},
		{"--advertise-stun", "a:1,b:1,c:1,d:1,e:1"},
	}
	for _, args := range tests {
		if _, err := parseRelayOptions(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("parseRelayOptions(%q) succeeded, want error", args)
		}
	}
}

func TestConfigureTLSGeneratesCompletePair(t *testing.T) {
	dir := t.TempDir()
	opts := relayOptions{certsDir: dir}
	cfg := &relay.Config{UseTLS: true}
	if err := configureTLS(cfg, opts); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.TLSCertFile, cfg.TLSKeyFile} {
		if !filepath.IsAbs(path) {
			t.Fatalf("generated path is not absolute: %s", path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("generated file %s: %v", path, err)
		}
	}
}

func TestRunRelayVersionDoesNotStartListener(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := runRelay(context.Background(), []string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() == 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// --advertise-stun 逗号分隔、两侧空白忽略，原样进 relay.Config。
func TestParseRelayOptionsAdvertiseSTUN(t *testing.T) {
	opts, err := parseRelayOptions([]string{"--advertise-stun", "23.95.78.14:3478, stun.chat.bilibili.com:3478,,stun.cloudflare.com:3478"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"23.95.78.14:3478", "stun.chat.bilibili.com:3478", "stun.cloudflare.com:3478"}
	if !reflect.DeepEqual(opts.advertiseSTUN, want) {
		t.Fatalf("advertiseSTUN = %q, want %q", opts.advertiseSTUN, want)
	}
}
