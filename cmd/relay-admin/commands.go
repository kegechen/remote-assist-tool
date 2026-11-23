package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

const (
	defaultValidDays = 365
	maxValidDays     = 2 * 365
	dialTimeout      = 10 * time.Second
)

func (a *app) flags(name, usageLine string) *flag.FlagSet {
	fset := flag.NewFlagSet(name, flag.ContinueOnError)
	fset.SetOutput(a.errOut)
	fset.Usage = func() {
		fmt.Fprintf(a.errOut, "用法: relay-admin %s\n", usageLine)
		fset.PrintDefaults()
	}
	return fset
}

// parseFlags 解析参数；-h 时返回 help=true，调用方直接结束、不报错。
func parseFlags(fset *flag.FlagSet, args []string) (help bool, err error) {
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func formatTime(unix int64) string { return time.Unix(unix, 0).Local().Format("2006-01-02 15:04") }

func indentJSON(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func validDays(days int) (time.Duration, error) {
	if days < 1 || days > maxValidDays {
		return 0, fmt.Errorf("有效期必须在 1 到 %d 天之间", maxValidDays)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// ---- keygen ----

func (a *app) cmdKeygen(args []string) error {
	fset := a.flags("keygen", "keygen")
	if help, err := parseFlags(fset, args); help || err != nil {
		return err
	}
	if fset.NArg() != 0 {
		return errors.New("keygen 不接受参数")
	}
	names := []string{directory.SignerPrimary, directory.SignerBackup}
	for _, n := range names {
		for _, p := range []string{a.keyPath(n), a.pubPath(n)} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s 已存在，拒绝覆盖。确实要重新生成，先把整个管理目录移走备份", p)
			}
		}
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return err
	}
	// 先把两把口令都收齐再写文件，避免中途放弃时只留下一把。
	passes := map[string]string{}
	for _, n := range names {
		p, err := a.newPassphrase(rootLabel(n))
		if err != nil {
			return err
		}
		passes[n] = p
	}
	pubs := map[string]string{}
	for _, n := range names {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		sealed, err := sealKey(n, priv, passes[n], a.logN)
		if err != nil {
			return err
		}
		if err := writeNewFile(a.keyPath(n), sealed); err != nil {
			return err
		}
		pubs[n] = base64.StdEncoding.EncodeToString(pub)
		if err := writeNewFile(a.pubPath(n), []byte(pubs[n]+"\n")); err != nil {
			return err
		}
	}
	if _, err := os.Stat(a.ledgerPath()); errors.Is(err, fs.ErrNotExist) {
		if err := a.saveLedger(&ledger{}); err != nil {
			return err
		}
	}

	a.printf("已生成根密钥，文件都在 %s：\n", a.dir)
	a.printf("  主根私钥    %s（口令加密）\n", a.keyPath(directory.SignerPrimary))
	a.printf("  主根公钥    %s\n", a.pubPath(directory.SignerPrimary))
	a.printf("  备用根私钥  %s（口令加密）\n", a.keyPath(directory.SignerBackup))
	a.printf("  备用根公钥  %s\n", a.pubPath(directory.SignerBackup))
	a.printf("  台账        %s\n", a.ledgerPath())
	a.printf("\n公钥（要编进 internal/directory/roots.go）：\n")
	a.printf("  primary  %s\n", pubs[directory.SignerPrimary])
	a.printf("  backup   %s\n", pubs[directory.SignerBackup])
	a.printf("\n请马上备份：\n")
	a.printf("  1. 把整个 %s 目录拷到离线介质（U 盘、密码管理器）。\n", a.dir)
	a.printf("  2. 备份确认无误后，从本机删掉 %s；要用时再拷回来，用完再删。\n", a.keyPath(directory.SignerBackup))
	a.printf("  3. 两把口令分别记好。忘了口令等于丢了密钥：没法再新增节点或更换状态密钥，只能发一个内置新公钥的客户端版本。\n")
	return nil
}

// ---- status-key new ----

func (a *app) cmdStatusKeyNew(args []string) error {
	fset := a.flags("status-key new", "status-key new [--root primary|backup] [--valid 天数]")
	root := fset.String("root", directory.SignerPrimary, "用哪把根密钥授权")
	days := fset.Int("valid", defaultValidDays, "授权有效天数")
	if help, err := parseFlags(fset, args); help || err != nil {
		return err
	}
	if fset.NArg() != 0 {
		return errors.New("status-key new 不接受位置参数")
	}
	if err := validRootName(*root); err != nil {
		return err
	}
	valid, err := validDays(*days)
	if err != nil {
		return err
	}
	l, err := a.loadLedger()
	if err != nil {
		return err
	}
	if _, err := a.checkRootUsable(l, *root, "授权"); err != nil {
		return err
	}
	rootPriv, err := a.loadRoot(*root)
	if err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	now := a.now()
	// 吊销根时声明过状态密钥序号下限：新授权的序号不能低于它，否则客户端会当作已作废的旧密钥。
	last := l.LastStatusSerial
	if l.MinStatusSerial > 0 {
		last = max(last, l.MinStatusSerial-1)
	}
	serial := timeSerial(last, now)
	env, err := directory.IssueStatusKey(rootPriv, *root, directory.StatusKeyCert{
		Serial: serial, PubKey: pub, NotBefore: now.Unix(), NotAfter: now.Add(valid).Unix(),
	})
	if err != nil {
		return err
	}
	certJSON, err := indentJSON(env)
	if err != nil {
		return err
	}
	// Worker 用 WebCrypto 导入 Ed25519 私钥，JWK 是它直接支持、又能自描述的格式。
	secretJSON, err := indentJSON(map[string]string{
		"kty": "OKP", "crv": "Ed25519",
		"d": base64.RawURLEncoding.EncodeToString(priv.Seed()),
		"x": base64.RawURLEncoding.EncodeToString(pub),
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.issuedDir(), 0o700); err != nil {
		return err
	}
	certPath := filepath.Join(a.issuedDir(), fmt.Sprintf("status-key-%d.json", serial))
	secretPath := filepath.Join(a.issuedDir(), fmt.Sprintf("status-key-%d.secret.jwk", serial))
	if err := writeNewFile(certPath, certJSON); err != nil {
		return err
	}
	if err := writeNewFile(secretPath, secretJSON); err != nil {
		os.Remove(certPath)
		return err
	}
	l.LastStatusSerial = serial
	if err := a.commitLedger(l, certPath, secretPath); err != nil {
		return err
	}

	a.printf("已生成状态密钥（序号 %d，%s签发，有效期至 %s）：\n", serial, rootLabel(*root), formatTime(now.Add(valid).Unix()))
	a.printf("  授权  %s\n", certPath)
	a.printf("  私钥  %s\n", secretPath)
	a.printf("\n下一步：\n")
	a.printf("  1. 在 Worker 项目的 relay-directory 目录里上传私钥：\n")
	a.printf("       wrangler secret put STATUS_KEY_JWK < \"%s\"\n", secretPath)
	a.printf("  2. 在管理页上传授权文件 %s。\n", certPath)
	a.printf("  3. 上传成功后删除私钥文件。丢了也没关系，重新运行 status-key new 即可。\n")
	a.printf("客户端见过这份授权后，就不再接受序号更低的旧状态密钥签的列表。\n")
	return nil
}

// ---- sign-node ----

func readFileLimited(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s 超过 %d 字节", path, limit)
	}
	return data, nil
}

// randomSerial 返回 [1, 2^53) 内、台账里没用过的随机序号。限制在 2^53 以内，是因为 Worker 用 JS 解析 JSON，
// 超过这个范围的整数会丢精度。
func randomSerial(l *ledger) (uint64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		s := binary.BigEndian.Uint64(b[:]) & (1<<53 - 1)
		if s != 0 && !slices.ContainsFunc(l.Nodes, func(n ledgerNode) bool { return n.Serial == s }) {
			return s, nil
		}
	}
}

func (a *app) cmdSignNode(args []string) error {
	fset := a.flags("sign-node", "sign-node [--prefix X | --renew X] [--official] [--valid 天数] [--root primary|backup] <申请文件>")
	prefixFlag := fset.String("prefix", "", "指定前缀（单个协助码字符）；不指定则分配第一个空闲的")
	renew := fset.String("renew", "", "给已有前缀换证书或续期：允许节点当前仍出示该前缀旧证书的指纹")
	official := fset.Bool("official", false, "标为官方节点（自己运营的节点才加）")
	days := fset.Int("valid", defaultValidDays, "证书有效天数")
	root := fset.String("root", directory.SignerPrimary, "用哪把根密钥签")
	if help, err := parseFlags(fset, args); help || err != nil {
		return err
	}
	if fset.NArg() != 1 {
		fset.Usage()
		return errors.New("需要一个申请文件")
	}
	if *prefixFlag != "" && *renew != "" {
		return errors.New("--prefix 和 --renew 只能选一个")
	}
	if err := validRootName(*root); err != nil {
		return err
	}
	valid, err := validDays(*days)
	if err != nil {
		return err
	}
	raw, err := readFileLimited(fset.Arg(0), directory.MaxRequestSize)
	if err != nil {
		return err
	}
	req, err := directory.VerifyNodeRequest(raw)
	if err != nil {
		return fmt.Errorf("申请无效: %w", err)
	}
	fp := req.Fingerprint
	if req.PrevFingerprint != "" && *renew == "" {
		return errors.New("这是换证书申请（带旧证书共签），请用 --renew <前缀>")
	}
	l, err := a.loadLedger()
	if err != nil {
		return err
	}
	rootFP, err := a.checkRootUsable(l, *root, "签发")
	if err != nil {
		return err
	}
	// 吊销的是那把 TLS 私钥：被宣告泄露的私钥既不能再签进新证书，也不能拿来共签、证明「掌握现有证书」。
	// 按公钥查，同一把私钥另签的证书（指纹不同）也认得出。申请里的证书都是 VerifyNodeRequest 解析过的。
	keys := map[string]string{} // 指纹 → 公钥哈希
	for _, der := range [][]byte{req.CertDER, req.PrevCertDER} {
		if len(der) == 0 {
			continue // 普通申请没有旧证书
		}
		key, err := certKeyHash(der)
		if err != nil {
			return fmt.Errorf("申请里的证书解析失败: %w", err)
		}
		f := directory.CertFingerprint(der)
		keys[f] = key
		if revoked := l.leakedKeyCerts(key); revoked != nil {
			return fmt.Errorf("申请里的证书（指纹 %s）用的私钥已随证书吊销（序号 %v）：这把 TLS 私钥不再可信，"+
				"节点必须用新生成的私钥签一张证书，再重新申请", f, revoked)
		}
	}
	reqKeys := slices.Collect(maps.Values(keys))

	// 定前缀，以及节点此刻可以出示哪些证书。签进证书的每个指纹都必须有「申请人掌握其私钥」的证明：
	// 新证书由申请签名证明，旧证书由换证书申请的共签证明。只凭「台账里有这个旧指纹」不够——
	// 节点的名称和地址都是公开的，谁都能照抄，再拿自己的密钥冒名换证书。
	allowed := map[string]bool{fp: true}
	var prefix string
	switch {
	case *renew != "":
		prefix = strings.ToUpper(*renew)
		if !directory.ValidPrefix(prefix) {
			return fmt.Errorf("%q 不是合法前缀", *renew)
		}
		existing := l.activeWithPrefix(prefix)
		if len(existing) == 0 {
			return fmt.Errorf("台账里没有前缀 %s 的有效证书，不能续签；新节点用 --prefix", prefix)
		}
		// 申请人必须证明自己掌握该前缀「最新那张」证书的私钥：要么申请就是用它签的（单纯续期），
		// 要么由它共签（换证书）。否则冒名者换个自己的地址也能把前缀抢走。
		// 只认最新的一张：被取代的旧证书私钥可能留在退役机器或旧备份里，不能凭它续签。
		latest := l.latestCert(prefix)
		if latest == nil {
			return fmt.Errorf("前缀 %s 最新的证书已被吊销：节点的哪把旧私钥都可能已经泄露，不能再用 --renew。"+
				"先 revoke 该前缀其余仍有效的证书（序号 %v），再用 --prefix %s 重新签发", prefix, serialsOf(existing), prefix)
		}
		switch {
		case req.PrevFingerprint != "" && !slices.Contains(latest.TLSFPs, req.PrevFingerprint):
			return fmt.Errorf("申请里共签的旧证书（指纹 %s）不是前缀 %s 最新证书（序号 %d）里的", req.PrevFingerprint, prefix, latest.Serial)
		case req.PrevFingerprint == "" && !slices.Contains(latest.TLSFPs, fp):
			return fmt.Errorf("申请人没有证明自己掌握前缀 %s 最新证书（序号 %d）的私钥：换证书申请必须由节点当前证书的私钥共签"+
				"（relay node-request 加 --prev-cert/--prev-key）。旧私钥确实丢了，就先 revoke 该前缀全部有效证书（序号 %v），再用 --prefix %s 重新签发",
				prefix, latest.Serial, serialsOf(existing), prefix)
		}
		if req.PrevFingerprint != "" {
			allowed[req.PrevFingerprint] = true
		}
		if latest.Name != req.Name || latest.Operator != req.Operator {
			a.printf("注意：名称/运营者由 %q / %q 改为 %q / %q。\n", latest.Name, latest.Operator, req.Name, req.Operator)
		}
		if latest.Official && !*official {
			a.printf("注意：前缀 %s 现在是官方节点，这次没带 --official，新证书将不再是官方节点。\n", prefix)
		}
		// 前缀挪到别的地址是续签里最需要看清的变化：确认是节点本人迁移，而不是有人借续签把前缀引走。
		if !sameSet(latest.TCP, req.TCP) || !sameSet(latest.QUIC, req.QUIC) {
			a.printf("注意：地址有变化，前缀 %s 将指向新地址，请确认这是节点本人的迁移：\n", prefix)
			a.printf("  TCP   %q → %q\n", latest.TCP, req.TCP)
			a.printf("  QUIC  %q → %q\n", latest.QUIC, req.QUIC)
		}
	case *prefixFlag != "":
		prefix = strings.ToUpper(*prefixFlag)
		if !directory.ValidPrefix(prefix) {
			return fmt.Errorf("%q 不是合法前缀，可用字符: %s", *prefixFlag, directory.CodeCharset)
		}
		if used := l.activeWithPrefix(prefix); len(used) > 0 {
			return fmt.Errorf("前缀 %s 已被 %q 占用（证书序号 %d）；给同一节点换证书请用 --renew %s", prefix, used[0].Name, used[0].Serial, prefix)
		}
	default:
		for _, c := range directory.CodeCharset {
			if len(l.activeWithPrefix(string(c))) == 0 {
				prefix = string(c)
				break
			}
		}
		if prefix == "" {
			return errors.New("没有空闲前缀了")
		}
	}
	// 同一把私钥不能用在两个前缀上：持有它的一方能同时冒充两个节点，吊销其中一个也会连带另一个。
	// 同一张证书挂两个前缀更不行（两者放进同一份列表时，客户端会整份拒绝）。
	for _, n := range l.certsTrustingKeys(reqKeys, nil) {
		if n.Prefix != prefix {
			// 不能引导去吊销那张证书：吊销会把这把私钥永久作废（见 leakedKeyCerts），照样签不进新前缀。
			return fmt.Errorf("申请里证书的私钥已用于前缀 %s 的证书（序号 %d）；同一把私钥不能用在两个前缀上，"+
				"节点须用新生成的私钥签一张证书，再重新申请", n.Prefix, n.Serial)
		}
	}

	// 实际连一次每个地址，核对节点出示的证书。
	a.printf("核对节点实际出示的证书：\n")
	fps := []string{fp}
	type target struct{ network, addr string }
	var targets []target
	for _, addr := range req.TCP {
		targets = append(targets, target{"tcp", addr})
	}
	for _, addr := range req.QUIC {
		targets = append(targets, target{"quic", addr})
	}
	for _, t := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
		got, err := a.fetchFingerprint(ctx, t.network, t.addr)
		cancel()
		if err != nil {
			// 错误文本可能含对端发来的内容（例如 QUIC 关闭原因），转义后再显示。
			return fmt.Errorf("连不上 %s %s: %q（节点必须能从这里连通才能签发）", t.network, t.addr, err.Error())
		}
		if !allowed[got] {
			return fmt.Errorf("%s %s 出示的证书指纹 %s 与申请里的 %s 不符", t.network, t.addr, got, fp)
		}
		a.printf("  %-4s %s  指纹 %s  一致\n", t.network, t.addr, got)
		if !slices.Contains(fps, got) {
			fps = append(fps, got)
		}
	}
	if len(fps) > directory.MaxTLSFPs {
		return fmt.Errorf("各地址出示的证书不止两张（%v），先让节点统一证书", fps)
	}

	serial, err := randomSerial(l)
	if err != nil {
		return err
	}
	now := a.now()
	cert := directory.NodeCert{
		Serial: serial, Prefix: prefix, Name: req.Name, Operator: req.Operator, Official: *official,
		Region: req.Region, TCP: req.TCP, QUIC: req.QUIC, TLSFPs: fps,
		NotBefore: now.Unix(), NotAfter: now.Add(valid).Unix(),
	}
	if err := directory.ValidateNodeCert(&cert); err != nil {
		return err
	}
	a.printf("\n将签发节点证书：\n")
	a.printf("  前缀      %s\n", prefix)
	a.printf("  名称      %q\n", cert.Name)
	a.printf("  运营者    %q\n", cert.Operator)
	a.printf("  地区      %q\n", cert.Region)
	a.printf("  官方节点  %v\n", cert.Official)
	a.printf("  TCP       %v\n", cert.TCP)
	a.printf("  QUIC      %v\n", cert.QUIC)
	a.printf("  证书指纹  %v\n", cert.TLSFPs)
	a.printf("  有效期    %s 至 %s\n", formatTime(cert.NotBefore), formatTime(cert.NotAfter))
	a.printf("  证书序号  %d\n", serial)
	a.printf("  签名根    %s\n", *root)
	if !cert.Official {
		a.printf("提醒：节点能看到经它中转的会话（见 SR-005），只给你信任的运营者签发。\n")
	}
	if err := a.confirm("确认签发？"); err != nil {
		return err
	}
	rootPriv, err := a.loadRoot(*root)
	if err != nil {
		return err
	}
	env, err := directory.IssueNodeCert(rootPriv, *root, cert)
	if err != nil {
		return err
	}
	data, err := indentJSON(env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.issuedDir(), 0o700); err != nil {
		return err
	}
	path := filepath.Join(a.issuedDir(), fmt.Sprintf("node-%s-%d.json", prefix, serial))
	if err := writeNewFile(path, data); err != nil {
		return err
	}
	// 签进证书的指纹都来自申请（allowed 只含 fp 与共签的旧证书），公钥哈希都已算好。
	var tlsKeys []string
	for _, f := range cert.TLSFPs {
		tlsKeys = append(tlsKeys, keys[f])
	}
	l.Nodes = append(l.Nodes, ledgerNode{
		Serial: serial, Prefix: prefix, Name: cert.Name, Operator: cert.Operator, Official: cert.Official,
		TCP: cert.TCP, QUIC: cert.QUIC,
		TLSFPs: cert.TLSFPs, TLSKeys: sortedUnion(tlsKeys, nil), Root: rootFP,
		IssuedAt: now.Unix(), NotAfter: cert.NotAfter, File: filepath.Base(path),
	})
	if err := a.commitLedger(l, path); err != nil {
		return err
	}
	a.printf("\n已签发: %s\n", path)
	a.printf("下一步：在管理页上传这个文件，初始设为停用或低权重，观察没问题再启用。\n")
	if *renew != "" && len(fps) > 1 {
		a.printf("换证书顺序：先上传这张新证书、等节点列表更新；再让节点切换到新 TLS 证书；切换完成后用同一份申请再运行一次 sign-node --renew %s，签一张只含新指纹的证书。\n", prefix)
	}
	return nil
}

