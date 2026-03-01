package client

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

const clientIDFileName = ".remote_assist_client_id"

// GetOrCreateClientID 获取或创建持久化的客户端ID
func GetOrCreateClientID() (string, error) {
	// 尝试从用户主目录读取
	homeDir, err := os.UserHomeDir()
	if err != nil {
		// 如果无法获取主目录，生成临时ID
		return generateClientID()
	}

	idPath := filepath.Join(homeDir, clientIDFileName)

	// 尝试读取现有ID
	if data, err := os.ReadFile(idPath); err == nil && len(data) > 0 {
		return string(data), nil
	}

	// 生成新ID
	id, err := generateClientID()
	if err != nil {
		return id, err
	}

	// 保存ID
	_ = os.WriteFile(idPath, []byte(id), 0600)

	return id, nil
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
