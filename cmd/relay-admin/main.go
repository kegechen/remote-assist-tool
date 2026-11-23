// Command relay-admin 是节点列表的离线签名工具：生成根密钥，签发状态密钥授权、节点证书与吊销列表，
// 以及逐层验签查看任意签名对象。只在管理员本机构建和运行，不进 release 产物。
//
// 设计见 docs/design/specs/2026-11-23-multi-relay-directory-design.md 第 3、9 节。
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/remote-assist/tool/internal/directory"
)

const usage = `用法: relay-admin [--dir 管理目录] <命令> [参数]

命令:
  keygen                          生成主、备根密钥（各自设口令）
  status-key new                  生成状态密钥并用根密钥授权
  sign-node <申请文件>            核对入网申请并签发节点证书
  revoke --node-serial N[,N...]   吊销节点证书
  revoke --root primary           用备用根吊销主根（备用根只能发版换掉）
  revoke --resign                 不新增吊销，把现有的全部吊销换一把根重签
  inspect <文件或 URL>            逐层验签并打印内容（按台账里的吊销核对）

管理目录默认是 %USERPROFILE%\.remote-assist-admin（类 Unix 上是 ~/.remote-assist-admin）。
每个命令加 -h 查看参数。
`

type app struct {
	in      *bufio.Reader
	console *os.File // 交互式输入来源；nil 表示输入来自管道/测试，按行读取
	out     io.Writer
	errOut  io.Writer
	now     func() time.Time
	dir     string
	logN    uint8 // 新建根密钥文件用的 scrypt 参数

	// fetchFingerprint 实际连接 addr，返回 relay 出示的证书指纹。network 是 "tcp" 或 "quic"。
	fetchFingerprint func(ctx context.Context, network, addr string) (string, error)
}

func main() {
	a := &app{
		in:               bufio.NewReader(os.Stdin),
		console:          os.Stdin,
		out:              os.Stdout,
		errOut:           os.Stderr,
		now:              time.Now,
		logN:             defaultScryptLogN,
		fetchFingerprint: fetchFingerprint,
	}
	if err := a.run(os.Args[1:]); err != nil {
		printError(os.Stderr, err)
		os.Exit(1)
	}
}

// printError 把错误打成一行。错误文本可能带着对端、申请文件里的任意内容，控制字符、双向控制等不可显示字符
// 一律转义，换行也转义，免得伪造出看似另一条提示的输出。
func printError(w io.Writer, err error) {
	fmt.Fprintln(w, "错误:", escapeText(err.Error()))
}

// escapeText 原样保留可显示字符（含中文）和 ASCII 空格，其余按 Go 字符串字面量的写法转义。
// 反斜杠不转义：错误里常有 Windows 路径，换行已经转义，单独的反斜杠伪造不出新的一行。
func escapeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsPrint(r):
			b.WriteString(s[i : i+size])
		default:
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String()
}

func defaultAdminDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".remote-assist-admin"
	}
	return filepath.Join(home, ".remote-assist-admin")
}

func (a *app) run(args []string) error {
	if a.dir == "" {
		a.dir = defaultAdminDir()
	}
	for len(args) >= 2 && (args[0] == "--dir" || args[0] == "-dir") {
		a.dir, args = args[1], args[2:]
	}
	if len(args) == 0 {
		io.WriteString(a.errOut, usage)
		return errors.New("缺少命令")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "keygen":
		return a.cmdKeygen(rest)
	case "status-key":
		if len(rest) == 0 || rest[0] != "new" {
			return errors.New("用法: relay-admin status-key new [参数]")
		}
		return a.cmdStatusKeyNew(rest[1:])
	case "sign-node":
		return a.cmdSignNode(rest)
	case "revoke":
		return a.cmdRevoke(rest)
	case "inspect":
		return a.cmdInspect(rest)
	case "-h", "--help", "help":
		io.WriteString(a.out, usage)
		return nil
	default:
		io.WriteString(a.errOut, usage)
		return fmt.Errorf("不认识的命令 %q", cmd)
	}
}

func (a *app) printf(format string, args ...any) { fmt.Fprintf(a.out, format, args...) }

// readLine 读一行，去掉行尾换行。
func (a *app) readLine() (string, error) {
	line, err := a.in.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		if err == io.EOF {
			return "", errors.New("输入已结束")
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readSecret 读口令：在终端上关闭回显；读到 Ctrl+C 时先恢复终端再退出。
func (a *app) readSecret(prompt string) (string, error) {
	fmt.Fprint(a.errOut, prompt)
	if a.console != nil {
		if restore, ok := disableEcho(a.console); ok {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			done := make(chan struct{})
			go func() {
				select {
				case <-sig:
					restore()
					fmt.Fprintln(a.errOut)
					os.Exit(130)
				case <-done:
				}
			}()
			defer func() {
				close(done)
				signal.Stop(sig)
				restore()
				fmt.Fprintln(a.errOut)
			}()
		} else {
			fmt.Fprint(a.errOut, "\n（警告：当前终端无法关闭回显，输入的口令会显示在屏幕上；建议在 PowerShell 或 Windows Terminal 里运行）\n> ")
		}
	}
	return a.readLine()
}

// newPassphrase 让用户为 label 设置口令：输两遍、至少 minPassphraseLen 个字符，最多试三次。
func (a *app) newPassphrase(label string) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		p1, err := a.readSecret(fmt.Sprintf("为%s设置口令（至少 %d 个字符）: ", label, minPassphraseLen))
		if err != nil {
			return "", err
		}
		if len([]rune(p1)) < minPassphraseLen {
			fmt.Fprintf(a.errOut, "口令太短，至少 %d 个字符。\n", minPassphraseLen)
			continue
		}
		p2, err := a.readSecret("再输一遍: ")
		if err != nil {
			return "", err
		}
		if p1 != p2 {
			fmt.Fprintln(a.errOut, "两次输入不一致。")
			continue
		}
		return p1, nil
	}
	return "", errors.New("三次都没设置成功，已放弃")
}