// ---- revoke ----

func (a *app) cmdRevoke(args []string) error {
	fset := a.flags("revoke", "revoke [--node-serial N[,N...]] [--root primary|backup] [--signer primary|backup] | revoke --resign [--signer primary|backup]")
	nodes := fset.String("node-serial", "", "要吊销的节点证书序号，逗号分隔")
	rootToRevoke := fset.String("root", "", "要吊销的根密钥")
	signerFlag := fset.String("signer", "", "用哪把根签（默认主根；吊销主根时默认备用根）")
	resign := fset.Bool("resign", false, "不新增任何吊销，把台账里现有的全部吊销换一把根重签（例如发版换掉备用根之后改由主根签）")
	if help, err := parseFlags(fset, args); help || err != nil {
		return err
	}
	if fset.NArg() != 0 {
		return errors.New("revoke 不接受位置参数")
	}
	var serials []uint64
	if *nodes != "" {
		for _, s := range strings.Split(*nodes, ",") {
			n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
			if err != nil || n == 0 {
				return fmt.Errorf("节点证书序号 %q 不对", s)
			}
			serials = append(serials, n)
		}
	}
	switch {
	case *resign && (len(serials) > 0 || *rootToRevoke != ""):
		return errors.New("--resign 不新增任何吊销，不能和 --node-serial、--root 一起用")
	case !*resign && len(serials) == 0 && *rootToRevoke == "":
		fset.Usage()
		return errors.New("要指定 --node-serial、--root 或 --resign")
	}
	signer := *signerFlag
	if signer == "" {
		signer = directory.SignerPrimary
		if *rootToRevoke == directory.SignerPrimary {
			signer = directory.SignerBackup
		}
	}
	if err := validRootName(signer); err != nil {
		return err
	}
	if *rootToRevoke != "" {
		if err := validRootName(*rootToRevoke); err != nil {
			return err
		}
		// 与客户端规则一致（directory.checkRootRevocations）：只有备用根能吊销主根。
		// 否则拿到主根的攻击者就能抢先吊销备用根，堵死恢复的路。
		if *rootToRevoke == directory.SignerBackup {
			const refused = "备用根不能用吊销列表吊销（客户端只认备用根吊销主根）；"
			if a.printBackupLeakTodo() {
				return errors.New(refused + "台账里主根也已吊销，没有能用的根了：只能发版，两把根都换新，见设计文档 3.3「备用根密钥泄露」")
			}
			return errors.New(refused + "备用根泄露时只能发版：" +
				"先用主根 status-key new 授权一把新状态密钥，再在 internal/directory/roots.go 的 RevokedRoots 里内置吊销、" +
				"换上新的备用根公钥，并把 MinStatusSerial 抬到那把新状态密钥的序号。" +
				"relay-admin 目前没有换根的命令，新备用根的生成办法见设计文档 3.3「备用根密钥泄露」")
		}
		if *rootToRevoke == signer {
			return fmt.Errorf("不能用%s吊销它自己", rootLabel(signer))
		}
	}
	l, err := a.loadLedger()
	if err != nil {
		return err
	}
	if *resign {
		// 根吊销只有备用根能签（directory.checkRevocations），主根重签含根吊销的累积列表，客户端会整份拒收。
		switch {
		case len(l.RevokedNodes) == 0 && len(l.RevokedRoots) == 0:
			return errors.New("台账里还没有任何吊销，没有可重签的")
		case len(l.RevokedRoots) > 0 && signer != directory.SignerBackup:
			return errors.New("累积的吊销里含根吊销，只有备用根能签这样的列表；请用 --signer backup")
		}
		a.printf("这次不新增任何吊销，只是把现有的全部吊销换%s重签。\n", rootLabel(signer))
	}
	if _, err := a.checkRootUsable(l, signer, "签发"); err != nil {
		return err
	}
	// 吊销按根公钥指纹记：发版换上的新公钥即使也叫 primary，也不受这条吊销影响。
	var revokeFP []string
	if *rootToRevoke != "" {
		fp, err := a.rootFP(*rootToRevoke)
		if err != nil {
			return err
		}
		switch {
		case !l.rootRevoked(fp):
			revokeFP = []string{fp}
		case len(serials) == 0:
			return fmt.Errorf("%s已经吊销过了，不用再吊销", rootLabel(*rootToRevoke))
		default:
			// 再吊销一次会把状态密钥序号下限抬到此刻，作废吊销之后新授权的现用状态密钥。
			a.printf("注意：%s已经吊销过了，这次只吊销节点证书。\n", rootLabel(*rootToRevoke))
		}
	}
	// 点名吊销一张证书，就是宣告它的私钥泄露。仍信任这些私钥的其他有效证书（换证书期间签的、
	// 同一私钥另签的）必须一并吊销：否则状态密钥或 CF 账号被盗时，把那张证书重新挂上，
	// 配上泄露的私钥照样能冒充节点。被连带的证书里其余的私钥不算泄露。
	var leaked []string
	for _, s := range serials {
		i := slices.IndexFunc(l.Nodes, func(n ledgerNode) bool { return n.Serial == s })
		if i < 0 {
			a.printf("注意：台账里没有序号 %d 的证书，仍会照样吊销。\n", s)
			continue
		}
		leaked = append(leaked, l.Nodes[i].TLSKeys...)
	}
	for _, n := range l.certsTrustingKeys(leaked, serials) {
		a.printf("注意：证书 %d（前缀 %s，%q）也信任同一把私钥，一并吊销。\n", n.Serial, n.Prefix, n.Name)
		serials = append(serials, n.Serial)
	}

	now := a.now()
	minStatus := l.MinStatusSerial
	if len(revokeFP) > 0 {
		// 被吊销的根留下的状态密钥位置会作废；同时声明此刻以前授权的状态密钥一律作废，
		// 免得早已被取代的旧状态密钥（哪怕由本根授权）借机重新生效。序号与 status-key new 一样取当前时间。
		minStatus = max(minStatus, timeSerial(l.LastStatusSerial, now))
	}
	rev := directory.Revocations{
		Serial:          timeSerial(l.LastRevocationSerial, now),
		IssuedAt:        now.Unix(),
		NodeSerials:     sortedUnion(l.RevokedNodes, serials),
		Roots:           sortedUnion(l.RevokedRoots, revokeFP),
		MinStatusSerial: minStatus,
	}
	if len(rev.NodeSerials) > directory.MaxRevokedNodes || len(rev.Roots) > directory.MaxRevokedRoots {
		return fmt.Errorf("吊销条目超过客户端上限（节点证书最多 %d 条、根最多 %d 把），客户端会整份拒收",
			directory.MaxRevokedNodes, directory.MaxRevokedRoots)
	}
	roots, _, _ := a.roots(false)
	a.printf("将签发吊销列表（序号 %d，%s签名），包含以往全部吊销：\n", rev.Serial, rootLabel(signer))
	a.printf("  节点证书序号  %v\n", rev.NodeSerials)
	a.printf("  根密钥        %q\n", describeRoots(roots, rev.Roots))
	if rev.MinStatusSerial > 0 {
		a.printf("  状态密钥序号下限  %d：此前授权的状态密钥全部作废\n", rev.MinStatusSerial)
	}
	// 被吊销的根签过的节点证书，客户端随之不再接受：上传吊销之前要先用另一把根重签，否则这些节点全部消失。
	var renew, reonboard []ledgerNode
	if len(revokeFP) > 0 {
		dead := a.deadRoots(revokeFP[0])
		renew, reonboard = l.strandedBy(dead, rev.NodeSerials)
		a.printStranded(renew, reonboard, revokeFP[0], dead, rootLabel(*rootToRevoke))
	}
	if err := a.confirm("确认吊销？吊销不可撤回。"); err != nil {
		return err
	}
	rootPriv, err := a.loadRoot(signer)
	if err != nil {
		return err
	}
	env, err := directory.IssueRevocations(rootPriv, signer, rev)
	if err != nil {
		return err
	}
	data, err := indentJSON(env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.issuedDir(), 0o700); err != nil {
		return err
	}
	path := filepath.Join(a.issuedDir(), fmt.Sprintf("revocations-%d.json", rev.Serial))
	if err := writeNewFile(path, data); err != nil {
		return err
	}
	l.LastRevocationSerial = rev.Serial
	l.RevokedNodes, l.RevokedRoots, l.MinStatusSerial = rev.NodeSerials, rev.Roots, rev.MinStatusSerial
	l.LeakedKeys = sortedUnion(l.LeakedKeys, leaked)
	if err := a.commitLedger(l, path); err != nil {
		return err
	}
	a.printf("\n已签发: %s\n", path)
	if len(revokeFP) > 0 {
		a.printf("下一步（全部备好再一起上传）：\n")
		a.printf("  1. 运行 status-key new --root %s 授权新的状态密钥（现用的状态密钥会随这份吊销作废）。\n", signer)
		if len(renew)+len(reonboard) > 0 {
			a.printf("  2. 处理上面列出的节点证书：\n")
			a.printResignSteps(renew, reonboard, signer)
			a.printf("  3. 在管理页把新状态密钥、重签的节点证书和最后生成的那份吊销列表（每次 revoke 都带着以往全部吊销，只传最后一份）一起上传。\n")
		} else {
			a.printf("  2. 在管理页把新状态密钥和这个文件一起上传。\n")
		}
		a.printf("只传吊销、不换状态密钥，客户端会拒收之后的全部列表；没重签就上传，那些节点在客户端上会全部消失。\n")
	} else {
		a.printf("下一步：在管理页上传这个文件。客户端见过它之后会永久记住这些吊销。\n")
	}
	a.printf("另外请在下一个客户端版本里把这些条目写进 internal/directory/roots.go 的 RevokedNodes/RevokedRoots，\n")
	a.printf("这样从没见过这份吊销的新装客户端也会拒绝它们。\n")
	if len(rev.Roots) > 0 {
		// 只认内置吊销的客户端没有这份吊销列表里的下限：被吊销的根名下的位置作废后，下限只剩内置值。
		a.printf("写 RevokedRoots 时，必须同时把 MinStatusSerial 抬到不低于 %d（本次吊销声明的下限）；\n", rev.MinStatusSerial)
		a.printf("不抬的话，新装的客户端会重新接受此前已被取代的旧状态密钥。\n")
		a.printf("被吊销的根公钥也要从 Keys 里删掉（或换上新公钥），TestBuiltinRoots 会检查。\n")
	}
	return nil
}

