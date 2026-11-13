package relay

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 复用令牌（持有证明）：relay 签发给 share、只有该 share 知道的无状态凭据，
// 用来在「码已无会话」（relay 重启 / share 离线超宽限被回收）后证明请求者是码的原持有者，
// 防止他人凭泄露的协助码带 request_code 抢先注册、占住该码。
//
// 令牌 = "v1." + base64url( createdAtUnixNano(8B) || HMAC-SHA256(secret, 域前缀 || 码 || ClientID || createdAt) )。
// 绑定了码、ClientID 与原始签发时间：换码、换 ClientID、改时间任一都验不过；
// 复用时由令牌还原原始 CreatedAt，使 --code-max-age 计时连续，不会因反复复用而重置。

const (
	reuseSecretFile = "reuse-secret"
	reuseSecretLen  = 32
	reuseTokenPref  = "v1."
	reuseMACDomain  = "ra-reuse-v1"
	// 令牌 base64 后长度固定 (8+32)*4/3 向上取整 = 54 字符；超长直接拒绝，避免对任意长串做解码。
	reuseTokenMaxLen = 128
)

type reuseSigner struct {
	secret []byte
	// persistent 密钥是否落盘（--state-dir）。仅用于日志提示。
	persistent bool
}

// newReuseSigner 创建签名器。stateDir 为空：内存随机密钥，进程重启即失效（默认，重启后不允许任何复用）；
// 非空：从 stateDir/reuse-secret 读取，不存在则生成并以 0600 写入（目录 0700）。
// 已存在但长度不对/不可读视为损坏，返回错误而不是静默换成内存密钥（否则运维以为码能跨重启保留）。
func newReuseSigner(stateDir string) (*reuseSigner, error) {
	if stateDir == "" {
		secret := make([]byte, reuseSecretLen)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate reuse secret: %w", err)
		}
		return &reuseSigner{secret: secret}, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, reuseSecretFile)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != reuseSecretLen {
			return nil, fmt.Errorf("reuse secret %s is corrupt (want %d bytes, got %d); delete it to start fresh (all issued codes will change)", path, reuseSecretLen, len(data))
		}
		return &reuseSigner{secret: data, persistent: true}, nil
	case os.IsNotExist(err):
		secret := make([]byte, reuseSecretLen)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate reuse secret: %w", err)
		}
		// O_EXCL：多个 relay 实例同时首启时只有一个写成功，其余回读同一份。
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				return newReuseSigner(stateDir)
			}
			return nil, fmt.Errorf("write reuse secret %s: %w", path, err)
		}
		if _, err := f.Write(secret); err != nil {
			f.Close()
			os.Remove(path)
			return nil, fmt.Errorf("write reuse secret %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			os.Remove(path)
			return nil, fmt.Errorf("write reuse secret %s: %w", path, err)
		}
		return &reuseSigner{secret: secret, persistent: true}, nil
	default:
		return nil, fmt.Errorf("read reuse secret %s: %w", path, err)
	}
}

func (r *reuseSigner) mac(code, clientID string, createdNano []byte) []byte {
	h := hmac.New(sha256.New, r.secret)
	h.Write([]byte(reuseMACDomain))
	var l [4]byte
	for _, f := range []string{code, clientID} {
		binary.BigEndian.PutUint32(l[:], uint32(len(f)))
		h.Write(l[:])
		h.Write([]byte(f))
	}
	h.Write(createdNano)
	return h.Sum(nil)
}

// issue 为 (code, clientID, createdAt) 签发令牌；clientID 为空（无持久身份，无从绑定持有者）返回空串。
func (r *reuseSigner) issue(code, clientID string, createdAt time.Time) string {
	if r == nil || code == "" || clientID == "" {
		return ""
	}
	buf := make([]byte, 8, 8+sha256.Size)
	binary.BigEndian.PutUint64(buf, uint64(createdAt.UnixNano()))
	buf = append(buf, r.mac(code, clientID, buf[:8])...)
	return reuseTokenPref + base64.RawURLEncoding.EncodeToString(buf)
}

// verify 校验令牌确实由本 relay（本密钥）为 (code, clientID) 签发，通过则返回原始签发时间。
func (r *reuseSigner) verify(token, code, clientID string) (time.Time, bool) {
	if r == nil || code == "" || clientID == "" || len(token) > reuseTokenMaxLen || !strings.HasPrefix(token, reuseTokenPref) {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[len(reuseTokenPref):])
	if err != nil || len(raw) != 8+sha256.Size {
		return time.Time{}, false
	}
	if !hmac.Equal(raw[8:], r.mac(code, clientID, raw[:8])) {
		return time.Time{}, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(raw[:8]))), true
}
