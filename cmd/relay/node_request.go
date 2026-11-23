package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

// dispatchSubcommand 接管 relay 本身流程之外的子命令（目前只有 node-request）。
// 其余参数（包括 run、普通旗标、Windows 的 service）不接管，返回 handled=false。
func dispatchSubcommand(args []string, stdout, stderr io.Writer) (handled bool, exitCode int) {
	if len(args) > 0 && args[0] == "node-request" {
		return true, runNodeRequest(args[1:], stdout, stderr)
	}
	return false, 0
}

// runNodeRequest 实现 `relay node-request`：节点运营者用它生成入网申请，发给目录管理员（relay-admin sign-node）签发节点证书。
//
// 申请里只有 relay 的 TLS 证书（公开信息）、节点名称、运营者、地区、地址，以及用 TLS 私钥对这些内容的签名，
// 证明申请人确实掌握这台机器的证书私钥；不含任何秘密。换证书时再用旧证书私钥共签（--prev-cert/--prev-key）。
// 与启动 relay 不同，这里找不到证书时绝不自动生成：签出一张和线上正在用的不一致的证书毫无意义。
func runNodeRequest(args []string, stdout, stderr io.Writer) int {
	if err := nodeRequest(args, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "relay node-request: %v\n", err)
		return 1
	}
	return 0
}

func nodeRequest(args []string, stdout, stderr io.Writer) error {
	var certFile, keyFile, prevCertFile, prevKeyFile, certsDir string
	var name, operator, region, tcp, quic, out string
	certsDir = "./certs"

	fs := flag.NewFlagSet("node-request", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&certFile, "cert", "", "The relay's TLS certificate file (default: <certs-dir>/server.crt)")
	fs.StringVar(&keyFile, "key", "", "The relay's TLS key file (default: <certs-dir>/server.key)")
	fs.StringVar(&certsDir, "certs-dir", certsDir, "Directory holding server.crt and server.key when --cert/--key are not given. Must be the certificate the relay is actually serving: pass the relay's own --certs-dir (a Windows service keeps it in config.json as certs_dir)")
	fs.StringVar(&prevCertFile, "prev-cert", "", "Certificate the node is presenting right now; use when replacing the certificate. Requires --prev-key")
	fs.StringVar(&prevKeyFile, "prev-key", "", "Private key of --prev-cert; co-signs the request to prove you hold the node's current certificate")
	fs.StringVar(&name, "name", "", "Node name shown to users (required, no spaces)")
	fs.StringVar(&operator, "operator", "", "Operator shown to users (required, no spaces; do not put personal or internal information here)")
	fs.StringVar(&region, "region", "", "Region shown to users (optional, no spaces)")
	fs.StringVar(&tcp, "tcp", "", "Comma-separated host:port clients dial over TCP/TLS (required)")
	fs.StringVar(&quic, "quic", "", "Comma-separated host:port clients dial over QUIC/UDP (optional)")
	fs.StringVar(&out, "out", "", "Where to write the request (required; \"-\" for stdout). An existing file is never overwritten")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Generate a node enrollment request for the directory administrator.\n\n")
		fmt.Fprintf(stderr, "Usage:\n  %s node-request --name N --operator O --tcp HOST:PORT [--quic HOST:PORT] --out FILE [options]\n\nOptions:\n", os.Args[0])
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nThe request holds the certificate (public), the node details and a signature made with the\ncertificate's private key. It contains no secrets.\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if out == "" {
		return errors.New("--out is required (use - for stdout)")
	}
	if (certFile == "") != (keyFile == "") {
		return errors.New("--cert and --key must be specified together")
	}
	if (prevCertFile == "") != (prevKeyFile == "") {
		return errors.New("--prev-cert and --prev-key must be specified together")
	}
	if certFile == "" {
		certFile, keyFile = filepath.Join(certsDir, "server.crt"), filepath.Join(certsDir, "server.key")
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	var prev *tls.Certificate
	if prevCertFile != "" {
		p, err := tls.LoadX509KeyPair(prevCertFile, prevKeyFile)
		if err != nil {
			return fmt.Errorf("load previous TLS certificate: %w", err)
		}
		prev = &p
	}

	raw, err := directory.NewNodeRequest(cert, prev, directory.NodeRequest{
		Name:      name,
		Operator:  operator,
		Region:    region,
		TCP:       splitList(tcp),
		QUIC:      splitList(quic),
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if out == "-" {
		if _, err := stdout.Write(raw); err != nil {
			return err
		}
	} else if err := writeNewFile(out, raw); err != nil {
		return err
	}

	dest := out
	if out == "-" {
		dest = "stdout"
	}
	fmt.Fprintf(stderr, "Node enrollment request written to %s\n", dest)
	// 打印实际用的证书文件：证书目录选错（尤其 Windows 服务的 certs_dir 不是前台默认的 ./certs）时，运营者能一眼看出来。
	fmt.Fprintf(stderr, "  certificate file:        %s\n", certFile)
	fmt.Fprintf(stderr, "  certificate fingerprint: %s\n", directory.CertFingerprint(cert.Certificate[0]))
	if prev != nil {
		fmt.Fprintf(stderr, "  replaces certificate:    %s (%s)\n", directory.CertFingerprint(prev.Certificate[0]), prevCertFile)
	}
	fmt.Fprintf(stderr, "  name: %s  operator: %s  region: %s\n  tcp: %s  quic: %s\n", name, operator, region, tcp, quic)
	fmt.Fprintf(stderr, "The request contains no secrets. Send it to the directory administrator and keep the private key where it is.\n")
	return nil
}

// writeNewFile 只新建、不覆盖：申请文件是要发给管理员的，误覆盖会让人拿旧申请去签。
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}
