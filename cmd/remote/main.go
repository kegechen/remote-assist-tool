package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/remote-assist/tool/internal/client"
	"github.com/remote-assist/tool/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "share":
		runShare(os.Args[2:])
	case "help":
		runHelp(os.Args[2:])
	case "--version", "-version", "version":
		fmt.Printf("remote-assist %s\n", version.Info())
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runShare(args []string) {
	fs := flag.NewFlagSet("share", flag.ExitOnError)
	server := fs.String("server", "localhost:8443", "Relay server address")
	sshAddr := fs.String("ssh", "127.0.0.1:22", "Local SSH address")
	insecure := fs.Bool("insecure", false, "Skip TLS verification")
	caFile := fs.String("ca", "", "CA certificate file")
	plain := fs.Bool("plain", false, "Use plain TCP (insecure, for dev only)")
	p2pMode := fs.String("p2p", "auto", "P2P mode: disabled, auto, required")
	stunServer := fs.String("stun", "", "STUN server address for P2P (default: same as relay:3478)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Share mode - allow others to assist you\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s share [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}
	fs.Parse(args)

	// Derive STUN server if not specified
	if *stunServer == "" && *server != "" {
		host, _, _ := net.SplitHostPort(*server)
		if host != "" {
			*stunServer = net.JoinHostPort(host, "3478")
		}
	}

	cfg := &client.Config{
		ServerAddr:   *server,
		InsecureSkip: *insecure,
		CAFile:       *caFile,
		UseTLS:       !*plain,
		P2PMode:      *p2pMode,
		STUNServer:   *stunServer,
	}

	share := client.NewShareMode(cfg, *sshAddr)
	code, expiresAt, err := share.Run()
	if err != nil {
		log.Fatalf("Error: %v", err)
	}

	fmt.Printf("\nSession ended. Code %s expired at %s\n", code, expiresAt.Local().Format("2006-01-02 15:04:05"))
}

func runHelp(args []string) {
	fs := flag.NewFlagSet("help", flag.ExitOnError)
	server := fs.String("server", "localhost:8443", "Relay server address")
	code := fs.String("code", "", "Assist code (required)")
	listenAddr := fs.String("listen", "127.0.0.1:2222", "Local listen address")
	insecure := fs.Bool("insecure", false, "Skip TLS verification")
	caFile := fs.String("ca", "", "CA certificate file")
	plain := fs.Bool("plain", false, "Use plain TCP (insecure, for dev only)")
	p2pMode := fs.String("p2p", "auto", "P2P mode: disabled, auto, required")
	stunServer := fs.String("stun", "", "STUN server address for P2P (default: same as relay:3478)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Help mode - assist someone else\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s help --code <code> [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}
	fs.Parse(args)

	if *code == "" {
		fmt.Fprintf(os.Stderr, "Error: --code is required\n\n")
		fs.Usage()
		os.Exit(1)
	}

	// Derive STUN server if not specified
	if *stunServer == "" && *server != "" {
		host, _, _ := net.SplitHostPort(*server)
		if host != "" {
			*stunServer = net.JoinHostPort(host, "3478")
		}
	}

	cfg := &client.Config{
		ServerAddr:   *server,
		InsecureSkip: *insecure,
		CAFile:       *caFile,
		UseTLS:       !*plain,
		P2PMode:      *p2pMode,
		STUNServer:   *stunServer,
	}

	help := client.NewHelpMode(cfg, *code, *listenAddr)
	if err := help.Run(); err != nil {
		log.Fatalf("Error: %v", err)
	}

	fmt.Println("\nSession ended.")
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Remote Assist CLI %s\n\n", version.Info())
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  %s <command> [options]\n\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  share     - Share your SSH access with someone\n")
	fmt.Fprintf(os.Stderr, "  help      - Help someone using their assist code\n")
	fmt.Fprintf(os.Stderr, "  --version - Show version information\n")
	fmt.Fprintf(os.Stderr, "\nUse '%s <command> -h' for more info about a command\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\n")
}
