package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// countingTool 记录自己被调用了几次，用来断言「拒绝发生在 Dispatch 之前」。
type countingTool struct {
	name  string
	calls int32
}

func (c *countingTool) Name() string { return c.name }
func (c *countingTool) Run(ctx context.Context, in json.RawMessage, out StreamSink) (json.RawMessage, error) {
	atomic.AddInt32(&c.calls, 1)
	return json.RawMessage(`{"ok":true}`), nil
}

type sealedFixture struct {
	t    *testing.T
	key  [32]byte
	out  chan *proto.Message
	tool *countingTool
	d    *Daemon
}

func newSealedFixture(t *testing.T) *sealedFixture {
	t.Helper()
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 16)
	conn := &fakeConn{in: in, out: out}

	tool := &countingTool{name: "probe"}
	r := NewRegistry()
	r.Register(tool)
	// 第二个工具用于「改挂到别的工具上」的断言。
	r.Register(&countingTool{name: "other"})

	key := proto.DeriveSessionKey("ABCD-2345", "nonceA", "nonceB")
	d := NewDaemon(r, conn, key)
	go d.RunLoop(context.Background())

	return &sealedFixture{t: t, key: key, out: out, tool: tool, d: d}
}

// seal 按 id/tool 正确封装 args。
func (f *sealedFixture) seal(id uint64, tool string, args string) json.RawMessage {
	f.t.Helper()
	wrapped, err := proto.AEADSealJSON(&f.key, json.RawMessage(args), proto.ToolReqAAD(id, tool, 0))
	if err != nil {
		f.t.Fatal(err)
	}
	return wrapped
}

