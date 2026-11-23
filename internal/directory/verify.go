package directory

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// State 是验证端持久保存的状态，用来防回放与回滚。
//
// (MaxStatusSerial, MaxSerial) 是已接受的最新列表的位置，按字典序比较：状态密钥授权序号更高的列表总是更新，
// 授权序号相同时才比列表序号。这样状态密钥泄露后，攻击者把列表序号推到极值也卡不住客户端——管理员授权一把
// 序号更高的新状态密钥，列表序号就从头算。StatusRoot 是授权那把状态密钥的根公钥指纹；这把根被吊销后，
// 它留下的位置作废：拿到泄露主根的攻击者能把两个序号都推到极值，备用根吊销主根之后客户端必须还能恢复。
// 位置作废后，状态密钥序号的下限改由吊销列表声明（Revocations.MinStatusSerial），此前被取代的旧状态密钥不会复活。
//
// 吊销按签发它的根公钥分别记（每把根一条 RevocationRecord），每条只增不减、取并集。吊销列表自己的序号不参与判断：
// 旧吊销列表撤销不了已记住的吊销，而拒收「序号更低」的吊销列表反倒会让攻击者用极大序号挡住之后备用根签的吊销。
// 分开记，是为了让泄露的根塞进来的东西能随它一起作废（规则见 effectiveRevocations）。
type State struct {
	StatusRoot      string             `json:"status_root,omitempty"`
	MaxStatusSerial uint64             `json:"max_status_serial"`
	MaxSerial       uint64             `json:"max_serial"`
	Revocations     []RevocationRecord `json:"revocations,omitempty"`
}

// RevocationRecord 是一把根签过的全部吊销的并集。每条的条目数受 MaxRevokedNodes/MaxRevokedRoots 限制；
// 只有验得过签名的根才会产生记录，条数不超过这台机器历来信任过的根公钥数。所以 State 的大小有上限。
type RevocationRecord struct {
	Root            string   `json:"root"` // 签发者的根公钥指纹
	NodeSerials     []uint64 `json:"node_serials,omitempty"`
	Roots           []string `json:"roots,omitempty"`
	MinStatusSerial uint64   `json:"min_status_serial,omitempty"`
}

// revocationSet 是某一时刻生效的吊销。
type revocationSet struct {
	builtin   map[string]bool // 内置吊销的根
	roots     map[string]bool // 生效的根吊销（含内置）
	nodes     map[uint64]bool
	minStatus uint64
}

// effectiveRevocations 汇总内置吊销与各条记录：
//   - 内置吊销无条件生效。
//   - 记录里的根吊销：签发它的根没被内置吊销就生效。只有发版能作废一把根签过的根吊销——
//     备用根泄露时，攻击者用它吊销的主根在发版（内置吊销那把备用根）后照常可用；
//     而备用根吊销了主根之后，主根反过来作废不了这条吊销。
//   - 记录里的节点吊销与状态密钥序号下限：签发它的根没被吊销（内置，或上一步生效的根吊销）才生效。
//     泄露的主根灌进来的节点吊销、推到极值的序号下限，在备用根吊销主根后一并失效。
func effectiveRevocations(roots Roots, recs []RevocationRecord) revocationSet {
	s := revocationSet{builtin: make(map[string]bool), nodes: make(map[uint64]bool), minStatus: roots.MinStatusSerial}
	for _, fp := range roots.RevokedRoots {
		s.builtin[fp] = true
	}
	s.roots = maps.Clone(s.builtin)
	for _, n := range roots.RevokedNodes {
		s.nodes[n] = true
	}
	for _, r := range recs {
		if !s.builtin[r.Root] {
			for _, fp := range r.Roots {
				s.roots[fp] = true
			}
		}
	}
	for _, r := range recs {
		if !s.roots[r.Root] {
			for _, n := range r.NodeSerials {
				s.nodes[n] = true
			}
			s.minStatus = max(s.minStatus, r.MinStatusSerial)
		}
	}
	return s
}

