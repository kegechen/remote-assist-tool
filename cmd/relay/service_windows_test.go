//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

type recordedEvent struct {
	level   string
	eventID uint32
	message string
}

type recordingEventLogger struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (logger *recordingEventLogger) Info(eventID uint32, message string) error {
	logger.record("info", eventID, message)
	return nil
}

func (logger *recordingEventLogger) Warning(eventID uint32, message string) error {
	logger.record("warning", eventID, message)
	return nil
}

func (logger *recordingEventLogger) Error(eventID uint32, message string) error {
	logger.record("error", eventID, message)
	return nil
}

func (logger *recordingEventLogger) record(level string, eventID uint32, message string) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	logger.events = append(logger.events, recordedEvent{level: level, eventID: eventID, message: message})
}

func TestWindowsServiceConfigDefaultsAndStrictDecode(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", filepath.Join(root, "Program Files"))
	t.Setenv("ProgramData", filepath.Join(root, "ProgramData"))
	paths, err := defaultWindowsServicePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWindowsServiceConfig(paths, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadWindowsServiceConfig(paths.configFile, defaultWindowsServiceConfig(paths))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TrustSourceIP || cfg.AuditLog != paths.auditFile || cfg.CertsDir != paths.certsDir {
		t.Fatalf("unexpected service defaults: %+v", cfg)
	}

	unknown := filepath.Join(root, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"listen":":8443","unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWindowsServiceConfig(unknown, defaultWindowsServiceConfig(paths)); err == nil {
		t.Fatal("unknown config field was accepted")
	}
}

func TestWindowsServiceConfigRejectsRelativePaths(t *testing.T) {
	cfg := windowsServiceConfig{
		ListenAddr:    ":8443",
		CodeTTL:       "30m",
		CodeLength:    10,
		AuditLog:      "audit.log",
		CertsDir:      `C:\ProgramData\RemoteAssistRelay\certs`,
		TrustSourceIP: true,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("validate()=%v, want absolute path error", err)
	}
}

func TestWindowsServiceConfigRequiresCertsDirForAutomaticTLS(t *testing.T) {
	cfg := windowsServiceConfig{
		ListenAddr:    ":8443",
		CodeTTL:       "30m",
		CodeLength:    10,
		AuditLog:      `C:\ProgramData\RemoteAssistRelay\logs\audit.jsonl`,
		TrustSourceIP: true,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "certs_dir") {
		t.Fatalf("validate()=%v, want certs_dir error", err)
	}

	cfg.CertFile = `C:\certs\server.crt`
	cfg.KeyFile = `C:\certs\server.key`
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() with explicit TLS pair: %v", err)
	}
	cfg.CertFile = ""
	cfg.KeyFile = ""
	cfg.Plain = true
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() in plain mode: %v", err)
	}
}

func TestWindowsServiceDataACLIsProtectedAndScoped(t *testing.T) {
	securityDescriptor, err := windows.SecurityDescriptorFromString(windowsServiceDataSDDL)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := securityDescriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("service data DACL is not protected")
	}
	sddl := securityDescriptor.String()
	if !strings.Contains(sddl, "O:BA") {
		t.Fatalf("service data security descriptor %q is not owned by Administrators", sddl)
	}
	for _, sid := range []string{";;;SY)", ";;;BA)", ";;;LS)"} {
		if !strings.Contains(sddl, sid) {
			t.Fatalf("service data DACL %q does not contain %s", sddl, sid)
		}
	}
	for _, sid := range []string{";;;BU)", ";;;WD)", ";;;AU)"} {
		if strings.Contains(sddl, sid) {
			t.Fatalf("service data DACL %q unexpectedly contains %s", sddl, sid)
		}
	}
}

