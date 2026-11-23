package main

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// ledger 是签发台账：记录发过的节点证书（防止前缀重复）、最近用过的序号（保证递增）和全部吊销（新吊销列表要包含旧条目）。
// 丢了台账不会破坏安全：状态密钥授权与吊销列表的序号取当前时间，不依赖台账也递增；节点证书序号是随机数。
// 但会丢掉「哪些前缀已占用」的记录，所以要和根密钥一起备份。
type ledger struct {
	Version              int          `json:"version"`
	LastStatusSerial     uint64       `json:"last_status_serial"`
	LastRevocationSerial uint64       `json:"last_revocation_serial"`
	Nodes                []ledgerNode `json:"nodes"`
	RevokedNodes         []uint64     `json:"revoked_nodes,omitempty"`
	RevokedRoots         []string     `json:"revoked_roots,omitempty"` // 根公钥指纹
	// MinStatusSerial 是吊销根时声明的状态密钥序号下限，之后的每份吊销列表都带上它。
	MinStatusSerial uint64 `json:"min_status_serial,omitempty"`
	// LeakedKeys 是被宣告泄露的 TLS 公钥哈希（见 ledgerNode.TLSKeys）：用 --node-serial 点名吊销的证书里的公钥。
	// 它们不能再签进任何证书。因为与之共用私钥而被连带吊销的证书，其余的公钥不算泄露。
	LeakedKeys []string `json:"leaked_keys,omitempty"`

	// loaded 是读入时台账文件内容的摘要，nil 表示读入时文件还不存在。写回前核对，发现文件在这期间被别的进程
	// 改过就拒绝覆盖（见 saveLedger）。
	loaded *[sha256.Size]byte
}

type ledgerNode struct {
	Serial   uint64   `json:"serial"`
	Prefix   string   `json:"prefix"`
	Name     string   `json:"name"`
	Operator string   `json:"operator"`
	Official bool     `json:"official"`
	TCP      []string `json:"tcp,omitempty"`
	QUIC     []string `json:"quic,omitempty"`
	TLSFPs   []string `json:"tls_fps"`
	// TLSKeys 是这些证书公钥（SPKI）的 sha256。指纹是整张证书的哈希，同一把私钥另签一张证书指纹就变了；
	// 吊销要认的是私钥，所以另记公钥。
	TLSKeys []string `json:"tls_keys"`
	// Root 是签发这张证书的根公钥指纹。那把根被吊销后，客户端不再接受这张证书，要用另一把根重签。
	Root     string `json:"root"`
	IssuedAt int64  `json:"issued_at"`
	NotAfter int64  `json:"not_after"`
	File     string `json:"file"`
}

const ledgerVersion = 1

func (a *app) ledgerPath() string { return filepath.Join(a.dir, "ledger.json") }

func (a *app) loadLedger() (*ledger, error) {
	data, err := os.ReadFile(a.ledgerPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("找不到台账 %s：请先运行 keygen，或把备份的管理目录恢复到这里", a.ledgerPath())
	}
	if err != nil {
		return nil, err
	}
	var l ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("台账 %s 格式不对: %w", a.ledgerPath(), err)
	}
	if l.Version != ledgerVersion {
		return nil, fmt.Errorf("台账版本 %d 不认识", l.Version)
	}
	sum := sha256.Sum256(data)
	l.loaded = &sum
	return &l, nil
}

// saveLedger 原子写回台账。写之前核对文件还是读入时的样子：命令从读台账到写台账中间要等拨号、确认、输口令，
// 同时开着两个 relay-admin 时，后写的若直接覆盖，先写的那次吊销或签发记录就丢了——吊销丢了，后面的吊销列表
// 会漏掉它，被吊销的证书还能续签。发现被改过就拒绝，重新运行命令即可。核对与改名之间仍有极小的窗口，
// 但管理员本机上交互式手工操作的并发撞上它的可能性可以忽略。
func (a *app) saveLedger(l *ledger) error {
	cur, err := os.ReadFile(a.ledgerPath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if l.loaded != nil {
			return fmt.Errorf("台账 %s 在命令执行期间被删除，请重新运行命令", a.ledgerPath())
		}
	case err != nil:
		return err
	default:
		if sum := sha256.Sum256(cur); l.loaded == nil || sum != *l.loaded {
			return fmt.Errorf("台账 %s 在命令执行期间被另一个进程改过，为免覆盖它的记录已放弃写入，请重新运行命令", a.ledgerPath())
		}
	}
	l.Version = ledgerVersion
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.ledgerPath(), data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	l.loaded = &sum
	return nil
}

// commitLedger 在已经写出签发文件之后写台账；台账写不进去就把刚写出的文件删掉，保证「要么都有，要么都没有」。
// 台账不知道的签发是危险的：吊销没记进台账，下一份吊销列表就会漏掉它；证书没记进台账，前缀可能被重复分配。
// 这些文件此时还没上传给任何人，删掉就等于这次命令没发生，重新运行即可。
func (a *app) commitLedger(l *ledger, issuedFiles ...string) error {
	err := a.saveLedger(l)
	if err == nil {
		return nil
	}
	var stuck []string
	for _, p := range issuedFiles {
		if rmErr := os.Remove(p); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			stuck = append(stuck, p)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("写台账失败，本次未签发，但刚写出的文件没能删掉，请手工删除且不要上传 %v: %w", stuck, err)
	}
	return fmt.Errorf("写台账失败，本次未签发（刚写出的文件已删除），排除原因后重新运行命令: %w", err)
}