// mergeRecord 把 rec 并进 recs 里同一把根的记录（没有就追加）。返回新切片，不改动 recs。
func mergeRecord(recs []RevocationRecord, rec RevocationRecord) ([]RevocationRecord, error) {
	out := make([]RevocationRecord, 0, len(recs)+1)
	merged := false
	for _, r := range recs {
		if r.Root == rec.Root {
			r = RevocationRecord{
				Root:            r.Root,
				NodeSerials:     union(r.NodeSerials, rec.NodeSerials),
				Roots:           union(r.Roots, rec.Roots),
				MinStatusSerial: max(r.MinStatusSerial, rec.MinStatusSerial),
			}
			if len(r.NodeSerials) > MaxRevokedNodes || len(r.Roots) > MaxRevokedRoots {
				return nil, verr(LayerRevocations, ErrMalformed, "root %s has revoked more than %d node serials or %d roots in total",
					r.Root, MaxRevokedNodes, MaxRevokedRoots)
			}
			merged = true
		}
		out = append(out, r)
	}
	if !merged {
		out = append(out, rec)
	}
	return out, nil
}

// pruneRecords 去掉签发者已被内置吊销的记录：它们什么都不再贡献。
func pruneRecords(recs []RevocationRecord, builtin map[string]bool) []RevocationRecord {
	var out []RevocationRecord
	for _, r := range recs {
		if !builtin[r.Root] {
			out = append(out, r)
		}
	}
	return out
}

// samePosition 报告两个状态是否指向同一份列表位置。
func samePosition(a, b State) bool {
	return a.StatusRoot == b.StatusRoot && a.MaxStatusSerial == b.MaxStatusSerial && a.MaxSerial == b.MaxSerial
}

// Node 是通过全部校验、可以使用的节点。
type Node struct {
	NodeCert
	Status string
	Weight int
}

// Skipped 是被丢弃的节点及原因。整份列表仍然有效，调用方应把它写进日志。
// Prefix/Serial 取自证书正文，签名没验过时也照样填，解析不出来就是零值。它们只用于日志和核对
// default_prefix 有没有列出（这项核对挡不住状态密钥持有者，用未验签的前缀不降低安全性），不能当作可信信息。
type Skipped struct {
	Prefix string
	Serial uint64
	Err    error
}

// Verified 是验证通过的列表。
type Verified struct {
	Serial          uint64
	IssuedAt        time.Time
	ExpiresAt       time.Time
	Policy          string
	DefaultPrefix   string
	StatusKeySerial uint64
	Nodes           []Node
	Skipped         []Skipped
	// RevokedNodes / RevokedRoots 是这次验证时生效的吊销（内置、已持久化、本列表携带的合在一起，作废的不算），
	// 已排序，供日志与展示。
	RevokedNodes []uint64
	RevokedRoots []string
	// NewState 是吸收了这份列表之后应当持久化的状态。
	NewState State
}

// NodeByPrefix 按前缀（不区分大小写）找节点。
func (v *Verified) NodeByPrefix(prefix string) *Node {
	if v == nil || len(prefix) != 1 {
		return nil
	}
	p := strings.ToUpper(prefix)
	for i := range v.Nodes {
		if v.Nodes[i].Prefix == p {
			return &v.Nodes[i]
		}
	}
	return nil
}

// NodeByFingerprint 按 relay TLS 证书指纹找节点。
func (v *Verified) NodeByFingerprint(fp string) *Node {
	if v == nil || fp == "" {
		return nil
	}
	fp = strings.ToLower(fp)
	for i := range v.Nodes {
		if slices.Contains(v.Nodes[i].TLSFPs, fp) {
			return &v.Nodes[i]
		}
	}
	return nil
}

// DefaultNode 返回默认节点（不论状态）；默认节点被丢弃时返回 nil。
func (v *Verified) DefaultNode() *Node {
	if v == nil {
		return nil
	}
	return v.NodeByPrefix(v.DefaultPrefix)
}

// VerifyError.Layer 的取值。
const (
	LayerRoots       = "roots"
	LayerDirectory   = "directory"
	LayerStatusKey   = "status_key"
	LayerRevocations = "revocations"
)

