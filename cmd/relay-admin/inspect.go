package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

// cmdInspect 逐层验签并打印任意签名对象：节点列表、节点证书、状态密钥授权、吊销列表、入网申请。
// 打印的字符串一律用 %q，即使内容来自第三方申请也不会把控制字符打到终端上。
func (a *app) cmdInspect(args []string) error {
	fset := a.flags("inspect", "inspect [--builtin] <文件或 URL>")
	builtin := fset.Bool("builtin", false, "用程序内置的根公钥验签，而不是管理目录里的 .pub")
	if help, err := parseFlags(fset, args); help || err != nil {
		return err
	}
	if fset.NArg() != 1 {
		fset.Usage()
		return errors.New("需要一个文件或 URL")
	}
	raw, err := readSource(fset.Arg(0))
	if err != nil {
		return err
	}
	var probe struct {
		Kind    directory.Kind  `json:"kind"`
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("不是 JSON: %w", err)
	}
	if probe.Request != nil {
		return a.inspectRequest(raw)
	}
	roots, desc, err := a.roots(*builtin)
	if err != nil {
		return err
	}
	if !*builtin {
		if roots, desc, err = a.withLedgerRevocations(roots, desc); err != nil {
			return err
		}
	}
	a.printf("验签用的根：%s\n\n", desc)
	if probe.Kind == directory.KindDirectory {
		return a.inspectDirectory(raw, roots)
	}
	var env directory.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	switch env.Kind {
	case directory.KindNodeCert, directory.KindStatusKey, directory.KindRevocations:
	default:
		return fmt.Errorf("认不出这是什么文件（kind %q）", env.Kind)
	}
	if err := directory.CheckRootSignature(env, env.Kind, roots); err != nil {
		return fmt.Errorf("%s 签名无效: %w", env.Kind, err)
	}
	a.printf("签名有效（%s 签发）。\n", env.Signer)
	if pub, ok := roots.Keys[env.Signer]; ok && slices.Contains(roots.RevokedRoots, directory.RootFingerprint(pub)) {
		a.printf("注意：签发它的%s已吊销，见过这条吊销（或发版内置了它）的客户端不会接受它。\n", rootLabel(env.Signer))
	}
	switch env.Kind {
	case directory.KindNodeCert:
		var c directory.NodeCert
		if err := json.Unmarshal(env.Payload, &c); err != nil {
			return err
		}
		a.printNodeCert(c)
		if slices.Contains(roots.RevokedNodes, c.Serial) {
			a.printf("注意：这张证书已吊销，见过这条吊销（或发版内置了它）的客户端不会接受它。\n")
		}
		if err := directory.ValidateNodeCert(&c); err != nil {
			return fmt.Errorf("证书内容不合规，客户端会丢弃: %w", err)
		}
	case directory.KindStatusKey:
		var c directory.StatusKeyCert
		if err := json.Unmarshal(env.Payload, &c); err != nil {
			return err
		}
		a.printf("状态密钥授权\n  序号    %d\n  公钥    %s\n  有效期  %s 至 %s\n",
			c.Serial, base64.StdEncoding.EncodeToString(c.PubKey), formatTime(c.NotBefore), formatTime(c.NotAfter))
		if c.Serial < roots.MinStatusSerial {
			a.printf("注意：序号低于状态密钥序号下限 %d，见过这个下限（或发版内置了它）的客户端会拒收它授权的列表。\n", roots.MinStatusSerial)
		}
	case directory.KindRevocations:
		var r directory.Revocations
		if err := json.Unmarshal(env.Payload, &r); err != nil {
			return err
		}
		a.printf("吊销列表\n  序号          %d\n  签发时间      %s\n  节点证书序号  %v\n  根密钥        %q\n",
			r.Serial, formatTime(r.IssuedAt), r.NodeSerials, describeRoots(roots, r.Roots))
		if r.MinStatusSerial > 0 {
			a.printf("  状态密钥序号下限  %d（更低的状态密钥授权一律作废）\n", r.MinStatusSerial)
		}
	}
	return nil
}

// withLedgerRevocations 把台账里的吊销（节点证书、根、状态密钥序号下限）并进 roots：见过那些吊销（或发版内置了它们）
// 的客户端就是按这些验证的。管理目录里的 .pub 只有公钥，不带这些，不并进来的话，已吊销的东西仍显示有效，
// 管理员应急时拿 inspect 核对要上传的东西会被误导。没有台账（比如只拷了 .pub）或台账里没有吊销时原样返回；
// 台账在但读不出来时报错，不能当成没有吊销。
func (a *app) withLedgerRevocations(roots directory.Roots, desc string) (directory.Roots, string, error) {
	if _, err := os.Stat(a.ledgerPath()); errors.Is(err, fs.ErrNotExist) {
		return roots, desc, nil
	}
	l, err := a.loadLedger()
	if err != nil {
		hint := "读不出台账就没法按里面的吊销核对"
		if len(directory.BuiltinRoots().Keys) > 0 {
			hint += "；要只按程序内置的根验证，用 --builtin"
		}
		return roots, desc, fmt.Errorf("%w（%s）", err, hint)
	}
	if len(l.RevokedNodes)+len(l.RevokedRoots) == 0 && l.MinStatusSerial == 0 {
		return roots, desc, nil
	}
	roots.RevokedNodes = sortedUnion(roots.RevokedNodes, l.RevokedNodes)
	roots.RevokedRoots = sortedUnion(roots.RevokedRoots, l.RevokedRoots)
	roots.MinStatusSerial = max(roots.MinStatusSerial, l.MinStatusSerial)
	return roots, desc + "（并按台账里的吊销验证）", nil
}