func (f *sealedFixture) inject(req *proto.ToolReq) {
	f.t.Helper()
	msg, err := proto.NewMessage(proto.MsgToolReq, req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.d.Inject(msg)
}

func (f *sealedFixture) resp() proto.ToolResp {
	f.t.Helper()
	select {
	case msg := <-f.out:
		if msg.Type != proto.MsgToolResp {
			f.t.Fatalf("期望 ToolResp，收到 %s", msg.Type)
		}
		var resp proto.ToolResp
		if err := proto.DecodePayload(msg, &resp); err != nil {
			f.t.Fatal(err)
		}
		return resp
	case <-time.After(2 * time.Second):
		f.t.Fatal("2s 内无响应")
		return proto.ToolResp{}
	}
}

// TestSealedReqRoundTrip 正常路径：封好的请求能解开、能执行、响应也是密文。
func TestSealedReqRoundTrip(t *testing.T) {
	f := newSealedFixture(t)
	f.inject(&proto.ToolReq{ID: 1, Tool: "probe", ArgsJSON: f.seal(1, "probe", `{}`)})

	resp := f.resp()
	if !resp.OK {
		t.Fatalf("期望成功，得到 %+v", resp)
	}
	plain, err := proto.AEADOpenJSON(&f.key, resp.ResultJSON, proto.ToolRespAAD(1, true, "", ""))
	if err != nil {
		t.Fatalf("响应解密失败: %v", err)
	}
	if string(plain) != `{"ok":true}` {
		t.Fatalf("结果 = %s", plain)
	}
	if got := atomic.LoadInt32(&f.tool.calls); got != 1 {
		t.Fatalf("工具调用次数 = %d", got)
	}
}

// TestSealedReqRejectsEmptyArgs 这是 #22 的核心断言：握手后不带 args 的请求必须被拒，
// 且拒绝要发生在 Dispatch 之前。以前的判据是 len(ArgsJSON) > 0，注入方发一条
// tool_req{tool:"process_list"} 不带 args 就能绕过全部解密直接触发远端执行。
func TestSealedReqRejectsEmptyArgs(t *testing.T) {
	f := newSealedFixture(t)
	f.inject(&proto.ToolReq{ID: 2, Tool: "probe"})

	resp := f.resp()
	if resp.OK || resp.ErrorCode != "unauthenticated" {
		t.Fatalf("期望 unauthenticated，得到 %+v", resp)
	}
	if got := atomic.LoadInt32(&f.tool.calls); got != 0 {
		t.Fatalf("工具在鉴权失败后仍被执行了 %d 次", got)
	}
}

// TestSealedReqRejectsCrossToolRedirect #20 的核心断言：把一条合法密文改挂到别的
// 工具名上必须解密失败。没有 AAD 时 tool 只是外层明文，改一个字段就能让同一份 args
// 落到另一个工具的语义里（read_file 的 args 挂到 write_file 会把文件截断成 0 字节）。
func TestSealedReqRejectsCrossToolRedirect(t *testing.T) {
	f := newSealedFixture(t)
	args := f.seal(3, "probe", `{}`)
	f.inject(&proto.ToolReq{ID: 3, Tool: "other", ArgsJSON: args})

	resp := f.resp()
	if resp.OK || resp.ErrorCode != "decrypt_failed" {
		t.Fatalf("期望 decrypt_failed，得到 %+v", resp)
	}
}

// TestSealedReqRejectsIDRewrite 改 ID 同样应当解密失败（ID 也在 AAD 里）。
func TestSealedReqRejectsIDRewrite(t *testing.T) {
	f := newSealedFixture(t)
	args := f.seal(4, "probe", `{}`)
	f.inject(&proto.ToolReq{ID: 5, Tool: "probe", ArgsJSON: args})

	resp := f.resp()
	if resp.OK || resp.ErrorCode != "decrypt_failed" {
		t.Fatalf("期望 decrypt_failed，得到 %+v", resp)
	}
}

// TestSealedReqRejectsDeadlineRewrite deadline_ms 也是明文，改大它能让一次注入的
// 调用跑得远比发起方允许的久，所以也必须进 AAD。
func TestSealedReqRejectsDeadlineRewrite(t *testing.T) {
	f := newSealedFixture(t)
	args := f.seal(6, "probe", `{}`)
	f.inject(&proto.ToolReq{ID: 6, Tool: "probe", DeadlineMs: 600000, ArgsJSON: args})

	resp := f.resp()
	if resp.OK || resp.ErrorCode != "decrypt_failed" {
		t.Fatalf("期望 decrypt_failed，得到 %+v", resp)
	}
}

// TestSealedReqRejectsExactReplay #21 的核心断言：AAD 挡住了改字段，但一字不改地
// 重放整条请求仍然成立（nonce 由发送方给），只能靠接收侧去重。
func TestSealedReqRejectsExactReplay(t *testing.T) {
	f := newSealedFixture(t)
	req := &proto.ToolReq{ID: 7, Tool: "probe", ArgsJSON: f.seal(7, "probe", `{}`)}

	f.inject(req)
	if resp := f.resp(); !resp.OK {
		t.Fatalf("第一次应成功，得到 %+v", resp)
	}

	f.inject(req) // 原样重放
	resp := f.resp()
	if resp.OK || resp.ErrorCode != "replayed" {
		t.Fatalf("期望 replayed，得到 %+v", resp)
	}
	if got := atomic.LoadInt32(&f.tool.calls); got != 1 {
		t.Fatalf("重放导致工具被执行了 %d 次", got)
	}
}

// TestSealedReqPlainChannelUnaffected key 为零（未握手的明文通道，如本地 stdio）
// 时不得启用上述任何一条限制，否则本地直连模式会全线不可用。
func TestSealedReqPlainChannelUnaffected(t *testing.T) {
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 16)
	tool := &countingTool{name: "probe"}
	r := NewRegistry()
	r.Register(tool)
	d := NewDaemon(r, &fakeConn{in: in, out: out}, [32]byte{})
	go d.RunLoop(context.Background())

	msg, _ := proto.NewMessage(proto.MsgToolReq, &proto.ToolReq{ID: 8, Tool: "probe", ArgsJSON: json.RawMessage(`{}`)})
	d.Inject(msg)
	d.Inject(msg) // 同一个 ID 再来一次：明文通道不做去重

	for i := 0; i < 2; i++ {
		select {
		case got := <-out:
			var resp proto.ToolResp
			proto.DecodePayload(got, &resp)
			if !resp.OK {
				t.Fatalf("第 %d 次应成功，得到 %+v", i+1, resp)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次无响应", i+1)
		}
	}
}

// TestSealedRespSealsErrors 握手后**每一条**响应都要加封，包括 ResultJSON 本来为空的
// 错误响应：密文里的 MAC 是 ok / error_code / error_msg 唯一的认证依据。不封的话，
// 中间人可以随手把一次失败改写成"成功 + 空结果"，接收侧无从分辨。
func TestSealedRespSealsErrors(t *testing.T) {
	f := newSealedFixture(t)
	f.inject(&proto.ToolReq{ID: 2, Tool: "probe"}) // 不带 args -> unauthenticated

	resp := f.resp()
	if resp.OK || resp.ErrorCode != "unauthenticated" {
		t.Fatalf("期望 unauthenticated，得到 %+v", resp)
	}
	if len(resp.ResultJSON) == 0 {
		t.Fatal("错误响应没有加封，ok/error_code 全无认证")
	}
	if _, err := proto.AEADOpenJSON(&f.key, resp.ResultJSON, proto.ToolRespAAD(resp.ID, resp.OK, resp.ErrorCode, resp.ErrorMsg)); err != nil {
		t.Fatalf("错误响应的 AAD 未绑定自身的明文字段: %v", err)
	}
	// 改一个字段就该验不过。
	if _, err := proto.AEADOpenJSON(&f.key, resp.ResultJSON, proto.ToolRespAAD(resp.ID, true, resp.ErrorCode, resp.ErrorMsg)); err == nil {
		t.Fatal("翻转 ok 后仍验得过")
	}
}

// TestSealedRespPlainChannelStaysPlain key 为零时不得加封，否则本地 stdio 直连模式
// 的调用方会拿到一坨 base64。
func TestSealedRespPlainChannelStaysPlain(t *testing.T) {
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 16)
	r := NewRegistry()
	r.Register(&countingTool{name: "probe"})
	d := NewDaemon(r, &fakeConn{in: in, out: out}, [32]byte{})
	go d.RunLoop(context.Background())

	msg, _ := proto.NewMessage(proto.MsgToolReq, &proto.ToolReq{ID: 1, Tool: "probe", ArgsJSON: json.RawMessage(`{}`)})
	d.Inject(msg)

	select {
	case got := <-out:
		var resp proto.ToolResp
		proto.DecodePayload(got, &resp)
		if string(resp.ResultJSON) != `{"ok":true}` {
			t.Fatalf("明文通道的结果被改写了: %s", resp.ResultJSON)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("无响应")
	}
}

// TestBusyRespIsSealed Inject 缓冲满时回的 server_busy 也必须加封。
// 这条路径绕开了 handleReq（请求根本没进 daemon），最容易在收紧响应认证时被漏掉：
// host 侧现在「key 非零就要求响应带密文」，裸发的 server_busy 会被判成
// unauthenticated，运维看到的是一条误导人的鉴权错误，而不是真正的「daemon 过载」。
func TestBusyRespIsSealed(t *testing.T) {
	out := make(chan *proto.Message, 4)
	r := NewRegistry()
	r.Register(&countingTool{name: "probe"})
	key := proto.DeriveSessionKey("ABCD-2345", "nonceA", "nonceB")
	// 故意不起 RunLoop：没人消费 inbound，塞满即触发 default 分支。
	d := NewDaemon(r, &fakeConn{in: make(chan *proto.Message, 4), out: out}, key)

	msg, err := proto.NewMessage(proto.MsgToolReq, &proto.ToolReq{ID: 42, Tool: "probe", ArgsJSON: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(d.inbound); i++ {
		d.Inject(msg)
	}
	d.Inject(msg) // 第 65 条：缓冲已满

	select {
	case got := <-out:
		var resp proto.ToolResp
		if err := proto.DecodePayload(got, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.ErrorCode != "server_busy" {
			t.Fatalf("期望 server_busy，得到 %+v", resp)
		}
		if len(resp.ResultJSON) == 0 {
			t.Fatal("server_busy 响应未加封，host 会把它判成 unauthenticated 并丢掉真正的原因")
		}
		if _, err := proto.AEADOpenJSON(&key, resp.ResultJSON, proto.ToolRespAAD(resp.ID, resp.OK, resp.ErrorCode, resp.ErrorMsg)); err != nil {
			t.Fatalf("server_busy 响应的 AAD 未绑定自身明文字段: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("缓冲满时没有回 server_busy")
	}
}
