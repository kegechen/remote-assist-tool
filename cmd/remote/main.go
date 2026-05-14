package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/client"
	"github.com/remote-assist/tool/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	log.Printf("remote-assist %s", version.Info())

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
	bindIP := fs.String("bind-ip", "", "Bind UDP to specific IP (bypass TUN proxy auto-detection)")
	rootDir := fs.String("root", "", "Sandbox root for file operations (required unless --unsafe-full-system)")
	allowExec := fs.String("allow-exec", "", "Comma-separated exec basename allowlist (empty = no restriction beyond deny)")
	denyExec := fs.String("deny-exec", "rm,shutdown,reboot,mkfs,dd", "Comma-separated exec basename denylist")
	elevate := fs.Bool("elevate", false, "Windows: request UAC elevation on startup via ShellExecuteW runas")
	unsafe := fs.Bool("unsafe-full-system", false, "DANGER: disable sandbox entirely")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Share mode - allow others to assist you\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s share [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}
	// --elevated-child 是内部旗标，不注册为 flag，必须在 fs.Parse 之前预先剥离，
	// 否则 flag.ExitOnError 会因"flag provided but not defined"直接 os.Exit(2)。
	hasElevatedChild := false
	clean := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--elevated-child" {
			hasElevatedChild = true
		} else {
			clean = append(clean, a)
		}
	}
	args = clean

	fs.Parse(args)

	if *unsafe {
		fmt.Fprint(os.Stderr, "\033[1;31m!!! DANGER: --unsafe-full-system disables ALL sandboxing.\nFiles, exec commands have NO restriction.\nAborting in 5 seconds — press Ctrl+C to abort.\033[0m\n")
		for i := 5; i > 0; i-- {
			fmt.Fprintf(os.Stderr, "%d... ", i)
			time.Sleep(time.Second)
		}
		fmt.Fprintln(os.Stderr)
	}

	root := *rootDir
	if root == "" && !*unsafe {
		cwd, err := os.Getwd()
		if err != nil {
			log.Fatalf("--root not set and getwd failed: %v", err)
		}
		root = cwd
		fmt.Fprintf(os.Stderr, "warning: --root not set, defaulting to CWD: %s\n", root)
	}

	sbCfg := agent.SandboxConfig{
		Root:      root,
		AllowExec: splitCSV(*allowExec),
		DenyExec:  splitCSV(*denyExec),
		Unsafe:    *unsafe,
	}

	if *elevate && !hasElevatedChild {
		if err := agent.RelaunchElevated(); err != nil {
			fmt.Fprintf(os.Stderr, "Elevation failed: %v\nContinuing without elevation.\n", err)
		}
	}

	if hasElevatedChild || agent.IsElevated() {
		fmt.Println("Running as: ELEVATED")
	} else if u, err := user.Current(); err == nil {
		fmt.Printf("Running as: %s (non-elevated)\n", u.Username)
	} else {
		fmt.Println("Running as: unknown (non-elevated)")
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
		BindIP:       *bindIP,
	}

	share := client.NewShareMode(cfg, *sshAddr, sbCfg)
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
	bindIP := fs.String("bind-ip", "", "Bind UDP to specific IP (bypass TUN proxy auto-detection)")
	mcpStdio := fs.Bool("mcp-stdio", false, "Run as MCP stdio server for Claude Code")
	legacySSH := fs.Bool("legacy-ssh", false, "Force original SSH tunnel mode (default if --mcp-stdio not set)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Help mode - assist someone else\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s help --code <code> [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}
	fs.Parse(args)

	if *code == "" && !*mcpStdio {
		fmt.Fprintf(os.Stderr, "Error: --code is required (or use --mcp-stdio for bootstrap mode where the code is supplied via Claude's connect tool)\n\n")
		fs.Usage()
		os.Exit(1)
	}

	if *mcpStdio && *legacySSH {
		fmt.Fprintln(os.Stderr, "Error: --mcp-stdio and --legacy-ssh are mutually exclusive")
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
		BindIP:       *bindIP,
	}

	if *mcpStdio {
		if *code == "" {
			boot := client.NewHelpMCPBootstrap(cfg)
			if err := boot.Run(context.Background()); err != nil {
				log.Fatalf("Error: %v", err)
			}
		} else {
			help := client.NewHelpModeMCP(cfg, *code)
			if err := help.Run(); err != nil {
				log.Fatalf("Error: %v", err)
			}
		}
	} else {
		help := client.NewHelpMode(cfg, *code, *listenAddr)
		if err := help.Run(); err != nil {
			log.Fatalf("Error: %v", err)
		}
	}

	fmt.Println("\nSession ended.")
}

// splitCSV 按逗号切分并去掉空字符串
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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