// leakedKeyCerts 在该公钥被宣告泄露时，返回含有它的已吊销证书序号（用于提示）；没泄露返回 nil。
// 同一把私钥另签的证书、哪怕指纹不同，也一样不可信。
func (l *ledger) leakedKeyCerts(key string) []uint64 {
	if !slices.Contains(l.LeakedKeys, key) {
		return nil
	}
	out := []uint64{}
	for _, n := range l.Nodes {
		if slices.Contains(l.RevokedNodes, n.Serial) && slices.Contains(n.TLSKeys, key) {
			out = append(out, n.Serial)
		}
	}
	return out
}

// certsTrustingKeys 返回仍有效（没被吊销、也不在 except 里）、且含有 keys 中任一公钥的证书。
func (l *ledger) certsTrustingKeys(keys []string, except []uint64) []ledgerNode {
	var out []ledgerNode
	for _, n := range l.Nodes {
		if slices.Contains(l.RevokedNodes, n.Serial) || slices.Contains(except, n.Serial) {
			continue
		}
		if slices.ContainsFunc(n.TLSKeys, func(k string) bool { return slices.Contains(keys, k) }) {
			out = append(out, n)
		}
	}
	return out
}

// certKeyHash 返回证书公钥规范编码（SPKI）的 sha256，小写 hex。
// 不能直接哈希证书里的公钥字段原文：同一把公钥有多种编码（例如 BIT STRING 的填充位数不为 0 时，
// 解析器会把密钥字节右移还原），改写编码就能让被吊销的私钥换个哈希重新签进证书。所以先解析、再重新编码。
func certKeyHash(der []byte) (string, error) {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	spki, err := x509.MarshalPKIXPublicKey(c.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:]), nil
}

// activeWithPrefix 返回占用该前缀、且没被吊销的证书。
func (l *ledger) activeWithPrefix(prefix string) []ledgerNode {
	var out []ledgerNode
	for _, n := range l.Nodes {
		if n.Prefix == prefix && !slices.Contains(l.RevokedNodes, n.Serial) {
			out = append(out, n)
		}
	}
	return out
}

// latestCert 返回该前缀最后签发的那张证书（Nodes 只追加，顺序就是签发顺序）；它被吊销了就返回 nil，
// 不往回找更早的。
//
// 只有最新这张的私钥能证明「掌握该前缀现有证书」。更早的证书在过期或吊销前仍然有效、仍占着前缀，
// 但它们的私钥可能留在退役机器或旧备份里，不能有续签资格；最新一张被吊销（私钥泄露）时，
// 退回去认更早的一张同样不安全——那等于让泄露事件之前的旧私钥重新生效。
func (l *ledger) latestCert(prefix string) *ledgerNode {
	for i := len(l.Nodes) - 1; i >= 0; i-- {
		n := &l.Nodes[i]
		if n.Prefix != prefix {
			continue
		}
		if slices.Contains(l.RevokedNodes, n.Serial) {
			return nil
		}
		return n
	}
	return nil
}

// strandedBy 返回由失效的根（dead 判定）签发、仍然有效（不在 revoked 里）、而根被吊销（或发版换掉）后客户端
// 不再接受的节点证书，按各前缀该怎么处理分两类，各自按前缀排序：
//   - renew：前缀最新的证书就由失效的根签发，用另一把根 `sign-node --renew` 重签即可；
//   - reonboard：前缀最新的证书已被吊销（私钥可能泄露），但还剩更早的有效证书由失效的根签发。--renew 会被拒绝
//     （只认最新那张），只能吊销该前缀剩下的全部有效证书，再用 --prefix 重新入网。返回这些前缀所有仍有效的证书。
//
// 前缀最新的证书已经由仍有效的根签发时什么都不用做，哪一类都不列：列表里每个前缀只放最新的那张。
func (l *ledger) strandedBy(dead func(rootFP string) bool, revoked []uint64) (renew, reonboard []ledgerNode) {
	view := *l
	view.RevokedNodes = revoked
	var prefixes []string
	for _, n := range l.Nodes {
		if !slices.Contains(prefixes, n.Prefix) {
			prefixes = append(prefixes, n.Prefix)
		}
	}
	slices.Sort(prefixes)
	for _, p := range prefixes {
		if latest := view.latestCert(p); latest != nil {
			if dead(latest.Root) {
				renew = append(renew, *latest)
			}
			continue
		}
		active := view.activeWithPrefix(p)
		if slices.ContainsFunc(active, func(n ledgerNode) bool { return dead(n.Root) }) {
			reonboard = append(reonboard, active...)
		}
	}
	return renew, reonboard
}

// rootRevoked 按根公钥指纹查台账里的根吊销。
func (l *ledger) rootRevoked(fp string) bool { return slices.Contains(l.RevokedRoots, fp) }

// timeSerial 取当前 Unix 秒作为序号，并保证比上一次大。用时间而不是计数器，是为了在
// 另一台机器上用备用根签发时（那里可能没有最新台账）序号也照样递增。
func timeSerial(last uint64, now time.Time) uint64 {
	s := uint64(now.Unix())
	if s <= last {
		s = last + 1
	}
	return s
}