func TestWindowsServiceConfigRelayArgsRoundTrip(t *testing.T) {
	cfg := windowsServiceConfig{
		ListenAddr:    "127.0.0.1:9443",
		CodeTTL:       "1h",
		CodeLength:    12,
		AuditLog:      `C:\ProgramData\RemoteAssistRelay\logs\audit.jsonl`,
		CertsDir:      `C:\ProgramData\RemoteAssistRelay\certs`,
		TrustSourceIP: false,
		STUNAddr:      ":3478",
	}
	opts, err := parseRelayOptions(cfg.relayArgs(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.listenAddr != cfg.ListenAddr || opts.codeTTL != time.Hour || opts.codeLength != 12 || opts.trustSourceIP {
		t.Fatalf("round trip mismatch: %+v", opts)
	}
}

func TestStripInternalServiceArgs(t *testing.T) {
	clean, menu := stripInternalServiceArgs([]string{"install", "--config", "x.json", serviceMenuReturnArg})
	if !menu || strings.Join(clean, "|") != "install|--config|x.json" {
		t.Fatalf("clean=%q menu=%v", clean, menu)
	}
}

func TestParseWindowsServiceInstallArgsMakesConfigAbsolute(t *testing.T) {
	path, err := parseWindowsServiceInstallArgs([]string{"--config", "config.json"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "config.json" {
		t.Fatalf("path=%q", path)
	}
}

func TestInstallExecutableCopiesCurrentBinary(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "relay.exe")
	if err := installExecutable(destination); err != nil {
		t.Fatal(err)
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if sourceInfo.Size() != destinationInfo.Size() {
		t.Fatalf("copied size=%d want=%d", destinationInfo.Size(), sourceInfo.Size())
	}
}

func TestEventLogWriterClassifiesMessages(t *testing.T) {
	events := &recordingEventLogger{}
	writer := newEventLogWriter(events, 8)
	_, _ = writer.Write([]byte("relay ready"))
	_, _ = writer.Write([]byte("WARNING: insecure mode"))
	_, _ = writer.Write([]byte("server error"))
	writer.Close()

	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.events) != 3 {
		t.Fatalf("events=%d, want 3", len(events.events))
	}
	levels := []string{events.events[0].level, events.events[1].level, events.events[2].level}
	if strings.Join(levels, ",") != "info,warning,error" {
		t.Fatalf("levels=%v", levels)
	}
}

func TestWindowsRelayServiceReportsReadyThenStops(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", filepath.Join(root, "Program Files"))
	t.Setenv("ProgramData", filepath.Join(root, "ProgramData"))
	paths, err := defaultWindowsServicePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWindowsServiceConfig(paths, ""); err != nil {
		t.Fatal(err)
	}
	events := &recordingEventLogger{}
	service := &windowsRelayService{
		eventLog:   events,
		configPath: paths.configFile,
		runRelay: func(ctx context.Context, _ []string, _, _ io.Writer, onReady func()) error {
			onReady()
			<-ctx.Done()
			return nil
		},
	}
	requests := make(chan svc.ChangeRequest, 1)
	statuses := make(chan svc.Status, 4)
	type result struct {
		specific bool
		code     uint32
	}
	done := make(chan result, 1)
	go func() {
		specific, code := service.Execute(nil, requests, statuses)
		done <- result{specific: specific, code: code}
	}()

	if status := receiveServiceStatus(t, statuses); status.State != svc.StartPending {
		t.Fatalf("first state=%v, want StartPending", status.State)
	}
	if status := receiveServiceStatus(t, statuses); status.State != svc.Running {
		t.Fatalf("second state=%v, want Running", status.State)
	}
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	if status := receiveServiceStatus(t, statuses); status.State != svc.StopPending {
		t.Fatalf("third state=%v, want StopPending", status.State)
	}
	select {
	case got := <-done:
		if got.specific || got.code != 0 {
			t.Fatalf("Execute result=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not stop")
	}
}

func TestWindowsRelayServiceDoesNotReportRunningOnStartupFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ProgramFiles", filepath.Join(root, "Program Files"))
	t.Setenv("ProgramData", filepath.Join(root, "ProgramData"))
	paths, err := defaultWindowsServicePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureWindowsServiceConfig(paths, ""); err != nil {
		t.Fatal(err)
	}
	service := &windowsRelayService{
		eventLog:   &recordingEventLogger{},
		configPath: paths.configFile,
		runRelay: func(context.Context, []string, io.Writer, io.Writer, func()) error {
			return errors.New("bind failed")
		},
	}
	statuses := make(chan svc.Status, 2)
	specific, code := service.Execute(nil, make(chan svc.ChangeRequest), statuses)
	if !specific || code == 0 {
		t.Fatalf("specific=%v code=%d", specific, code)
	}
	if status := receiveServiceStatus(t, statuses); status.State != svc.StartPending {
		t.Fatalf("state=%v, want only StartPending", status.State)
	}
	select {
	case status := <-statuses:
		t.Fatalf("unexpected additional status: %+v", status)
	default:
	}
}

func receiveServiceStatus(t *testing.T, statuses <-chan svc.Status) svc.Status {
	t.Helper()
	select {
	case status := <-statuses:
		return status
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for service status")
		return svc.Status{}
	}
}