// confirm 打印 prompt，只有输入 yes 才返回 nil。
func (a *app) confirm(prompt string) error {
	fmt.Fprintf(a.errOut, "%s 输入 yes 继续: ", prompt)
	line, err := a.readLine()
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != "yes" {
		return errors.New("已取消")
	}
	return nil
}

func rootLabel(name string) string {
	if name == directory.SignerBackup {
		return "备用根密钥"
	}
	return "主根密钥"
}

func (a *app) keyPath(name string) string { return filepath.Join(a.dir, "root-"+name+".key") }
func (a *app) pubPath(name string) string { return filepath.Join(a.dir, "root-"+name+".pub") }
func (a *app) issuedDir() string          { return filepath.Join(a.dir, "issued") }

func validRootName(name string) error {
	if name != directory.SignerPrimary && name != directory.SignerBackup {
		return fmt.Errorf("根密钥只有 primary 和 backup 两把，不认识 %q", name)
	}
	return nil
}

// loadRoot 读出并解密一把根私钥（会提示输入口令），并核对它与 .pub 文件一致。
func (a *app) loadRoot(name string) (ed25519.PrivateKey, error) {
	if err := validRootName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(a.keyPath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && name == directory.SignerBackup {
			return nil, fmt.Errorf("找不到 %s：备用根密钥平时放在离线介质上，用前先拷回管理目录，用完再删掉", a.keyPath(name))
		}
		return nil, err
	}
	pass, err := a.readSecret(fmt.Sprintf("输入%s的口令: ", rootLabel(name)))
	if err != nil {
		return nil, err
	}
	priv, err := openKey(data, name, pass)
	if err != nil {
		return nil, err
	}
	if pub, err := readPub(a.pubPath(name)); err == nil && !pub.Equal(priv.Public()) {
		return nil, fmt.Errorf("%s 与私钥不符，管理目录可能被改过", a.pubPath(name))
	}
	return priv, nil
}

func readPub(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s 不是 ed25519 公钥", path)
	}
	return ed25519.PublicKey(b), nil
}

// rootFP 返回管理目录里某把根公钥的指纹。吊销按公钥指纹记，不按名字：发版换掉的新公钥不受旧吊销影响。
func (a *app) rootFP(name string) (string, error) {
	if err := validRootName(name); err != nil {
		return "", err
	}
	pub, err := readPub(a.pubPath(name))
	if err != nil {
		return "", fmt.Errorf("读不到%s公钥（%w）：管理目录不完整，请从备份恢复", rootLabel(name), err)
	}
	return directory.RootFingerprint(pub), nil
}

// checkRootUsable 在要口令之前按台账拒绝已被吊销的根；可用时返回它的公钥指纹。
func (a *app) checkRootUsable(l *ledger, name, action string) (string, error) {
	fp, err := a.rootFP(name)
	if err != nil {
		return "", err
	}
	if l.rootRevoked(fp) {
		return "", fmt.Errorf("%s已被吊销，不能再用它%s", rootLabel(name), action)
	}
	return fp, nil
}

// roots 返回验签用的根：管理目录里的 .pub 优先，没有就用编进程序的内置根。
func (a *app) roots(builtin bool) (directory.Roots, string, error) {
	if !builtin {
		keys := map[string]ed25519.PublicKey{}
		for _, name := range []string{directory.SignerPrimary, directory.SignerBackup} {
			if pub, err := readPub(a.pubPath(name)); err == nil {
				keys[name] = pub
			}
		}
		if len(keys) > 0 {
			return directory.Roots{Keys: keys}, "管理目录里的根公钥", nil
		}
	}
	r := directory.BuiltinRoots()
	if len(r.Keys) == 0 {
		if builtin {
			return r, "", errors.New("程序里还没内置根公钥")
		}
		return r, "", errors.New("没有可用的根公钥：管理目录里没有 .pub 文件，程序里也还没内置")
	}
	return r, "程序内置的根公钥", nil
}

// writeFileAtomic 写同目录临时文件、fsync 后 rename，权限 0600。
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	done = true
	return nil
}

// writeNewFile 只在文件不存在时创建（0600），绝不覆盖已有的密钥或签发记录。
// 写入、刷盘、关闭任何一步失败都删掉文件：调用方收到错误就当没签发，不能留下一份已签名、台账却不知道的文件。
func writeNewFile(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close() // 先关再删：Windows 上删不掉打开着的文件
			os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
