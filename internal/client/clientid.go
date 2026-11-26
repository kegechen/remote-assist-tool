package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const clientIDFileName = ".remote_assist_client_id"

// 文件已存在却读到空内容时，最多等 clientIDWaitTries 次、每次 clientIDWaitStep：多半是另一个进程刚建好、
// 还没写完。一直等不到（上次写到一半退出，留下空文件）才自己写。
const (
	clientIDWaitTries = 50
	clientIDWaitStep  = 20 * time.Millisecond
)

// GetOrCreateClientID 获取或创建持久化的客户端ID
func GetOrCreateClientID() (string, error) {
	id, _, err := loadOrCreateClientID()
	return id, err
}

// loadOrCreateClientID 同 GetOrCreateClientID，另外报告 ID 是否已落盘：主目录拿不到、文件建不了或读不了时
// 返回的是临时 ID，下次调用会是另一个值。
//
// share 和 help 都会读写这个文件，首次运行时可能同时创建：用 O_EXCL 保证只有一方建成，其余读它写进去的值。
// 各写各的会互相覆盖，先注册的 share 下次重连读到别人的值，ClientID 一变，relay 复用不了原会话，协助码跟着变。
func loadOrCreateClientID() (id string, persisted bool, err error) {
	// 尝试从用户主目录读取
	homeDir, err := os.UserHomeDir()
	if err != nil {
		// 如果无法获取主目录，生成临时ID
		id, err = generateClientID()
		return id, false, err
	}
	idPath := filepath.Join(homeDir, clientIDFileName)

	id, err = generateClientID()
	if err != nil {
		return id, false, err
	}
	for try := 0; try < clientIDWaitTries; try++ {
		data, rerr := os.ReadFile(idPath)
		switch {
		case rerr == nil && len(data) > 0:
			return string(data), true, nil
		case errors.Is(rerr, fs.ErrNotExist):
			cerr := createClientIDFile(idPath, id)
			if cerr == nil {
				return id, true, nil
			}
			if !errors.Is(cerr, fs.ErrExist) {
				return id, false, nil // 目录不存在、没有权限：这次用临时 ID
			}
			// 别人抢先建成了，回头读它的
		case rerr != nil:
			return id, false, nil // 读不了（没有权限、被占用）：不覆盖原文件，这次用临时 ID
		default:
			time.Sleep(clientIDWaitStep) // 存在但为空：等建文件的一方写完
		}
	}
	// 一直是空文件：上次写到一半退出留下的，自己写一个。几个进程恰好同时等满时仍会互相覆盖，
	// 只在这种「上次崩在写入中途」之后的首次启动才会碰上，不再为它加锁。
	return id, os.WriteFile(idPath, []byte(id), 0600) == nil, nil
}

// createClientIDFile 只在文件不存在时创建并写入 id；文件已存在返回 fs.ErrExist。写入失败（磁盘满）时删掉
// 刚建的文件：留下空文件会让之后每次调用都先白等 clientIDWaitTries 轮。
func createClientIDFile(path, id string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(id)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
	}
	return werr
}

// helpDeviceIDPrefix 是 help 设备标识的域分隔前缀，改了它所有 help 的设备键都会变。
const helpDeviceIDPrefix = "remote-assist/help-device-id/v1\x00"

// helpDeviceID 返回 help 加入 relay（证书指纹 relayFP，明文连接为空）时自报的设备标识：由本机持久 ClientID
// 与 relayFP 单向派生（SHA-256 截断 16 字节），relay 只拿它区分设备。
//
// 不发 ClientID 原文：relay 凭 ClientID 就能接管这台机器上 share 的会话、拿到当前协助码，而 help 连上的
// relay 不一定可信。混进 relayFP：同一台机器发给每个 relay 的值都不同，恶意 relay 拿到的值到别的 relay 上
// 冒名制造失败封不掉这台设备，几个 relay 之间也没法按它把同一个用户对上。这只在 TLS 下成立：明文连接（--plain，
// 仅开发测试）relayFP 为空，发给所有明文 relay 的值都一样，而且路上看得见（README「按设备计分」写明了）。
//
// ClientID 落不了盘（每次都是新值）时返回空：带了 relay 也只会把每次加入当成新设备，不如不带，
// 让 relay 退回按身份串加来源计。
func helpDeviceID(relayFP string) string {
	cid, persisted, err := loadOrCreateClientID()
	if err != nil || !persisted || cid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(helpDeviceIDPrefix + relayFP + "\x00" + cid))
	return "hd1_" + hex.EncodeToString(sum[:16])
}

func generateClientID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		// 降级方案
		return "cid_" + randomStringFallback(16), nil
	}
	return "cid_" + hex.EncodeToString(b), nil
}

func randomStringFallback(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[i%len(charset)]
	}
	return string(b)
}
