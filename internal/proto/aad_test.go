package proto

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"testing"
)

func testKey(t *testing.T) [32]byte {
	t.Helper()
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAEADAADMustMatch(t *testing.T) {
	key := testKey(t)
	plain := []byte(`{"path":"/etc/passwd"}`)
	ct, err := AEADSeal(&key, plain, ToolReqAAD(1, "read_file", 0))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := AEADOpen(&key, ct, ToolReqAAD(1, "read_file", 0)); err != nil || !bytes.Equal(out, plain) {
		t.Fatalf("同 AAD 应能解开: out=%s err=%v", out, err)
	}
	// 换成别的工具名 —— 这正是「把 read_file 的 args 改挂到 write_file」的攻击。
	if _, err := AEADOpen(&key, ct, ToolReqAAD(1, "write_file", 0)); err == nil {
		t.Fatal("AAD 不一致却解开了")
	}
	if _, err := AEADOpen(&key, ct, nil); err == nil {
		t.Fatal("不带 AAD 却解开了")
	}
}

func TestAEADSealJSONAADMustMatch(t *testing.T) {
	key := testKey(t)
	wrapped, err := AEADSealJSON(&key, json.RawMessage(`{"ok":1}`), ToolRespAAD(7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AEADOpenJSON(&key, wrapped, ToolRespAAD(8)); err == nil {
		t.Fatal("换一个调用 ID 却解开了")
	}
	if _, err := AEADOpenJSON(&key, wrapped, ToolRespAAD(7)); err != nil {
		t.Fatalf("同 AAD 应能解开: %v", err)
	}
}

// TestAADDomainsAreDistinct 三个方向共用同一把 key，若标签不区分，请求的密文可以被
// 当作响应或流帧重放回去。
func TestAADDomainsAreDistinct(t *testing.T) {
	req := ToolReqAAD(1, "", 0)
	resp := ToolRespAAD(1)
	chunk := StreamChunkAAD(1, 0, "")
	if bytes.Equal(req, resp) || bytes.Equal(req, chunk) || bytes.Equal(resp, chunk) {
		t.Fatalf("三个方向的 AAD 出现重合: req=%x resp=%x chunk=%x", req, resp, chunk)
	}
}

// TestAADFieldsAreUnambiguous 长度前缀的意义：不加的话 tool="ab"+后续字段 "c" 会和
// tool="abc" 拼出同一串，绑定就形同虚设。
func TestAADFieldsAreUnambiguous(t *testing.T) {
	if bytes.Equal(ToolReqAAD(1, "ab", 0), ToolReqAAD(1, "abc", 0)) {
		t.Fatal("不同工具名产生了相同 AAD")
	}
	if bytes.Equal(StreamChunkAAD(1, 0, "stdout"), StreamChunkAAD(1, 0, "stderr")) {
		t.Fatal("不同流别产生了相同 AAD")
	}
	if bytes.Equal(StreamChunkAAD(1, 0, "stdout"), StreamChunkAAD(1, 1, "stdout")) {
		t.Fatal("不同 seq 产生了相同 AAD")
	}
	if bytes.Equal(ToolReqAAD(1, "exec", 1000), ToolReqAAD(1, "exec", 600000)) {
		t.Fatal("不同 deadline 产生了相同 AAD")
	}
}

// TestToolProtocolVersionIsV2 v2 的三项变更（AAD / 强制密文 args / 抗重放）都不向后
// 兼容。版本号必须一起抬，否则旧端会在每条请求上收到 decrypt_failed，而不是握手阶段
// 一条可读的"版本不支持"。
func TestToolProtocolVersionIsV2(t *testing.T) {
	if ToolProtocolVersion != "2" {
		t.Fatalf("ToolProtocolVersion = %q，AAD/强制密文/抗重放要求版本为 2", ToolProtocolVersion)
	}
}
