package gui

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/remote-assist/tool/internal/client"
)

var errLegacy = errors.New("handshake failed: " + client.LegacyPeerMarker + "工具协议版本不兼容：对端（被协助端）只支持 v1")

// fakeCaller 记录每次调用的 minProto，并按脚本依次返回结果。
type fakeCaller struct {
	calls []string
	errs  []error
}

func (f *fakeCaller) call(_ context.Context, minProto string) (json.RawMessage, error) {
	f.calls = append(f.calls, minProto)
	i := len(f.calls) - 1
	if i < len(f.errs) {
		return nil, f.errs[i]
	}
	return json.RawMessage(`{}`), nil
}

func TestFallbackLegacyOnLANRetriesOnce(t *testing.T) {
	f := &fakeCaller{errs: []error{errLegacy}}
	retried := 0
	res, err := connectWithFallback(context.Background(), f.call, fallbackPolicy{LAN: true}, func() { retried++ })
	if err != nil || !res.FellBack || res.MinProto != "1" {
		t.Fatalf("应降级成功: res=%+v err=%v", res, err)
	}
	if len(f.calls) != 2 || f.calls[0] != "" || f.calls[1] != "1" || retried != 1 {
		t.Fatalf("calls=%v retried=%d", f.calls, retried)
	}
}

// 降级后仍然失败（哪怕还是旧版错误）也绝不再试第三次。
func TestFallbackNeverMoreThanOneRetry(t *testing.T) {
	f := &fakeCaller{errs: []error{errLegacy, errLegacy, errLegacy}}
	res, err := connectWithFallback(context.Background(), f.call, fallbackPolicy{LAN: true}, nil)
	if err == nil || res.FellBack {
		t.Fatalf("应失败: %+v %v", res, err)
	}
	if len(f.calls) != 2 || res.Attempts != 2 {
		t.Fatalf("最多重试 1 次，实际 calls=%v", f.calls)
	}
}

func TestFallbackOtherErrorsDoNotDowngrade(t *testing.T) {
	for _, msg := range []string{
		"join failed: join failed", "relay connect failed: dial tcp: i/o timeout",
		"connect failed: connection refused", "handshake failed: 对端拒绝工具通道握手：" + client.LegacyPeerMarker,
	} {
		f := &fakeCaller{errs: []error{errors.New(msg)}}
		res, err := connectWithFallback(context.Background(), f.call, fallbackPolicy{LAN: true}, nil)
		if err == nil || len(f.calls) != 1 || res.NeedsAsk {
			t.Fatalf("%q 不应降级: calls=%v res=%+v", msg, f.calls, res)
		}
	}
}

func TestFallbackPublicRelayNeedsConfirm(t *testing.T) {
	f := &fakeCaller{errs: []error{errLegacy}}
	res, err := connectWithFallback(context.Background(), f.call, fallbackPolicy{LAN: false}, nil)
	if err == nil || !res.NeedsAsk || len(f.calls) != 1 {
		t.Fatalf("公网中继应要求确认且不自动重试: %+v calls=%v", res, f.calls)
	}
	// 用户确认后：直接 v1 连一次
	f = &fakeCaller{}
	res, err = connectWithFallback(context.Background(), f.call, fallbackPolicy{AllowLegacy: true}, nil)
	if err != nil || res.MinProto != "1" || len(f.calls) != 1 || f.calls[0] != "1" {
		t.Fatalf("确认后应直接 v1: %+v calls=%v", res, f.calls)
	}
}

func TestFallbackNoDowngradeOption(t *testing.T) {
	f := &fakeCaller{errs: []error{errLegacy}}
	res, err := connectWithFallback(context.Background(), f.call, fallbackPolicy{LAN: true, NoDowngrade: true}, nil)
	if err == nil || res.NeedsAsk || len(f.calls) != 1 {
		t.Fatalf("禁止降级时不得重试: %+v calls=%v", res, f.calls)
	}
}

func TestFallbackCanceledNoRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeCaller{errs: []error{errLegacy}}
	call := func(c context.Context, m string) (json.RawMessage, error) {
		cancel()
		return f.call(c, m)
	}
	_, err := connectWithFallback(ctx, call, fallbackPolicy{LAN: true}, nil)
	if err == nil || len(f.calls) != 1 {
		t.Fatalf("取消后不应重试: calls=%v", f.calls)
	}
}

func TestFallbackExplicitMinProtoSingleAttempt(t *testing.T) {
	f := &fakeCaller{errs: []error{errLegacy}}
	_, _ = connectWithFallback(context.Background(), f.call, fallbackPolicy{MinProto: "1", LAN: true}, nil)
	if len(f.calls) != 1 || f.calls[0] != "1" {
		t.Fatalf("显式 min_proto 只连一次: %v", f.calls)
	}
}

func TestClassifyConnectError(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{errLegacy, reasonPeerLegacy},
		{context.Canceled, reasonCanceled},
		{errors.New("connect failed: join failed: join failed"), reasonBadCode},
		{errors.New("relay connect failed: failed to connect: dial tcp 1.2.3.4:5: i/o timeout"), reasonTimeout},
		{errors.New("relay connect failed: failed to connect: dial tcp 127.0.0.1:1: connectex: No connection could be made because the target machine actively refused it."), reasonRefused},
		{errors.New("relay connect failed: failed to connect: dial tcp: lookup x: no such host"), reasonUnreach},
		{errors.New("handshake failed: 对端拒绝工具通道握手：x"), reasonRejected},
		{errors.New("something else"), reasonOther},
	}
	for _, c := range cases {
		if got, _ := classifyConnectError(c.err); got != c.code {
			t.Errorf("%v -> %s, want %s", c.err, got, c.code)
		}
	}
}

func TestIsLANRelay(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8731": true, "192.168.1.5:8731": true, "localhost:1": true,
		"relay.example.com:8731": false, "8.8.8.8:1": false, "": false,
	} {
		if got := isLANRelay(addr, ""); got != want {
			t.Errorf("%q = %v, want %v", addr, got, want)
		}
	}
	if !isLANRelay("", "10.0.0.2:8731") {
		t.Error("server 为空时应回退到 defaultServer")
	}
}