// deadRoots 返回「这把根签的证书在 fp 被吊销（或发版换掉）后客户端还认不认」的判定：fp 本身失效；
// 管理目录里已经没有的根也算失效——那是手工换掉的旧根（设计文档 3.3），发版会内置吊销它。
// 台账里的证书记的是签发时的根指纹，不这样判定，换根之前签的证书会被认不出、静默漏掉。
func (a *app) deadRoots(fp string) func(string) bool {
	known := map[string]bool{}
	for _, name := range []string{directory.SignerPrimary, directory.SignerBackup} {
		if k, err := a.rootFP(name); err == nil {
			known[k] = true
		}
	}
	return func(r string) bool { return r == fp || !known[r] }
}

// printStranded 列出被吊销（或发版换掉）的根签发、仍然有效，但根被吊销后客户端不再接受的节点证书。
// 由已换掉的旧根签的单独注明；要重新入网的前缀列的是它剩下的全部有效证书，其中仍有效的根签的客户端仍接受，
// 只是重新入网前也要吊销，也单独注明。
func (a *app) printStranded(renew, reonboard []ledgerNode, fromFP string, dead func(string) bool, from string) {
	if len(renew)+len(reonboard) == 0 {
		return
	}
	const replaced = "由已换掉、不在管理目录里的旧根签发，发版后客户端不再接受"
	a.printf("注意：以下仍有效的节点证书上传前要先处理（除注明的以外都由%s签发，生效后客户端不再接受）：\n", from)
	for _, n := range renew {
		note := ""
		if n.Root != fromFP {
			note = "（" + replaced + "）"
		}
		a.printf("  前缀 %s  序号 %d  %q%s\n", n.Prefix, n.Serial, n.Name, note)
	}
	for _, n := range reonboard {
		note := "该前缀最新的证书已被吊销，不能 --renew"
		switch {
		case n.Root == fromFP:
		case dead(n.Root):
			note += "；" + replaced
		default:
			note += "；这张不是" + from + "签的，客户端仍接受，但重新入网前要一并吊销"
		}
		a.printf("  前缀 %s  序号 %d  %q（%s）\n", n.Prefix, n.Serial, n.Name, note)
	}
}

