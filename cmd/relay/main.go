package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/relay"
	"github.com/remote-assist/tool/internal/version"
)

type relayOptions struct {
	listenAddr    string
	certFile      string
	keyFile       string
	codeTTL       time.Duration
	codeLength    int
	auditLog      string
	plain         bool
	genCerts      bool
	certsDir      string
	stunAddr      string
	trustSourceIP bool
	limitsFile    string
	printLimits   bool
	noAuth        bool
	showVersion   bool
}

func main() {
	if handled, exitCode := dispatchPlatform(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(exitCode)
	}

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	prepareInteractiveConsole()
	os.Exit(runForegroundRelay(args, os.Stdout, os.Stderr))
}

func runForegroundRelay(args []string, stdout, stderr io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := runRelay(ctx, args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "relay: %v\n", err)
		return 1
	}
	return 0
}

func runRelay(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runRelayWithReady(ctx, args, stdout, stderr, nil)
}

func runRelayWithReady(ctx context.Context, args []string, stdout, stderr io.Writer, onReady func()) error {
	opts, err := parseRelayOptions(args, stderr)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Fprintf(stdout, "relay-server %s\n", version.Info())
		return nil
	}
	if opts.printLimits {
		fmt.Fprintln(stdout, relay.DefaultLimits().JSON())
		return nil
	}
	if opts.genCerts {
		return generateCerts(opts.certsDir, stdout)
	}

	limits := relay.DefaultLimits()
	if opts.limitsFile != "" {
		limits, err = relay.LoadLimitsFile(opts.limitsFile)
		if err != nil {
			return fmt.Errorf("invalid limits file %s: %w", opts.limitsFile, err)
		}
	}

	cfg := &relay.Config{
		ListenAddr:            opts.listenAddr,
		TLSCertFile:           opts.certFile,
		TLSKeyFile:            opts.keyFile,
		CodeTTL:               opts.codeTTL,
		CodeLength:            opts.codeLength,
		AuditLogFile:          opts.auditLog,
		UseTLS:                !opts.plain,
		STUNListenAddr:        opts.stunAddr,
		NoAuth:                opts.noAuth,
		DisableSourceIPLimits: !opts.trustSourceIP,
		Limits:                limits,
	}

	if opts.noAuth {
		log.Printf("WARNING: NO-AUTH mode enabled. Any device that can reach this relay can connect and fully control the share side. Use ONLY on a fully trusted private LAN.")
	}
	if err := configureTLS(cfg, opts); err != nil {
		return err
	}

	server, err := relay.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	log.Printf("Relay server version: %s", version.Info())
	if opts.plain {
		log.Printf("WARNING: Running in plain mode (INSECURE - for development only)")
	}
	if err := server.StartWithContextReady(ctx, onReady); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

func parseRelayOptions(args []string, stderr io.Writer) (relayOptions, error) {
	opts := relayOptions{
		listenAddr:    ":8443",
		codeTTL:       30 * time.Minute,
		codeLength:    10,
		auditLog:      "audit.log",
		certsDir:      "./certs",
		trustSourceIP: true,
		limitsFile:    strings.TrimSpace(os.Getenv("REMOTE_RELAY_LIMITS_FILE")),
	}

	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.listenAddr, "listen", opts.listenAddr, "Listen address")
	fs.StringVar(&opts.certFile, "cert", "", "TLS certificate file")
	fs.StringVar(&opts.keyFile, "key", "", "TLS key file")
	fs.DurationVar(&opts.codeTTL, "ttl", opts.codeTTL, "Assist code TTL")
	fs.IntVar(&opts.codeLength, "length", opts.codeLength, "Assist code length")
	fs.StringVar(&opts.auditLog, "audit", opts.auditLog, "Audit log file")
	fs.BoolVar(&opts.plain, "plain", false, "Use plain TCP (insecure, for dev only)")
	fs.BoolVar(&opts.genCerts, "gen-certs", false, "Generate self-signed certs and exit")
	fs.StringVar(&opts.certsDir, "certs-dir", opts.certsDir, "Directory for generated certs")
	fs.StringVar(&opts.stunAddr, "stun", "", "STUN server listen address (empty to disable, e.g. ':3478' to enable)")
	fs.BoolVar(&opts.trustSourceIP, "trust-source-ip", opts.trustSourceIP, "Trust direct peer IP for per-IP limits; set false behind an SNAT load balancer")
	fs.StringVar(&opts.limitsFile, "limits-file", opts.limitsFile, "JSON rate-limit config file (or REMOTE_RELAY_LIMITS_FILE)")
	fs.BoolVar(&opts.printLimits, "print-default-limits", false, "Print default rate-limit JSON and exit")
	fs.BoolVar(&opts.noAuth, "no-auth", false, "Use fixed code (no assist code exchange needed). DANGER: trusted private LANs only")
	fs.BoolVar(&opts.showVersion, "version", false, "Show version information")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Remote Assist Relay Server %s\n\n", version.Info())
		fmt.Fprintf(stderr, "Usage:\n  %s run [options]\n  %s [options]\n\nOptions:\n", os.Args[0], os.Args[0])
		fs.PrintDefaults()
		fmt.Fprintln(stderr)
	}
	if err := fs.Parse(args); err != nil {
		return relayOptions{}, err
	}
	if fs.NArg() != 0 {
		return relayOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.codeTTL <= 0 {
		return relayOptions{}, fmt.Errorf("ttl must be positive")
	}
	if opts.codeLength <= 0 {
		return relayOptions{}, fmt.Errorf("length must be positive")
	}
	if (opts.certFile == "") != (opts.keyFile == "") {
		return relayOptions{}, fmt.Errorf("cert and key must be specified together")
	}
	return opts, nil
}

func configureTLS(cfg *relay.Config, opts relayOptions) error {
	if opts.plain || (opts.certFile != "" && opts.keyFile != "") {
		return nil
	}
	defaultCert := filepath.Join(opts.certsDir, "server.crt")
	defaultKey := filepath.Join(opts.certsDir, "server.key")
	if fileExists(defaultCert) && fileExists(defaultKey) {
		cfg.TLSCertFile = defaultCert
		cfg.TLSKeyFile = defaultKey
		return nil
	}

	log.Printf("No TLS certs specified, generating self-signed certs in %s", opts.certsDir)
	if err := os.MkdirAll(opts.certsDir, 0700); err != nil {
		return fmt.Errorf("create certs directory: %w", err)
	}
	if err := crypto.GenerateSelfSignedCert(defaultCert, defaultKey); err != nil {
		return fmt.Errorf("generate self-signed certs: %w", err)
	}
	cfg.TLSCertFile = defaultCert
	cfg.TLSKeyFile = defaultKey
	return nil
}

func generateCerts(certsDir string, stdout io.Writer) error {
	if err := os.MkdirAll(certsDir, 0700); err != nil {
		return fmt.Errorf("create certs directory: %w", err)
	}
	certPath := filepath.Join(certsDir, "server.crt")
	keyPath := filepath.Join(certsDir, "server.key")
	if err := crypto.GenerateSelfSignedCert(certPath, keyPath); err != nil {
		return fmt.Errorf("generate self-signed certs: %w", err)
	}
	fmt.Fprintf(stdout, "Generated certs:\n  Cert: %s\n  Key:  %s\n", certPath, keyPath)
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
