package main

import (
	"bytes"
	"testing"

	"github.com/remote-assist/tool/internal/update"
	"github.com/remote-assist/tool/internal/version"
)

func TestResolveUpdateInfo(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	resolve := func(args ...string) (string, error) {
		t.Helper()
		opts, err := parseRelayOptions(args, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		info, err := resolveUpdateInfo(opts)
		return info.LatestVersion, err
	}

	// 默认：取 relay 自身版本 + GitHub 模板
	version.Version = "v1.1.0"
	opts, _ := parseRelayOptions(nil, &bytes.Buffer{})
	got, err := resolveUpdateInfo(opts)
	if err != nil || got.LatestVersion != "v1.1.0" || got.DownloadURL != update.DefaultDownloadURL || got.ReleasePage != update.DefaultReleasePage {
		t.Errorf("default: %+v err=%v", got, err)
	}

	// 自身不是正式 tag：静默不下发（含纯数字 commit 哈希、describe 后缀、预发布）
	for _, v := range []string{"dev", "1234567", "v1.1.0-3-gabc123", "v1.2.0-rc1"} {
		version.Version = v
		if latest, err := resolve(); latest != "" || err != nil {
			t.Errorf("relay version %q should not announce: %q err=%v", v, latest, err)
		}
	}

	// 显式指定覆盖自身版本
	version.Version = "v1.1.0"
	if latest, err := resolve("--latest-version", "1.2.0", "--download-url", "https://intranet/{version}/{os}-{arch}{ext}"); latest != "1.2.0" || err != nil {
		t.Errorf("explicit: %q err=%v", latest, err)
	}
	if latest, err := resolve("--latest-version", "1.2.0", "--download-url", "https://intranet/v1.2.0/remote-assist-cli-{os}-{arch}{ext}", "--release-page", "https://intranet/releases/v1.2.0"); latest != "v1.2.0" || err != nil {
		t.Errorf("literal-link explicit: %q err=%v", latest, err)
	}
	if opts, err := parseRelayOptions([]string{"--latest-version", "1.2.0"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	} else if info, err := resolveUpdateInfo(opts); err != nil || info.LatestVersion != "v1.2.0" {
		t.Errorf("default GitHub fallback should use release tag: %+v err=%v", info, err)
	}

	// none 关闭
	for _, v := range []string{"none", "NONE"} {
		if latest, err := resolve("--latest-version", v); latest != "" || err != nil {
			t.Errorf("%q should disable: %q err=%v", v, latest, err)
		}
	}

	// 显式非法值报错，而不是静默关闭
	for _, args := range [][]string{
		{"--latest-version", "garbage"},
		{"--latest-version", "v1.2"},
		{"--download-url", "http://plain/{os}"},
		{"--release-page", "https://a b"},
	} {
		if _, err := resolve(args...); err == nil {
			t.Errorf("%v should be rejected", args)
		}
	}
}
