package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/client"
	"github.com/remote-assist/tool/internal/crypto"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/relay"
	"github.com/remote-assist/tool/internal/version"
)

// detectLANIPv4 找一个 non-loopback、non-down 的 IPv4 私网地址（用于 standalone 模式
// 提示用户告诉 Claude 哪个地址）。优先返回 192.168.* / 10.* / 172.16-31.*；
// 找不到私网地址时回退到第一个非环回 IPv4。空字符串表示完全找不到 IPv4。
func detectLANIPv4() string {
	ifaces, _ := net.Interfaces()
	var fallback string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil {
				continue
			}
			if v4[0] == 10 ||
				(v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) ||
				(v4[0] == 192 && v4[1] == 168) {
				return v4.String()
			}
			if fallback == "" {
				fallback = v4.String()
			}
		}
	}
	return fallback
}

func main() {
	// 无参数（典型场景：直接双击 exe，会弹出一个 console 窗口）默认进 share 协助端模式，
	// 方便非命令行用户一键起协助端、看协助码。命令行完整用法见 `remote --help`。
	if len(os.Args) < 2 {
		log.Printf("remote-assist %s", version.Info())
		fmt.Println("（无参数启动 → 默认 share 协助端模式；命令行用法见 `remote --help`）")
		runShare(nil)
		return
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
	case "upgrade-stage", "upgrade-finalize":
		if err := runUpgradeCommand(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "upgrade helper:", err)
			os.Exit(1)
		}
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runShare(args []string) {
	fs := flag.NewFlagSet("share", flag.ExitOnError)
	server := fs.String("server", defaultRelayServer(), "Relay server address (host 或 host:port；省略端口时默认 8443)")
	sshAddr := fs.String("ssh", "127.0.0.1:22", "Local SSH address")
	newInstance := fs.Bool("new-instance", false, "Start an additional independent share with a new assist code")
	insecure := fs.Bool("insecure", true, "Skip TLS verification (default true: built-in relay uses a self-signed cert). WARNING: default also skips verification for public/CA relays — transport identity is NOT authenticated; security then relies on tool-channel AEAD + SSH host-key. Use --insecure=false to enforce.")
	caFile := fs.String("ca", "", "CA certificate file")
	plain := fs.Bool("plain", false, "Use plain TCP (insecure, for dev only)")
	p2pMode := fs.String("p2p", "auto", "P2P mode: disabled, auto, required")
	stunServer := fs.String("stun", "", "STUN server address for P2P (default: same as relay:3478)")
	bindIP := fs.String("bind-ip", "", "Bind UDP to specific IP (bypass TUN proxy auto-detection)")
	rootDir := fs.String("root", "", "Optional: limit file tools to this subtree (empty = no limit). Guards against slips, NOT a security boundary — exec is not confined by it.")
	allowExec := fs.String("allow-exec", "", "Comma-separated exec basename allowlist (empty = no restriction beyond deny)")
	denyExec := fs.String("deny-exec", "rm,shutdown,reboot,mkfs,dd", "Comma-separated exec basename denylist")
	elevate := fs.Bool("elevate", false, "Windows: request UAC elevation on startup via ShellExecuteW runas")
	unsafeExec := fs.Bool("unsafe-exec", false, "DANGER: drop the exec deny/allow lists, letting any command run")
	standalone := fs.Bool("standalone", false, "Embed relay in-process and listen on --standalone-listen; share connects to loopback. For LAN-only scenarios where no external relay is available.")
	standaloneListen := fs.String("standalone-listen", ":8443", "Standalone mode: address relay listens on (use :port to listen on all interfaces)")
	noAuth := fs.Bool("no-auth", false, "Standalone mode: use a fixed code instead of random generation, so the help side needs no --code. DANGER: any device that can reach this relay can connect and control this machine. Use ONLY on a fully trusted private LAN.")
	codeFile := fs.String("code-file", "", "Write assist code + expiry as JSON to this file once registered (for host programs to read instead of parsing stdout)")
	codeFileMirror := fs.String("code-file-mirror", "", "Internal (make-before-break upgrade): additionally mirror the assist code JSON here, so a prior --code-file path keeps refreshing after an in-channel upgrade")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Share mode - allow others to assist you\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s share [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}
	// 以下是内部旗标，不注册为 flag，必须在 fs.Parse 之前预先剥离，否则
	// flag.ExitOnError 会因"flag provided but not defined"直接 os.Exit(2)。
	hasElevatedChild := false
	isUpgradeSuccessor := false
	clean := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "--elevated-child":
			hasElevatedChild = true
		case upgradeSuccessorFlag:
			isUpgradeSuccessor = true
		default:
			clean = append(clean, a)
		}
	}
	args = clean

	fs.Parse(args)
	rejectPositionalArgs(fs, "share")

	resolveTLSVerify(fs, insecure, *caFile)

	*server = client.NormalizeServerAddr(*server) // 无端口补默认 :8443（standalone 走 loopback 不受影响）

	if *unsafeExec {
		fmt.Fprint(os.Stderr, "\033[1;31m!!! DANGER: --unsafe-exec drops the exec deny/allow lists.\nAny command can run, including rm / shutdown / mkfs.\nAborting in 5 seconds — press Ctrl+C to abort.\033[0m\n")
		for i := 5; i > 0; i-- {
			fmt.Fprintf(os.Stderr, "%d... ", i)
			time.Sleep(time.Second)
		}
		fmt.Fprintln(os.Stderr)
	}

	// share 由本机用户主动发起、凭协助码授权，信任边界是“协助码交给谁”。--root 是可选的
	// 防手滑护栏，不是安全边界（exec 不受其约束），故默认不限制。
	if *rootDir == "" {
		fmt.Fprintln(os.Stderr, "File access: unrestricted (--root <dir> limits file tools to a subtree)")
	} else {
		fmt.Fprintf(os.Stderr, "File access: file tools limited to %s (exec is NOT limited by --root)\n", *rootDir)
	}

	sbCfg := agent.SandboxConfig{
		Root:       *rootDir,
		AllowExec:  splitCSV(*allowExec),
		DenyExec:   splitCSV(*denyExec),
		UnsafeExec: *unsafeExec,
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
	if !*newInstance {
		if isUpgradeSuccessor {
			// make-before-break：new 必须先上线，但 old 此时仍持有标准锁。交接门锁
			// 会挡住第三个默认 share，直到 new 在 old 退出后取得标准锁。
			handover, err := client.BeginShareInstanceLockHandover()
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			acquired := make(chan io.Closer, 1)
			defer func() {
				select {
				case instanceLock := <-acquired:
					_ = instanceLock.Close()
				default:
				}
			}()
			go func() {
				result := <-handover
				if result.Err != nil {
					log.Fatalf("Error: 接管 share 单实例锁失败: %v", result.Err)
				}
				acquired <- result.Lock
				// 拿到标准锁 == old 已退出，HOME/USERPROFILE 的升级隔离到此结束，
				// 还原成真实家目录，否则之后所有 exec 子进程的 ~ 都指向升级暂存目录。
				announceRestoredUpgradeHome()
			}()
		} else {
			instanceLock, err := client.AcquireShareInstanceLock()
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
			defer instanceLock.Close()
		}
	} else if isUpgradeSuccessor {
		// --new-instance 的继任者不参与锁交接，没有那个还原点。它也不需要隔离：
		// registrationClientID 在该模式下返回进程内随机 ID（share.go:374），与 HOME 无关，
		// 所以启动即可还原，不会和仍在运行的 old 撞 ClientID。
		announceRestoredUpgradeHome()
	}

	// Standalone (LAN) 模式：进程内启动 relay，share 连 loopback；
	// LAN 上其他人通过 LAN IP 直连这个 relay，不依赖任何外部服务器。
	if *standalone {
		_, listenPort, err := net.SplitHostPort(*standaloneListen)
		if err != nil || listenPort == "" {
			log.Fatalf("invalid --standalone-listen %q: %v", *standaloneListen, err)
		}
		// 进程内 relay 走 TLS（临时自签证书），让 help 端连 standalone 与连公网 relay
		// 命令完全一致（默认 --insecure=true 跳过自签校验），消除原先必须 --plain 的陷阱。
		if *plain {
			fmt.Fprintln(os.Stderr, "standalone mode: ignoring --plain; embedded relay uses TLS with a self-signed cert so the help side needs no special flag")
			*plain = false
		}
		certDir, err := os.MkdirTemp("", "remote-standalone-certs-")
		if err != nil {
			log.Fatalf("standalone: create temp cert dir failed: %v", err)
		}
		defer os.RemoveAll(certDir)
		certFile := filepath.Join(certDir, "server.crt")
		keyFile := filepath.Join(certDir, "server.key")
		if err := crypto.GenerateSelfSignedCert(certFile, keyFile); err != nil {
			os.RemoveAll(certDir) // log.Fatalf 会 os.Exit 跳过 defer，手动兜底清理
			log.Fatalf("standalone: generate self-signed cert failed: %v", err)
		}
		relayCfg := &relay.Config{
			ListenAddr:     *standaloneListen,
			UseTLS:         true,
			TLSCertFile:    certFile,
			TLSKeyFile:     keyFile,
			CodeTTL:        30 * time.Minute,
			CodeLength:     10,
			AuditLogFile:   "",
			STUNListenAddr: "", // 不启 STUN，LAN 下 P2P 没必要
			NoAuth:         *noAuth,
		}
		if *noAuth {
			fmt.Fprintln(os.Stderr, "\033[1;31m!!! WARNING: NO-AUTH mode enabled. Any device that can reach this relay can connect and fully control this machine (exec, read/write files). Use ONLY on a fully trusted private LAN.\033[0m")
		}
		relaySrv, err := relay.NewServer(relayCfg)
		if err != nil {
			os.RemoveAll(certDir) // log.Fatalf 会 os.Exit 跳过 defer，手动兜底清理
			log.Fatalf("standalone relay init failed: %v", err)
		}
		go func() {
			if err := relaySrv.StartWithContext(context.Background()); err != nil {
				os.RemoveAll(certDir) // 后台 goroutine 内 log.Fatalf 直接 os.Exit，主 goroutine 的 defer 不会执行，手动兜底清理
				log.Fatalf("standalone relay error: %v", err)
			}
		}()
		// 等 relay listener 就绪（NewServer 已经 bind 但 Accept 需要时间）
		time.Sleep(300 * time.Millisecond)

		// share 端连 loopback，走 TLS + insecure（自签证书）；外部 LAN 用户连 LAN IP
		*server = "127.0.0.1:" + listenPort
		*insecure = true      // 自签证书：share 与 help 两端都跳过校验
		*p2pMode = "disabled" // standalone 内部不用 P2P
		*stunServer = ""

		lanIP := detectLANIPv4()
		fmt.Println()
		fmt.Println("================ standalone (LAN) mode ================")
		if lanIP != "" {
			fmt.Printf("Relay (TLS, self-signed) listening at: %s:%s (LAN reachable)\n", lanIP, listenPort)
			if *noAuth {
				fmt.Printf("NO-AUTH mode: help side needs no --code:\n")
				fmt.Printf("    remote help --server %s:%s --no-auth --p2p disabled\n", lanIP, listenPort)
				fmt.Printf("Or tell Claude: connect(server=\"%s:%s\", no_auth=true)\n", lanIP, listenPort)
			} else {
				fmt.Printf("Help side connects exactly like a normal relay (no --plain needed):\n")
				fmt.Printf("    remote help --server %s:%s --code <code> --p2p disabled\n", lanIP, listenPort)
				fmt.Printf("Or tell Claude: \"协助码 <code> 在 %s:%s\" (connect tool overrides server).\n", lanIP, listenPort)
			}
			fmt.Printf("    (self-signed cert covers localhost only — on LAN keep the default --insecure=true; --insecure=false will fail)\n")
		} else {
			fmt.Printf("Relay (TLS, self-signed) listening at: 0.0.0.0:%s (LAN IP auto-detect failed; find this host's LAN IP manually)\n", listenPort)
		}
		fmt.Println("=======================================================")
		fmt.Println()
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

	share := client.NewShareMode(cfg, *sshAddr, *newInstance, sbCfg, *codeFile, *codeFileMirror)
	code, expiresAt, err := share.Run()
	if err != nil {
		log.Fatalf("Error: %v", err)
	}

	fmt.Printf("\nSession ended. Code %s expired at %s\n", code, expiresAt.Local().Format("2006-01-02 15:04:05"))
}

func runHelp(args []string) {
	fs := flag.NewFlagSet("help", flag.ExitOnError)
	server := fs.String("server", defaultRelayServer(), "Relay server address (host 或 host:port；省略端口时默认 8443)")
	code := fs.String("code", "", "Assist code (required)")
	listenAddr := fs.String("listen", "127.0.0.1:2222", "Local listen address")
	insecure := fs.Bool("insecure", true, "Skip TLS verification (default true: built-in relay uses a self-signed cert). WARNING: default also skips verification for public/CA relays — transport identity is NOT authenticated; security then relies on tool-channel AEAD + SSH host-key. Use --insecure=false to enforce.")
	caFile := fs.String("ca", "", "CA certificate file")
	plain := fs.Bool("plain", false, "Use plain TCP (insecure, for dev only)")
	p2pMode := fs.String("p2p", "auto", "P2P mode: disabled, auto, required")
	stunServer := fs.String("stun", "", "STUN server address for P2P (default: same as relay:3478)")
	bindIP := fs.String("bind-ip", "", "Bind UDP to specific IP (bypass TUN proxy auto-detection)")
	noAuthHelp := fs.Bool("no-auth", false, "Connect without an assist code (use with --no-auth share/relay). DANGER: any device that can reach the relay can connect. Use ONLY on a fully trusted private LAN.")
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
	rejectPositionalArgs(fs, "help")

	resolveTLSVerify(fs, insecure, *caFile)

	if *noAuthHelp {
		fmt.Fprintln(os.Stderr, "\033[1;31m!!! WARNING: NO-AUTH mode. Any device that can reach the relay can connect and control the remote share side. Use ONLY on a fully trusted private LAN.\033[0m")
		if *code == "" {
			*code = proto.NoAuthCode
		}
	}

	if *code == "" && !*mcpStdio {
		fmt.Fprintf(os.Stderr, "Error: --code is required (or use --mcp-stdio for bootstrap mode where the code is supplied via Claude's connect tool, or use --no-auth for trusted LANs)\n\n")
		fs.Usage()
		os.Exit(1)
	}

	if *mcpStdio && *legacySSH {
		fmt.Fprintln(os.Stderr, "Error: --mcp-stdio and --legacy-ssh are mutually exclusive")
		os.Exit(1)
	}

	*server = client.NormalizeServerAddr(*server) // 无端口补默认 :8443

	// Derive STUN server if not specified
	if *stunServer == "" && *server != "" {
		host, _, _ := net.SplitHostPort(*server)
		if host != "" {
			*stunServer = net.JoinHostPort(host, "3478")
		}
	}

	// 私网/loopback relay（standalone / 同 LAN）下 auto 跳过 P2P：relay 已直连、P2P 多余，
	// 且 standalone 不启 STUN 必然打洞超时。required 仍尊重用户意图。
	if *p2pMode == "auto" && client.IsLANServer(*server) {
		fmt.Fprintf(os.Stderr, "P2P: server %s 为私网/loopback，relay 已 LAN 直连，跳过 P2P（auto→disabled；如需强制用 --p2p required）\n", *server)
		*p2pMode = "disabled"
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
				fmt.Fprintf(os.Stderr, "\n连接中断: %v\nMCP 通道已断开。请让 Claude 重新调用 connect 工具重连（或重启此 MCP server）。\n", err)
				os.Exit(1)
			}
		} else {
			help := client.NewHelpModeMCP(cfg, *code)
			if err := help.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "\n连接中断: %v\nMCP 通道已断开。请让 Claude 重新调用 connect 工具重连（或重启此 MCP server）。\n", err)
				os.Exit(1)
			}
		}
		return
	} else {
		help := client.NewHelpMode(cfg, *code, *listenAddr)
		if err := help.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "\n连接中断: %v\n协助会话已结束。如需继续，请重新运行 remote help（SSH 隧道断开后原 ssh 会话也需重连）。\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("\nSession ended.")
}

