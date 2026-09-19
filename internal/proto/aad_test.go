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
	wrapped, err := AEADSealJSON(&key, json.RawMessage(`{"ok":1}`), ToolRespAAD(7, true, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AEADOpenJSON(&key, wrapped, ToolRespAAD(8, true, "", "")); err == nil {
		t.Fatal("换一个调用 ID 却解开了")
	}
	if _, err := AEADOpenJSON(&key, wrapped, ToolRespAAD(7, true, "", "")); err != nil {
		t.Fatalf("同 AAD 应能解开: %v", err)
	}
}

// TestAADDomainsAreDistinct 三个方向共用同一把 key，若标签不区分，请求的密文可以被
// 当作响应或流帧重放回去。
func TestAADDomainsAreDistinct(t *testing.T) {
	req := ToolReqAAD(1, "", 0)
	resp := ToolRespAAD(1, false, "", "")
	chunk := StreamChunkAAD(1, 0, "", false)
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
	if bytes.Equal(StreamChunkAAD(1, 0, "stdout", false), StreamChunkAAD(1, 0, "stderr", false)) {
		t.Fatal("不同流别产生了相同 AAD")
	}
	if bytes.Equal(StreamChunkAAD(1, 0, "stdout", false), StreamChunkAAD(1, 1, "stdout", false)) {
		t.Fatal("不同 seq 产生了相同 AAD")
	}
	if bytes.Equal(ToolReqAAD(1, "exec", 1000), ToolReqAAD(1, "exec", 600000)) {
		t.Fatal("不同 deadline 产生了相同 AAD")
	}
}

// TestToolProtocolVersionIsV2 v2 的几项变更（AAD / 强制密文 args / 抗重放）改变了线上
// 格式，必须由版本号承载，这样不匹配的两端在握手阶段就能谈出结论，而不是在每条请求上
// 收到 decrypt_failed。
//
// 互通性现在由版本协商负责（见 handshake.go），不再靠"两端编译期常量恰好相等"。
func TestToolProtocolVersionIsV2(t *testing.T) {
	if ToolProtocolVersion != "2" {
		t.Fatalf("ToolProtocolVersion = %q，AAD/强制密文/抗重放要求版本为 2", ToolProtocolVersion)
	}
}

// TestDefaultMinProtoRefusesV1 锁定默认不降级到 v1。
//
// 这是整个兼容方案的安全支点：一旦默认值放宽到 v1，不可信的 relay 只要从 Hello 里删掉
// versions 字段，就能把两个 v2 端悄悄打回无 AAD、无抗重放的通道。兼容性由
// --min-proto=1 显式承担，不能靠改这个默认值来换。
//
// 断言的是"拒绝 v1"这个行为，而不是"等于 ToolProtocolVersion"这个恒等式 —— 后者会在
// 下次升版本时把 v3 默认拒绝 v2 一并锁成正确答案，理由见
// TestDefaultMinProtoIsLowestAuthenticatedVersion。
func TestDefaultMinProtoRefusesV1(t *testing.T) {
	if versionRank(DefaultMinProto) >= versionRank(ToolProtocolVersionV1) {
		t.Fatalf("DefaultMinProto = %q，默认不得接受 v1", DefaultMinProto)
	}
}
