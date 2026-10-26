//go:build windows

package relayapp

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsServiceConfigModuleConfigFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", filepath.Join(root, "Program Files"))
	t.Setenv("ProgramData", filepath.Join(root, "ProgramData"))
	paths, err := defaultWindowsServicePaths()
	if err != nil {
		t.Fatal(err)
	}
	// 旧配置文件没有 module_config_file：照常加载，默认不启用模块框架。
	if err := os.MkdirAll(filepath.Dir(paths.configFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.configFile, []byte(`{"listen":":8443","ttl":"30m","length":10}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadWindowsServiceConfig(paths.configFile, defaultWindowsServiceConfig(paths))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModuleConfigFile != "" {
		t.Fatalf("module config enabled by default: %q", cfg.ModuleConfigFile)
	}
	opts, err := parseRelayOptions(cfg.relayArgs(), io.Discard)
	if err != nil || opts.moduleConfig != "" {
		t.Fatalf("legacy config round trip: %q %v", opts.moduleConfig, err)
	}

	cfg.ModuleConfigFile = filepath.Join(paths.dataDir, "modules.json")
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	opts, err = parseRelayOptions(cfg.relayArgs(), io.Discard)
	if err != nil || opts.moduleConfig != cfg.ModuleConfigFile {
		t.Fatalf("module config round trip: %q %v", opts.moduleConfig, err)
	}
	cfg.ModuleConfigFile = `relative\modules.json`
	if err := cfg.validate(); err == nil {
		t.Fatal("relative module_config_file accepted")
	}
}
