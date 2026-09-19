package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// v1 会话在 daemon 侧的行为断言。
//
// 这些分支各自对应 0.0.x 的一条既成事实，改不了也绕不开：那时的 help 不带 AAD 加封、
// 空 result 不加封、调用 ID 也不保证单调。--min-proto=1 之后 daemon 会和这样的对端通话，
// 所以每一条都要有测试钉住，否则以后有人"顺手统一"掉某个分支，兼容模式就废了，
// 而 v2 的测试全都还是绿的。
//
// 反过来也要钉：凡是"复刻旧行为"会削弱安全而兼容收益为零的地方（见
// TestLegacyDaemonRejectsBlankArgs），就**不**复刻，并用测试锁住这个决定。

// newLegacyFixture 建一个 v1 会话的 daemon 夹具。
func newLegacyFixture(t *testing.T) *sealedFixture {
	t.Helper()
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 16)
	conn := &fakeConn{in: in, out: out}

	tool := &countingTool{name: "probe"}
	r := NewRegistry()
	r.Register(tool)
	r.Register(&countingTool{name: "other"})

	key := proto.DeriveSessionKey("ABCD-2345", "nonceA", "nonceB", proto.ToolProtocolVersionV1)
	d := NewDaemon(r, conn, proto.Session{Key: key, Version: proto.ToolProtocolVersionV1})
	go d.RunLoop(context.Background())

	return &sealedFixture{t: t, key: key, out: out, tool: tool, d: d}
}

// sealLegacy 按 v1 的方式加封：无 AAD。
func sealLegacy(t *testing.T, key [32]byte, args string) json.RawMessage {
	t.Helper()
	wrapped, err := proto.AEADSealJSON(&key, json.RawMessage(args), nil)
	if err != nil {
		t.Fatal(err)
	}
	return wrapped
}

// TestLegacyDaemonAcceptsUnAADedArgs v1 会话必须能解开不带 AAD 的密文。
func TestLegacyDaemonAcceptsUnAADedArgs(t *testing.T) {
	f := newLegacyFixture(t)
	f.inject(&proto.ToolReq{ID: 1, Tool: "probe", ArgsJSON: sealLegacy(t, f.key, `{}`)})
	if resp := f.resp(); !resp.OK {
		t.Fatalf("v1 会话应接受无 AAD 的密文，得到 %+v", resp)
	}
}