// rejectPositionalArgs 在解析后拦下任何多余的位置参数。
//
// Go 的 flag 在第一个非旗标处停止解析，剩下的全落进 fs.Args()，默认被静默忽略——
// 而这里的旗标大多是安全护栏（--root / --allow-exec / --deny-exec）。一旦命令行被
// 意外撕开（历史上 Windows --elevate 的空格拼接就会这样，见 internal/agent/winargs.go），
// 表现是"提权成功了，但白名单悄悄变成空"，也就是放行一切。宁可报错退出，也不能让
// 护栏无声消失。
func rejectPositionalArgs(fs *flag.FlagSet, mode string) {
	if fs.NArg() == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "Error: unexpected argument %q after the %s options: %v\n",
		fs.Arg(0), mode, fs.Args())
	fmt.Fprintf(os.Stderr, "Options must all start with '-'; a value containing spaces needs quoting.\n"+
		"Refusing to continue: flags after this point were ignored, which can silently disable --root / --allow-exec / --deny-exec.\n\n")
	fs.Usage()
	os.Exit(2)
}

// resolveTLSVerify 调和 --insecure 与 --ca。
//
// --insecure 默认 true 是为了让自签证书的内置/standalone relay 开箱即用（见 0dac99c），
// 这个默认值本身要保留：relay 自动生成的自签证书 SAN 只含 localhost/127.0.0.1，而客户端
// 默认连的是裸 IP，翻转默认值会让工具开箱即坏。
//
// 但用户显式传了 --ca 就已经明确表达"我要校验"，此时继续沿用 insecure 的默认值会让
// RootCAs 被 InsecureSkipVerify 静默架空。这里按意图调和：未显式传 --insecure 时自动
// 开启校验；显式传了 --insecure=true 又给 --ca 属自相矛盾，报错而不是二选一地静默忽略。
func resolveTLSVerify(fs *flag.FlagSet, insecure *bool, caFile string) {
	if caFile == "" {
		return
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "insecure" {
			explicit = true
		}
	})
	if explicit {
		if *insecure {
			fmt.Fprintf(os.Stderr, "Error: --ca conflicts with --insecure=true; specifying a CA means certificates must be verified. Drop one of them (--insecure=false enforces verification).\n")
			os.Exit(1)
		}
		return
	}
	*insecure = false
	fmt.Fprintf(os.Stderr, "--ca given: enabling certificate verification (implies --insecure=false)\n")
}

// defaultRelayServerAddr 是内置的公网 relay 地址，未显式 --server 时使用。
// 可用环境变量 REMOTE_RELAY_SERVER 覆盖，避免换 IP 时重新编译。
const defaultRelayServerAddr = "23.95.78.14:8443"

func defaultRelayServer() string {
	if v := strings.TrimSpace(os.Getenv("REMOTE_RELAY_SERVER")); v != "" {
		return v
	}
	return defaultRelayServerAddr
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