// printResignSteps 打印 printStranded 列出的证书各自该怎么处理；to 是用来重签的根（primary|backup）。
func (a *app) printResignSteps(renew, reonboard []ledgerNode, to string) {
	if len(renew) > 0 {
		a.printf("     用%s重签，每个前缀一条（节点证书没换过的话，可以用它当初提交的申请文件；官方节点记得加 --official）：\n", rootLabel(to))
		for _, n := range renew {
			a.printf("       relay-admin sign-node --renew %s --root %s <申请文件>\n", n.Prefix, to)
		}
	}
	var prefixes []string
	for _, n := range reonboard {
		if !slices.Contains(prefixes, n.Prefix) {
			prefixes = append(prefixes, n.Prefix)
		}
	}
	for _, p := range prefixes {
		var serials []uint64
		for _, n := range reonboard {
			if n.Prefix == p {
				serials = append(serials, n.Serial)
			}
		}
		strs := make([]string, len(serials))
		for i, s := range serials {
			strs[i] = strconv.FormatUint(s, 10)
		}
		a.printf("     前缀 %s 最新的证书已被吊销，旧私钥都可能已泄露，不能续签：节点用新生成的私钥重新申请，先吊销剩下的证书，再重新入网：\n", p)
		a.printf("       relay-admin revoke --signer %s --node-serial %s\n", to, strings.Join(strs, ","))
		a.printf("       relay-admin sign-node --prefix %s --root %s <新私钥的申请文件>\n", p, to)
	}
}