func isRootName(name string) bool { return name == SignerPrimary || name == SignerBackup }

// Usable 报告是否至少配置了一把格式正确的根公钥。不满足时 Verify 一律返回 ErrNoRoots，
// relay 据此在启动时就拒绝开启节点模式，而不是等到第一次拉取才发现。
func (r Roots) Usable() bool {
	for _, name := range []string{SignerPrimary, SignerBackup} {
		if _, ok := r.key(name); ok {
			return true
		}
	}
	return false
}

// window 检查 [notBefore, notAfter] 是否覆盖 now，两端各放宽 ClockSkew。
// 用 now 加减而不是对边界加减，边界取极端值时也不会溢出。
func window(notBefore, notAfter int64, now time.Time) error {
	n, skew := now.Unix(), int64(ClockSkew/time.Second)
	if n+skew < notBefore {
		return ErrNotYetValid
	}
	if n-skew > notAfter {
		return ErrExpired
	}
	return nil
}

// Verify 校验 raw（JSON 编码的 KindDirectory 信封）。st 是此前持久化的状态。
// 全部通过才返回结果；整份列表级别的问题返回 *VerifyError，单个节点的问题只丢弃该节点。
//
// 校验顺序：信封与正文能解析 → 吊销列表（根签名）→ 状态密钥授权（根签名、未吊销、序号不回退、在有效期内）
// → 外层签名（状态密钥）→ 列表头（位置不回退、在有效期内、有效期不超上限）→ 逐个节点。
// 状态密钥公钥写在正文里，所以必须先解析正文再验外层签名；在外层签名验过之前，正文里只有自带根签名的
// 吊销列表与状态密钥授权会被使用。
func Verify(raw []byte, roots Roots, st State, now time.Time) (*Verified, error) {
	if !roots.Usable() {
		return nil, verr(LayerRoots, ErrNoRoots, "")
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var body wireBody
	if err := json.Unmarshal(env.Payload, &body); err != nil {
		return nil, verr(LayerDirectory, ErrMalformed, "body: %v", err)
	}
	if body.Format != FormatVersion {
		return nil, verr(LayerDirectory, ErrMalformed, "unsupported format %d", body.Format)
	}

	// 吊销：内置 + 已持久化的记录 + 本列表携带的（并进签发它的那把根的记录）。
	recs := st.Revocations
	rv := effectiveRevocations(roots, recs)
	if body.Revocations != nil {
		rec, err := verifyRevocations(*body.Revocations, roots, rv.roots)
		if err != nil {
			return nil, err
		}
		if recs, err = mergeRecord(recs, rec); err != nil {
			return nil, err
		}
		rv = effectiveRevocations(roots, recs)
	}

	// 已接受的最新位置；授权它的根已被吊销（含本列表刚带来的吊销）时作废，下限退回吊销列表声明的值。
	floorStatus, floorSerial := st.MaxStatusSerial, st.MaxSerial
	if st.StatusRoot != "" && rv.roots[st.StatusRoot] {
		floorStatus, floorSerial = 0, 0
	}
	sk, skRoot, err := verifyStatusKey(body.StatusKey, roots, rv.roots, max(floorStatus, rv.minStatus), now)
	if err != nil {
		return nil, err
	}
	if env.Kind != KindDirectory || env.Signer != SignerStatus || !verifyEnvelope(env, KindDirectory, sk.PubKey) {
		return nil, verr(LayerDirectory, ErrBadSignature, "not signed by the authorized status key (serial %d)", sk.Serial)
	}
	if sk.Serial > floorStatus {
		floorSerial = 0 // 换了新状态密钥，列表序号从头算
	}

	if err := checkHeader(&body.Body, len(body.Nodes), floorSerial, now); err != nil {
		return nil, err
	}

	v := &Verified{
		Serial:          body.Serial,
		IssuedAt:        time.Unix(body.IssuedAt, 0),
		ExpiresAt:       time.Unix(body.ExpiresAt, 0),
		Policy:          body.Policy,
		DefaultPrefix:   body.DefaultPrefix,
		StatusKeySerial: sk.Serial,
	}
	if v.Policy != PolicyPreferDefault && v.Policy != PolicyFastest {
		// 策略只影响选节点顺序、不影响信任；不认识的新策略按最保守的处理，而不是丢掉整份列表。
		v.Policy = PolicyPreferDefault
	}

	listed := make(map[string]bool)   // 正文里出现过的前缀（含被丢弃的），用于核对默认节点
	unreadable := false               // 有被丢弃的项读不出前缀：它可能就是默认节点
	byPrefix := make(map[string]bool) // 以下三个只统计通过校验的节点
	bySerial := make(map[uint64]bool)
	byFP := make(map[string]string)
	for _, e := range body.Nodes {
		node, skip := verifyNode(e, roots, rv.roots, rv.nodes, now)
		if skip != nil {
			if skip.Prefix != "" {
				listed[skip.Prefix] = true
			} else {
				unreadable = true
			}
			v.Skipped = append(v.Skipped, *skip)
			continue
		}
		listed[node.Prefix] = true
		if byPrefix[node.Prefix] {
			return nil, verr(LayerDirectory, ErrMalformed, "prefix %s listed twice", node.Prefix)
		}
		if bySerial[node.Serial] {
			return nil, verr(LayerDirectory, ErrMalformed, "node cert serial %d listed twice", node.Serial)
		}
		for _, fp := range node.TLSFPs {
			if other, dup := byFP[fp]; dup {
				return nil, verr(LayerDirectory, ErrMalformed, "tls fingerprint shared by nodes %s and %s", other, node.Prefix)
			}
			byFP[fp] = node.Prefix
		}
		byPrefix[node.Prefix], bySerial[node.Serial] = true, true
		v.Nodes = append(v.Nodes, *node)
	}
	// 这项核对只为发现写错的 default_prefix，挡不住状态密钥持有者（他能放一张自称任意前缀的坏证书进来），
	// 所以有项读不出前缀时放过它，不因为一项坏了拒掉整份列表（设计文档第 4 节）。
	if !listed[body.DefaultPrefix] && !unreadable {
		return nil, verr(LayerDirectory, ErrMalformed, "default_prefix %q matches no listed node", body.DefaultPrefix)
	}

	// 走到这里，本列表的位置不低于（未作废的）已见位置，直接成为新位置。
	v.RevokedNodes = sortedKeys(rv.nodes)
	v.RevokedRoots = sortedKeys(rv.roots)
	v.NewState = State{
		StatusRoot:      skRoot,
		MaxStatusSerial: sk.Serial,
		MaxSerial:       body.Serial,
		Revocations:     pruneRecords(recs, rv.builtin),
	}
	return v, nil
}

// parseEnvelope 检查大小并严格解析信封：拒绝未知字段与尾随内容。信封里只有 kind 和 payload 参与签名，
// 其它字段谁都能加；放过它们，转发者（镜像、中间人）就能往客户端缓存里塞东西。
// 重复的键 encoding/json 取最后一个、挡不住，所以缓存只存重新编码后的信封（见 Cache.Offer）。
func parseEnvelope(raw []byte) (Envelope, error) {
	var env Envelope
	if len(raw) == 0 || len(raw) > MaxRawSize {
		return env, verr(LayerDirectory, ErrMalformed, "size %d not in 1..%d", len(raw), MaxRawSize)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return env, verr(LayerDirectory, ErrMalformed, "envelope: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return env, verr(LayerDirectory, ErrMalformed, "envelope: trailing data")
	}
	if len(env.Payload) == 0 {
		return env, verr(LayerDirectory, ErrMalformed, "empty payload")
	}
	return env, nil
}

func sortedKeys[T string | uint64](m map[T]bool) []T {
	if len(m) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}

// wireBody 是 Verify 解码正文用的：节点项先保留原文，由 verifyNode 逐项解码。直接解到 []NodeEntry 的话，
// 一项的字段类型不对（例如 weight 写成小数）整份正文就解析失败，连累其余节点。
type wireBody struct {
	Body
	Nodes []json.RawMessage `json:"nodes"` // 比 Body.Nodes 浅一层，encoding/json 解到这里，Body.Nodes 留空
}

// checkHeader 校验列表头。nodes 是列表项个数；minSerial 是同一把状态密钥签过的最高列表序号（换了新状态密钥时为 0）。
func checkHeader(b *Body, nodes int, minSerial uint64, now time.Time) error {
	if b.Serial == 0 {
		return verr(LayerDirectory, ErrMalformed, "serial must be > 0")
	}
	if b.Serial < minSerial {
		return verr(LayerDirectory, ErrRollback, "serial %d is below the highest seen %d", b.Serial, minSerial)
	}
	// 要求 issued_at > 0，保证下面的差值不会溢出。
	if b.IssuedAt <= 0 || b.ExpiresAt <= b.IssuedAt {
		return verr(LayerDirectory, ErrMalformed, "bad validity %d..%d", b.IssuedAt, b.ExpiresAt)
	}
	if b.ExpiresAt-b.IssuedAt > int64(MaxListLifetime/time.Second) {
		return verr(LayerDirectory, ErrMalformed, "lifetime exceeds %s", MaxListLifetime)
	}
	if err := window(b.IssuedAt, b.ExpiresAt, now); err != nil {
		return verr(LayerDirectory, err, "valid %s..%s, now %s",
			time.Unix(b.IssuedAt, 0).UTC().Format(time.RFC3339), time.Unix(b.ExpiresAt, 0).UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	if !ValidPrefix(b.DefaultPrefix) {
		return verr(LayerDirectory, ErrMalformed, "default_prefix %q is not a single code character", b.DefaultPrefix)
	}
	if nodes == 0 || nodes > MaxNodes {
		return verr(LayerDirectory, ErrMalformed, "node count %d not in 1..%d", nodes, MaxNodes)
	}
	return nil
}

// verifyRevocations 验证列表携带的吊销列表，返回以签发者根公钥指纹为键的记录。
func verifyRevocations(env Envelope, roots Roots, revokedRoots map[string]bool) (RevocationRecord, error) {
	fp, err := verifyRootSigned(env, KindRevocations, roots, revokedRoots)
	if err != nil {
		return RevocationRecord{}, verr(LayerRevocations, err, "signer %q", env.Signer)
	}
	var r Revocations
	if err := json.Unmarshal(env.Payload, &r); err != nil {
		return RevocationRecord{}, verr(LayerRevocations, ErrMalformed, "%v", err)
	}
	if err := checkRevocations(env.Signer, fp, r); err != nil {
		return RevocationRecord{}, verr(LayerRevocations, ErrMalformed, "%v", err)
	}
	return RevocationRecord{
		Root:            fp,
		NodeSerials:     union(r.NodeSerials, nil),
		Roots:           union(r.Roots, nil),
		MinStatusSerial: r.MinStatusSerial,
	}, nil
}

// verifyStatusKey 验证状态密钥授权，另外返回授权它的根公钥指纹。
func verifyStatusKey(env Envelope, roots Roots, revokedRoots map[string]bool, minSerial uint64, now time.Time) (*StatusKeyCert, string, error) {
	fp, err := verifyRootSigned(env, KindStatusKey, roots, revokedRoots)
	if err != nil {
		return nil, "", verr(LayerStatusKey, err, "signer %q", env.Signer)
	}
	var c StatusKeyCert
	if err := json.Unmarshal(env.Payload, &c); err != nil {
		return nil, "", verr(LayerStatusKey, ErrMalformed, "%v", err)
	}
	if len(c.PubKey) != ed25519.PublicKeySize || c.Serial == 0 || c.NotAfter <= c.NotBefore {
		return nil, "", verr(LayerStatusKey, ErrMalformed, "bad pubkey, serial or validity")
	}
	if c.Serial < minSerial {
		return nil, "", verr(LayerStatusKey, ErrRollback, "serial %d is below the highest seen %d", c.Serial, minSerial)
	}
	if err := window(c.NotBefore, c.NotAfter, now); err != nil {
		return nil, "", verr(LayerStatusKey, err, "serial %d", c.Serial)
	}
	return &c, fp, nil
}

// CheckRootSignature 校验 env 是 kind 类型、由 roots 里某把根密钥签发。不考虑吊销与有效期，
// 只给管理工具展示单个对象用；客户端与 relay 一律走 Verify。
func CheckRootSignature(env Envelope, kind Kind, roots Roots) error {
	_, err := verifyRootSigned(env, kind, roots, nil)
	return err
}

// verifyRootSigned 校验信封由一把已配置、未吊销的根密钥签发，返回那把根公钥的指纹。
// 吊销按指纹判断：名字只说明是哪个角色，发版换了公钥之后，旧公钥的吊销与新公钥无关。
func verifyRootSigned(env Envelope, kind Kind, roots Roots, revokedRoots map[string]bool) (string, error) {
	if !isRootName(env.Signer) {
		return "", ErrBadSignature
	}
	pub, ok := roots.key(env.Signer)
	if !ok {
		return "", ErrBadSignature
	}
	fp := RootFingerprint(pub)
	if revokedRoots[fp] {
		return "", ErrRevoked
	}
	if !verifyEnvelope(env, kind, pub) {
		return "", ErrBadSignature
	}
	return fp, nil
}

func verifyNode(raw json.RawMessage, roots Roots, revokedRoots map[string]bool, revokedNodes map[uint64]bool, now time.Time) (*Node, *Skipped) {
	// 字段类型不对时，encoding/json 仍会填好其余字段，再返回第一个类型错误：列表项和证书正文读出了前缀
	// 就照样带上，读不出前缀的由调用方放过 default_prefix 核对。默认节点这一项坏了也只是丢弃它。
	var e NodeEntry
	entryErr := json.Unmarshal(raw, &e)
	var c NodeCert
	certErr := json.Unmarshal(e.Cert.Payload, &c)
	skip := func(err error, format string, args ...any) (*Node, *Skipped) {
		s := &Skipped{Serial: c.Serial, Err: verr("node "+c.Prefix, err, format, args...)}
		if ValidPrefix(c.Prefix) {
			s.Prefix = c.Prefix
		} else {
			s.Err = verr("node", err, format, args...)
		}
		return nil, s
	}
	// 列表项本身的错误优先：项不是对象时证书正文是空的，报「unexpected end」没有意义；
	// 项是 null、{} 之类没带证书的，直接说没有证书。
	if entryErr != nil {
		return skip(ErrMalformed, "%v", entryErr)
	}
	if len(e.Cert.Payload) == 0 {
		return skip(ErrMalformed, "no cert")
	}
	if certErr != nil {
		return skip(ErrMalformed, "cert: %v", certErr)
	}
	if _, err := verifyRootSigned(e.Cert, KindNodeCert, roots, revokedRoots); err != nil {
		return skip(err, "cert signer %q", e.Cert.Signer)
	}
	if err := ValidateNodeCert(&c); err != nil {
		return skip(ErrMalformed, "%v", err)
	}
	if revokedNodes[c.Serial] {
		return skip(ErrRevoked, "cert serial %d", c.Serial)
	}
	if err := window(c.NotBefore, c.NotAfter, now); err != nil {
		return skip(err, "cert valid %s..%s", time.Unix(c.NotBefore, 0).UTC().Format(time.DateOnly), time.Unix(c.NotAfter, 0).UTC().Format(time.DateOnly))
	}
	switch e.Status {
	case StatusActive, StatusDraining, StatusDisabled:
	default:
		return skip(ErrMalformed, "unknown status %q", e.Status)
	}
	if e.Weight < 0 || e.Weight > 100 {
		return skip(ErrMalformed, "weight %d not in 0..100", e.Weight)
	}
	return &Node{NodeCert: c, Status: e.Status, Weight: e.Weight}, nil
}

// union 返回 a ∪ b，排序去重；两者都为空时返回 nil。不修改入参。
func union[T string | uint64](a, b []T) []T {
	if len(a)+len(b) == 0 {
		return nil
	}
	out := make([]T, 0, len(a)+len(b))
	out = append(append(out, a...), b...)
	slices.Sort(out)
	return slices.Compact(out)
}
