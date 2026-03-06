package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/relay"
)

var (
	listenAddr  = flag.String("listen", ":8443", "Listen address")
	certFile    = flag.String("cert", "", "TLS certificate file")
	keyFile     = flag.String("key", "", "TLS key file")
	codeTTL     = flag.Duration("ttl", 30*time.Minute, "Assist code TTL")
	codeLength  = flag.Int("length", 10, "Assist code length")
	auditLog    = flag.String("audit", "audit.log", "Audit log file")
	plain       = flag.Bool("plain", false, "Use plain TCP (insecure, for dev only)")
	genCerts    = flag.Bool("gen-certs", false, "Generate self-signed certs and exit")
	certsDir    = flag.String("certs-dir", "./certs", "Directory for generated certs")
	stunAddr    = flag.String("stun", ":3478", "STUN server listen address (empty to disable)")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Remote Assist Relay Server\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}
	flag.Parse()

	if *genCerts {
		generateCerts()
		return
	}

	cfg := &relay.Config{
		ListenAddr:     *listenAddr,
		TLSCertFile:    *certFile,
		TLSKeyFile:     *keyFile,
		CodeTTL:        *codeTTL,
		CodeLength:     *codeLength,
		AuditLogFile:   *auditLog,
		UseTLS:         !*plain,
		STUNListenAddr: *stunAddr,
	}

	if !*plain && (*certFile == "" || *keyFile == "") {
		defaultCert := filepath.Join(*certsDir, "server.crt")
		defaultKey := filepath.Join(*certsDir, "server.key")
		if fileExists(defaultCert) && fileExists(defaultKey) {
			cfg.TLSCertFile = defaultCert
			cfg.TLSKeyFile = defaultKey
		} else {
			log.Printf("No TLS certs specified, generating self-signed certs in %s", *certsDir)
			if err := os.MkdirAll(*certsDir, 0700); err == nil {
				if err := crypto.GenerateSelfSignedCert(defaultCert, defaultKey); err == nil {
					cfg.TLSCertFile = defaultCert
					cfg.TLSKeyFile = defaultKey
				}
			}
		}
	}

	server, err := relay.NewServer(cfg)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	if *plain {
		log.Printf("WARNING: Running in plain mode (INSECURE - for development only)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	err = server.StartWithContext(ctx)
	if err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func generateCerts() {
	if err := os.MkdirAll(*certsDir, 0700); err != nil {
		log.Fatalf("Failed to create certs dir: %v", err)
	}
	certPath := filepath.Join(*certsDir, "server.crt")
	keyPath := filepath.Join(*certsDir, "server.key")

	if err := crypto.GenerateSelfSignedCert(certPath, keyPath); err != nil {
		log.Fatalf("Failed to generate certs: %v", err)
	}
	fmt.Printf("Generated certs:\n  Cert: %s\n  Key:  %s\n", certPath, keyPath)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