// printBackupLeakTodo 在拒绝用吊销列表吊销备用根时，列出备用根签发、发版换掉备用根之后客户端不再接受的节点证书，
// 以及要用主根做的事。返回台账里主根是否也已吊销（这时没有能用的根，提示和拒绝的理由都不同）。
// 读不到台账或根公钥时打印警告，不影响后面的拒绝。
func (a *app) printBackupLeakTodo() (primaryRevoked bool) {
	const cannotList = "注意：%v；没法列出备用根签发、发版后要改由主根处理的节点证书，补齐后再运行一次。\n"
	l, err := a.loadLedger()
	if err != nil {
		a.printf(cannotList, err)
		return false
	}
	// 先看主根：这一步不需要备用根的文件（它平时离线保管）。
	// 根吊销只能由备用根签、且只能吊销主根，所以台账里有根吊销就是主根吊销过：主根签的东西客户端不认，
	// 重签的命令也会被 checkRootUsable 拒绝，不能再往那条路上引。台账只记管理员自己签的吊销，
	// 所以这是因主根泄露而正当吊销的，不是攻击者拿备用根干的。
	pfp, err := a.rootFP(directory.SignerPrimary)
	if err != nil {
		a.printf(cannotList, err)
		return false
	}
	if l.rootRevoked(pfp) {
		a.printf("主根密钥也已吊销，没有能用的根了：两把根都要换新，见设计文档 3.3「备用根密钥泄露」。\n")
		return true
	}
	fp, err := a.rootFP(directory.SignerBackup)
	if err != nil {
		a.printf(cannotList, err)
		return false
	}
	dead := a.deadRoots(fp)
	renew, reonboard := l.strandedBy(dead, l.RevokedNodes)
	a.printStranded(renew, reonboard, fp, dead, rootLabel(directory.SignerBackup))
	a.printResignSteps(renew, reonboard, directory.SignerPrimary)
	if len(l.RevokedNodes)+len(l.RevokedRoots) > 0 {
		// 台账不记每份吊销列表是哪把根签的；最后一份若由主根签，重签也无妨。列表里内嵌的吊销验签不过，
		// 客户端拒收的是整份节点列表，不只是这份吊销。
		a.printf("最后那份吊销列表若是用 --signer backup 签的，发版前必须用 relay-admin revoke --resign 由主根重签一份并上传替换，\n")
		a.printf("否则新版本客户端会整份拒收节点列表，只能退回内置默认节点。\n")
	}
	return false
}

func serialsOf(nodes []ledgerNode) []uint64 {
	out := make([]uint64, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Serial)
	}
	return out
}

// sameSet 报告两组地址是否相同（不计顺序）。
func sameSet(a, b []string) bool {
	return slices.Equal(sortedUnion(a, nil), sortedUnion(b, nil))
}

func sortedUnion[T string | uint64](a, b []T) []T {
	out := append(slices.Clone(a), b...)
	slices.Sort(out)
	return slices.Compact(out)
}