// describeRoots 把根公钥指纹标上已知的名字，便于核对吊销的是哪一把。
func describeRoots(roots directory.Roots, fps []string) []string {
	out := make([]string, 0, len(fps))
	for _, fp := range fps {
		label := fp
		for name, pub := range roots.Keys {
			if directory.RootFingerprint(pub) == fp {
				label = name + " " + fp
			}
		}
		out = append(out, label)
	}
	return out
}

func readSource(src string) ([]byte, error) {
	if !strings.HasPrefix(src, "https://") && !strings.HasPrefix(src, "http://") {
		return readFileLimited(src, directory.MaxRawSize)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(src)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 状态行是服务端给的任意文本，转义后再进错误信息。
		return nil, fmt.Errorf("%s 返回 %q", src, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, directory.MaxRawSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > directory.MaxRawSize {
		return nil, fmt.Errorf("%s 返回的内容超过 %d 字节", src, directory.MaxRawSize)
	}
	return data, nil
}

func (a *app) printNodeCert(c directory.NodeCert) {
	a.printf("  前缀 %q  名称 %q  运营者 %q  地区 %q  官方 %v\n", c.Prefix, c.Name, c.Operator, c.Region, c.Official)
	a.printf("    证书序号 %d  有效期 %s 至 %s\n", c.Serial, formatTime(c.NotBefore), formatTime(c.NotAfter))
	a.printf("    TCP %q  QUIC %q\n", c.TCP, c.QUIC)
	a.printf("    指纹 %q\n", c.TLSFPs)
}

func (a *app) inspectDirectory(raw []byte, roots directory.Roots) error {
	v, err := directory.Verify(raw, roots, directory.State{}, a.now())
	if err != nil {
		return fmt.Errorf("节点列表验签失败: %w", err)
	}
	a.printf("节点列表（列表签名有效，各节点是否可用见下）\n")
	a.printf("  序号          %d\n", v.Serial)
	a.printf("  签发 / 过期   %s / %s\n", formatTime(v.IssuedAt.Unix()), formatTime(v.ExpiresAt.Unix()))
	a.printf("  策略          %s\n", v.Policy)
	a.printf("  默认节点      %s\n", v.DefaultPrefix)
	a.printf("  状态密钥授权  序号 %d\n", v.StatusKeySerial)
	if len(v.RevokedNodes)+len(v.RevokedRoots) > 0 {
		a.printf("  吊销          节点证书 %v  根 %q\n", v.RevokedNodes, describeRoots(roots, v.RevokedRoots))
	}
	a.printf("\n通过校验的节点 %d 个（是否启用看各自的状态和权重）：\n", len(v.Nodes))
	for _, n := range v.Nodes {
		a.printf("  [%s 权重 %d]\n", n.Status, n.Weight)
		a.printNodeCert(n.NodeCert)
	}
	if len(v.Skipped) > 0 {
		a.printf("\n被丢弃的节点 %d 个（客户端不会使用）：\n", len(v.Skipped))
		for _, s := range v.Skipped {
			a.printf("  前缀 %q  证书序号 %d  原因: %v\n", s.Prefix, s.Serial, s.Err)
		}
	}
	return nil
}

func (a *app) inspectRequest(raw []byte) error {
	req, err := directory.VerifyNodeRequest(raw)
	if err != nil {
		return fmt.Errorf("入网申请无效: %w", err)
	}
	if req.PrevFingerprint != "" {
		a.printf("换证书申请（签名有效：申请人同时掌握新旧两张证书的私钥）\n")
	} else {
		a.printf("入网申请（签名有效：申请人掌握该证书私钥）\n")
	}
	a.printf("  名称      %q\n  运营者    %q\n  地区      %q\n", req.Name, req.Operator, req.Region)
	a.printf("  TCP       %q\n  QUIC      %q\n", req.TCP, req.QUIC)
	a.printf("  证书指纹  %s\n", req.Fingerprint)
	if req.PrevFingerprint != "" {
		a.printf("  旧证书    %s\n", req.PrevFingerprint)
	}
	a.printf("  生成时间  %s\n", formatTime(req.CreatedAt))
	a.printf("地址是否真的出示这张证书，sign-node 会实际连接核对。\n")
	return nil
}
