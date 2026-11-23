package directory

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
)

// Roots 是验证端信任的根。生产环境用 BuiltinRoots（编进二进制），测试注入自己的。
type Roots struct {
	// Keys: 签名者名字（SignerPrimary / SignerBackup）→ 根公钥。名字只决定角色（谁能吊销谁）；
	// 吊销、持久化的位置都按公钥指纹（RootFingerprint）记，发版换上新公钥后与旧公钥互不相干。
	Keys map[string]ed25519.PublicKey
	// MinStatusSerial 内置的状态密钥授权序号下限。每次发版可以抬高，
	// 让「首次运行、没有本地缓存」的客户端也拒绝已知泄露的旧状态密钥。
	MinStatusSerial uint64
	// RevokedNodes 内置吊销的节点证书序号。发版时把已确认被吊销的条目写进来，
	// 这样即使 CF 被盗后回放一份旧列表，新装的客户端也不会信任那些节点。
	RevokedNodes []uint64
	// RevokedRoots 内置吊销的根公钥指纹（RootFingerprint 的小写 hex 原样照抄，写错格式会静默失效，
	// TestBuiltinRoots 会查）。泄露过的根都要写进来（哪怕当时已被另一把根吊销）：
	// 它签过的吊销、它名下的位置随之作废，攻击者用它写进客户端状态的东西就卡不住恢复。
	//
	// 往这里加条目时必须同时抬高 MinStatusSerial（spec 3.3）：位置作废后序号下限只剩内置值和吊销列表声明的值，
	// 没收到那份吊销列表的客户端只有内置值，不抬就会重新接受此前被取代的旧状态密钥。
	// 主根被吊销：至少抬到那份吊销列表声明的下限（relay-admin revoke --root 会打印）；
	// 备用根泄露：抬到发版前用主根新授权的状态密钥的序号。
	// 同时把那把公钥从 Keys 里删掉或换成新公钥（TestBuiltinRoots 会查）。客户端判断根吊销只看签发者是不是
	// 备用根、吊销的是不是它自己，不依赖 Keys 里还留着被吊销的公钥。
	RevokedRoots []string
}

// 内置根公钥。由 `relay-admin keygen` 生成后填入；为空时目录功能整体关闭（Verify 返回 ErrNoRoots），
// 客户端退回内置默认节点，行为与没有这个功能时一致。公钥本身是公开信息，可以进仓库。
var builtinRoots = Roots{}

// BuiltinRoots 返回编进二进制的根。
func BuiltinRoots() Roots { return builtinRoots }

// RootFingerprint 返回根公钥的指纹：公钥 sha256 的小写 hex。吊销根、记录位置都用它。
func RootFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

func (r Roots) key(name string) (ed25519.PublicKey, bool) {
	k, ok := r.Keys[name]
	if !ok || len(k) != ed25519.PublicKeySize {
		return nil, false
	}
	return k, true
}
