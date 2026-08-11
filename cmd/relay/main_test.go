package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
	if opts.listenAddr != ":8443" || opts.codeTTL != 30*time.Minute || opts.codeLength != 10 {
		t.Fatalf("unexpected defaults: %+v", opts)
	}
	if !opts.trustSourceIP || opts.plain || opts.noAuth {
		t.Fatalf("unexpected security defaults: %+v", opts)
	}
}

func TestParseRelayOptionsRejectsInvalidValues(t *testing.T) {
	tests := [][]string{
		{"--ttl", "0"},
		{"--length", "0"},
		{"--cert", "server.crt"},
		{"--key", "server.key"},
		{"unexpected"},
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
