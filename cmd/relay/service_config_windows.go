//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	windowsServiceName        = "RemoteAssistRelay"
	windowsServiceDisplayName = "Remote Assist Relay"
	windowsServiceDescription = "Remote Assist relay server"
	windowsEventSource        = windowsServiceName
)

type windowsServicePaths struct {
	installDir string
	executable string
	dataDir    string
	configFile string
	certsDir   string
	auditFile  string
}

type windowsServiceConfig struct {
	ListenAddr    string `json:"listen"`
	CertFile      string `json:"cert,omitempty"`
	KeyFile       string `json:"key,omitempty"`
	CodeTTL       string `json:"ttl"`
	CodeLength    int    `json:"length"`
	AuditLog      string `json:"audit"`
	Plain         bool   `json:"plain"`
	CertsDir      string `json:"certs_dir"`
	STUNAddr      string `json:"stun,omitempty"`
	TrustSourceIP bool   `json:"trust_source_ip"`
	LimitsFile    string `json:"limits_file,omitempty"`
	NoAuth        bool   `json:"no_auth"`
}

func defaultWindowsServicePaths() (windowsServicePaths, error) {
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		return windowsServicePaths{}, errors.New("ProgramFiles environment variable is empty")
	}
	programData := os.Getenv("ProgramData")
	if programData == "" {
		return windowsServicePaths{}, errors.New("ProgramData environment variable is empty")
	}
	installDir := filepath.Join(programFiles, "RemoteAssistRelay")
	dataDir := filepath.Join(programData, "RemoteAssistRelay")
	return windowsServicePaths{
		installDir: installDir,
		executable: filepath.Join(installDir, "relay.exe"),
		dataDir:    dataDir,
		configFile: filepath.Join(dataDir, "config.json"),
		certsDir:   filepath.Join(dataDir, "certs"),
		auditFile:  filepath.Join(dataDir, "logs", "audit.jsonl"),
	}, nil
}

func defaultWindowsServiceConfig(paths windowsServicePaths) windowsServiceConfig {
	return windowsServiceConfig{
		ListenAddr:    ":8443",
		CodeTTL:       "30m",
		CodeLength:    10,
		AuditLog:      paths.auditFile,
		CertsDir:      paths.certsDir,
		TrustSourceIP: true,
	}
}

func loadWindowsServiceConfig(path string, defaults windowsServiceConfig) (windowsServiceConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return windowsServiceConfig{}, err
	}
	defer file.Close()

	cfg := defaults
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return windowsServiceConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return windowsServiceConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return windowsServiceConfig{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return cfg, nil
}

func (cfg windowsServiceConfig) validate() error {
	if cfg.ListenAddr == "" {
		return errors.New("listen must not be empty")
	}
	ttl, err := time.ParseDuration(cfg.CodeTTL)
	if err != nil || ttl <= 0 {
		return fmt.Errorf("ttl must be a positive duration: %q", cfg.CodeTTL)
	}
	if cfg.CodeLength <= 0 {
		return errors.New("length must be positive")
	}
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return errors.New("cert and key must be specified together")
	}
	if !cfg.Plain && cfg.CertFile == "" && cfg.CertsDir == "" {
		return errors.New("certs_dir must be a non-empty absolute path when TLS certificates are generated automatically")
	}
	paths := map[string]string{
		"cert":        cfg.CertFile,
		"key":         cfg.KeyFile,
		"audit":       cfg.AuditLog,
		"certs_dir":   cfg.CertsDir,
		"limits_file": cfg.LimitsFile,
	}
	for name, path := range paths {
		if path != "" && !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an absolute path: %s", name, path)
		}
	}
	return nil
}

func (cfg windowsServiceConfig) relayArgs() []string {
	args := []string{
		"--listen", cfg.ListenAddr,
		"--ttl", cfg.CodeTTL,
		"--length", strconv.Itoa(cfg.CodeLength),
		"--audit", cfg.AuditLog,
		"--certs-dir", cfg.CertsDir,
		"--trust-source-ip=" + strconv.FormatBool(cfg.TrustSourceIP),
	}
	if cfg.CertFile != "" {
		args = append(args, "--cert", cfg.CertFile, "--key", cfg.KeyFile)
	}
	if cfg.Plain {
		args = append(args, "--plain")
	}
	if cfg.STUNAddr != "" {
		args = append(args, "--stun", cfg.STUNAddr)
	}
	if cfg.LimitsFile != "" {
		args = append(args, "--limits-file", cfg.LimitsFile)
	}
	if cfg.NoAuth {
		args = append(args, "--no-auth")
	}
	return args
}

func ensureWindowsServiceConfig(paths windowsServicePaths, source string) error {
	if err := os.MkdirAll(filepath.Dir(paths.auditFile), 0750); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	if err := os.MkdirAll(paths.certsDir, 0750); err != nil {
		return fmt.Errorf("create cert directory: %w", err)
	}

	defaults := defaultWindowsServiceConfig(paths)
	if source != "" {
		cfg, err := loadWindowsServiceConfig(source, defaults)
		if err != nil {
			return err
		}
		return writeWindowsServiceConfig(paths.configFile, cfg, true)
	}
	if _, err := os.Stat(paths.configFile); err == nil {
		_, err = loadWindowsServiceConfig(paths.configFile, defaults)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeWindowsServiceConfig(paths.configFile, defaults, false)
}

func writeWindowsServiceConfig(path string, cfg windowsServiceConfig, replace bool) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	if !replace {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
		if err != nil {
			return err
		}
		if _, err = file.Write(data); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return replaceFileWindows(tempPath, path)
}