// TestLegacyDaemonRejectsBlankArgs 空 args 在 v1 会话下同样必须被拒，且拒绝要发生在
// 工具执行之前。两种"空"都要盖住：args:null（真实客户端唯一能发出的形态）和整个
// args 字段缺失（只有手写 JSON 造得出来）。
//
// 这里刻意**不**复刻真实 v1 的 len > 0 判据。真 v1 对"字段缺失"是跳过解密直接派发的，
// 但那条分支合法客户端根本走不到——ToolReq.ArgsJSON 的 tag 没有 omitempty，任何版本
// 的客户端经结构体序列化都必定写出 "args":null。能造出缺字段帧的只有注入方，所以
// 忠实复刻等于专门为攻击者留一条免密钥执行远端工具的路（正是 0a03278 / 7d92bb9 堵掉
// 的那个口子），而兼容性收益是零：真 0.0.x 的无参调用发的是 args:null，在真 v1 那边
// 本来也失败，这里只是把错误码从 decrypt_failed 换成更准确的 unauthenticated。
func TestLegacyDaemonRejectsBlankArgs(t *testing.T) {
	cases := []struct {
		name   string
		inject func(f *sealedFixture)
	}{
		{"args_为_null", func(f *sealedFixture) {
			f.inject(&proto.ToolReq{ID: 1, Tool: "probe"}) // 线上即 args:null
		}},
		{"args_字段缺失", func(f *sealedFixture) {
			f.d.Inject(&proto.Message{
				Type:    proto.MsgToolReq,
				Payload: json.RawMessage(`{"id":1,"tool":"probe","deadline_ms":0}`),
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLegacyFixture(t)
			tc.inject(f)
			resp := f.resp()
			if resp.OK {
				t.Fatalf("v1 会话不得放行空 args，得到 %+v", resp)
			}
			if resp.ErrorCode != "unauthenticated" {
				t.Fatalf("应回 unauthenticated，得到 %q", resp.ErrorCode)
			}
			if atomic.LoadInt32(&f.tool.calls) != 0 {
				t.Fatalf("工具不该被执行，实际 %d 次", atomic.LoadInt32(&f.tool.calls))
			}
		})
	}
}

// TestV2DaemonStillRejectsBlankArgs v2 会话下同样拒绝空 args，且同样不执行工具。
//
// 与 v1 那条成对存在：两个版本现在走的是同一条判据，任何一侧被放松都该有测试变红。
func TestV2DaemonStillRejectsBlankArgs(t *testing.T) {
	f := newSealedFixture(t)
	f.inject(&proto.ToolReq{ID: 1, Tool: "probe"})
	resp := f.resp()
	if resp.OK || resp.ErrorCode != "unauthenticated" {
		t.Fatalf("v2 会话必须拒绝无密文调用，期望 unauthenticated，得到 %+v", resp)
	}
	if atomic.LoadInt32(&f.tool.calls) != 0 {
		t.Fatal("拒绝必须发生在工具执行之前")
	}
}

// TestLegacyDaemonSkipsAntiReplay v1 会话不做抗重放。
//
// 0.0.x 的发送方不保证调用 ID 单调（Bridge 的随机 ID 纪元是后来才加的），在 v1 通道上
// 开滑动窗口会把合法请求误判成 replayed。这里直接投两条同 ID 的请求——v2 下第二条必然
// 被拒（见 TestDaemonReplayWindowKeptOnHotUpgrade），v1 下则必须照常执行。
func TestLegacyDaemonSkipsAntiReplay(t *testing.T) {
	f := newLegacyFixture(t)
	for i := 0; i < 2; i++ {
		f.inject(&proto.ToolReq{ID: 1, Tool: "probe", ArgsJSON: sealLegacy(t, f.key, `{}`)})
		if resp := f.resp(); !resp.OK {
			t.Fatalf("v1 第 %d 次同 ID 调用应被接受，得到 %+v", i+1, resp)
		}
	}
	if atomic.LoadInt32(&f.tool.calls) != 2 {
		t.Fatalf("两次调用都应执行，实际 %d 次", atomic.LoadInt32(&f.tool.calls))
	}
}

// TestLegacyDaemonLeavesEmptyResultUnsealed v1 的空 result 不加封。
//
// 0.0.x 的接收侧按 len(result) > 0 决定要不要解密，这里若按 v2 的规矩封一个 "{}" 过去，
// 对端会把密文当成真实结果解出来。
func TestLegacyDaemonLeavesEmptyResultUnsealed(t *testing.T) {
	f := newLegacyFixture(t)
	sess := proto.Session{Key: f.key, Version: proto.ToolProtocolVersionV1}
	if err := f.d.sendResp(sess, proto.ToolResp{ID: 9, OK: false, ErrorCode: "boom"}); err != nil {
		t.Fatal(err)
	}
	msg := <-f.out
	var resp proto.ToolResp
	if err := proto.DecodePayload(msg, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.ResultJSON) != 0 {
		t.Fatalf("v1 的空 result 不应被加封，实际带了 %d 字节", len(resp.ResultJSON))
	}
}

// TestV2DaemonSealsEmptyResult 反向锁定：v2 下同一条响应必须带密文，因为 ok/error_code
// 这些明文字段唯一的认证依据就是它们进了密文的 AAD。
func TestV2DaemonSealsEmptyResult(t *testing.T) {
	f := newSealedFixture(t)
	if err := f.d.sendResp(proto.Session{Key: f.key}, proto.ToolResp{ID: 9, OK: false, ErrorCode: "boom"}); err != nil {
		t.Fatal(err)
	}
	msg := <-f.out
	var resp proto.ToolResp
	if err := proto.DecodePayload(msg, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.ResultJSON) == 0 {
		t.Fatal("v2 的空 result 必须加封，否则 ok/error_code 无从认证")
	}
}
