package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// code-file 是给宿主程序（管家之类）读的稳定契约，它们不解析 stdout。
// 光给协助码不够：协助端还得知道去哪台 relay 找这个码，而实际连的那台是
// 「编译期默认值 → env → --server → 补端口 → --standalone 改写」算出来的，
// 宿主无从推断。这条测试钉住三个字段都在。
func TestWriteCodeFileIncludesServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code.json")
	exp := time.Now().Add(30 * time.Minute).Truncate(time.Second)

	s := &ShareMode{
		client:    NewClient(&Config{ServerAddr: "127.0.0.1:18498", UseTLS: true, InsecureSkip: true}),
		codeFile:  path,
		code:      "y79CVxX6ce",
		expiresAt: exp,
	}
	s.writeCodeFile()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 code-file: %v", err)
	}
	var got struct {
		Code      string `json:"code"`
		Server    string `json:"server"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("解析 code-file %q: %v", raw, err)
	}
	if got.Code != "y79CVxX6ce" {
		t.Errorf("code=%q", got.Code)
	}
	if got.Server != "127.0.0.1:18498" {
		t.Errorf("server=%q，想要实际连接的地址 127.0.0.1:18498", got.Server)
	}
	if got.ExpiresAt != exp.Unix() {
		t.Errorf("expiresAt=%d，想要 %d", got.ExpiresAt, exp.Unix())
	}
}

// 原子写：宿主程序可能随时在读，不能让它读到写了一半的半截 JSON。
// 写完后目录里不该留下 .tmp。
func TestWriteCodeFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code.json")
	s := &ShareMode{
		client:    NewClient(&Config{ServerAddr: "h:1"}),
		codeFile:  path,
		code:      "ABCD",
		expiresAt: time.Now(),
	}
	s.writeCodeFile()
	s.writeCodeFile() // 重连刷新 code 会覆盖写，必须仍然干净

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("残留临时文件 %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("目录里应只有 code.json，实际 %d 个", len(entries))
	}
}
