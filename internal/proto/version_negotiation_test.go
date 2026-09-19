package proto

import (
	"errors"
	"strings"
	"testing"
)

// legacyHello 复刻 0.0.x 的 help 端发出的 Hello：只有 version 字段，没有 versions。
// 这是兼容性测试的基准样本——0.0.9 及更早的线上客户端就是这么发的。
func legacyHello() Hello {
	return Hello{
		Version:      ToolProtocolVersionV1,
		Capabilities: []string{"exec", "read_file"},
		NonceB64:     NewNonceB64(),
	}
}

// negotiateAsShare 走 share 端的协商路径。
func negotiateAsShare(h Hello, minProto string) (string, error) {
	return NegotiateToolVersion(h.Version, h.Versions, minProto, SideShare, SideHelp)
}

// TestNegotiationMatrix 覆盖新旧两端的四种组合 × 两种 min-proto 设置。
//
// 这张表就是整个改动要守住的契约：默认配置下新旧不互通（且给出可操作提示），放宽之后
// 互通且谈成 v1，而两个新端无论开不开兼容开关都必须谈成 v2。
func TestNegotiationMatrix(t *testing.T) {
	modernDefault := NewHello("")                   // 新 help，默认不降级
	modernCompat := NewHello(ToolProtocolVersionV1) // 新 help，开了 --min-proto=1
	legacy := legacyHello()                         // 0.0.x 的 help

	cases := []struct {
		name          string
		hello         Hello
		shareMinProto string
		wantVersion   string
		wantErr       bool
	}{
		{"新help+新share：谈成v2", modernDefault, "", ToolProtocolVersion, false},
		{"新help兼容模式+新share默认：仍谈成v2", modernCompat, "", ToolProtocolVersion, false},
		{"新help兼容模式+新share兼容：仍优先v2", modernCompat, ToolProtocolVersionV1, ToolProtocolVersion, false},
		{"旧help+新share默认：拒绝", legacy, "", "", true},
		{"旧help+新share兼容：谈成v1", legacy, ToolProtocolVersionV1, ToolProtocolVersionV1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := negotiateAsShare(tc.hello, tc.shareMinProto)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝，却谈成了 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外失败: %v", err)
			}
			if got != tc.wantVersion {
				t.Fatalf("协商版本 = %q，期望 %q", got, tc.wantVersion)
			}
		})
	}
}

// acceptedByStrictEqualityShare 复刻"只认 Version 字段严格相等"的旧 share 判据。
//
// 0.0.x 与已发布的 1.0.0 都是这个形状，区别只在它要求的常量：0.0.x 要 "1"，1.0.0 要 "2"。
// 两者都没有 Versions 字段，看不到我们通告的列表。
func acceptedByStrictEqualityShare(h Hello, theirConstant string) bool {
	return h.Version == theirConstant
}

// TestFirstHelloReachesReleasedV2Share 第一轮 Hello 必须能过 1.0.0 的相等比对——
// **哪怕开了 --min-proto=1**。
//
// 这是兼容锚点最容易搞反的一处：一个锚点值只能讨好一代人，而 1.0.0 是当前已发布版本。
// 若一上来就发 "1" 去迁就 0.0.x，一个本意放宽兼容的开关反而会打断所有 1.0.0 对端——
// 比没有这个开关还糟。所以第一轮固定发最高版本，兼容锚点留到第二轮。
func TestFirstHelloReachesReleasedV2Share(t *testing.T) {
	for _, minProto := range []string{"", ToolProtocolVersionV1} {
		h := NewHello(minProto)
		if !acceptedByStrictEqualityShare(h, ToolProtocolVersionV2) {
			t.Fatalf("min-proto=%q 时第一轮 Version=%q，会被已发布的 1.0.0 share 拒绝", minProto, h.Version)
		}
	}
}

// TestFallbackHelloReachesLegacyShare 第二轮的兼容锚点必须能过 0.0.x 的相等比对。
func TestFallbackHelloReachesLegacyShare(t *testing.T) {
	h := NewFallbackHello(ToolProtocolVersionV1)
	if !acceptedByStrictEqualityShare(h, ToolProtocolVersionV1) {
		t.Fatalf("兼容重试的 Version=%q，过不了 0.0.x 的相等比对", h.Version)
	}
	// 同时新 share 仍要能从 versions 里看出它支持 v2。
	if got, err := negotiateAsShare(h, ""); err != nil || got != ToolProtocolVersion {
		t.Fatalf("新 share 应从 versions 谈成 v2，得到 %q err=%v", got, err)
	}
}

// TestFallbackRetryOnlyInCompatMode 默认配置下被拒就是被拒，不能变着法再试一轮，
// 否则"默认不降级"就成了空话。
func TestFallbackRetryOnlyInCompatMode(t *testing.T) {
	rejected := HelloAck{Accept: false, Version: ToolProtocolVersionV1}
	if ShouldRetryWithFallbackAnchor(rejected, "") {
		t.Fatal("默认配置不得用兼容锚点重试")
	}
	if !ShouldRetryWithFallbackAnchor(rejected, ToolProtocolVersionV1) {
		t.Fatal("开了 --min-proto=1 就该重试一轮")
	}
	if ShouldRetryWithFallbackAnchor(HelloAck{Accept: true}, ToolProtocolVersionV1) {
		t.Fatal("已被接受的握手不该重试")
	}
}

// TestCompatModeHelloIsAcceptedByLegacyShare 锁定 legacy_version 锚点这个关键技巧。
//
// 0.0.x 的 share 只看 Hello.Version 且做严格相等比对（当年的代码是
// `if hello.Version != ToolProtocolVersion` → 拒绝，那时常量是 "1"）。所以开了兼容模式
// 的新 help 必须把 Version 填成 "1" 才能被它接受；真正的协商能力放在它看不懂、
// 因而会忽略的 versions 字段里。
//
// 这里直接复刻旧 share 的判据，而不是调用现在的协商函数——要测的正是"对一段我们已经
// 改不动的旧代码来说，这个 Hello 长得对不对"。
func TestCompatModeHelloIsAcceptedByLegacyShare(t *testing.T) {
	// 兼容锚点在**第二轮**。第一轮固定发最高版本，理由见 TestFirstHelloReachesReleasedV2Share。
	h := NewFallbackHello(ToolProtocolVersionV1)
	// 旧 share 的原始判据：`if hello.Version != <它的常量 "1"> { 拒绝 }`。
	if h.Version != ToolProtocolVersionV1 {
		t.Fatalf("兼容重试的 Version 必须是 v1 锚点，得到 %q —— 0.0.x 的 share 会拒绝", h.Version)
	}
	// 同时新 share 仍要能从 versions 里看出它支持 v2。
	if got, err := negotiateAsShare(h, ""); err != nil || got != ToolProtocolVersion {
		t.Fatalf("新 share 应从 versions 谈成 v2，得到 %q err=%v", got, err)
	}
}

// TestDefaultHelloIsRejectedByLegacyShare 默认配置下 Hello.Version 是 "2"，旧 share 的
// 严格相等比对会拒绝它 —— 这是**期望行为**，不是缺陷：它保证不会发生静默降级，用户
// 拿到的是一条带升级指引的明确失败。
func TestDefaultHelloIsRejectedByLegacyShare(t *testing.T) {
	h := NewHello("")
	if h.Version == ToolProtocolVersionV1 {
		t.Fatal("默认配置不该发 v1 锚点，否则等于默认允许降级")
	}
}

// TestInterpretHelloAckRejectsUnilateralDowngrade 这是协商链路上最容易被绕过的一环。
//
// HelloAck.Version 由应答方单方面选定。发起方若照单全收，一条伪造的
// Accept:true + Version:"1" 就能把一个要求 v2 的发起方拖进 v1 会话——不需要任何密钥，
// 改一个 JSON 字段即可，而这正是版本协商本该防住的事。
func TestInterpretHelloAckRejectsUnilateralDowngrade(t *testing.T) {
	ack := HelloAck{
		Version:  ToolProtocolVersionV1,
		Versions: []string{ToolProtocolVersionV2, ToolProtocolVersionV1},
		Accept:   true,
		NonceB64: NewNonceB64(),
	}
	if _, err := InterpretHelloAck(ack, "", SideHelp, SideShare); err == nil {
		t.Fatal("默认配置下必须拒绝对端单方面选定的 v1")
	}
	// 放宽到 v1 之后这条**仍然**要拒：它同时通告支持 v2，双方都能用更高版本却谈成低的，
	// 那是提议被篡改的签名，见 TestInterpretHelloAckDetectsTamperedDowngrade。
	if _, err := InterpretHelloAck(ack, ToolProtocolVersionV1, SideHelp, SideShare); err == nil {
		t.Fatal("对端自称支持 v2 却选 v1，即便放宽到 v1 也必须拒绝")
	}
	// 只有"确实只会 v1"的对端（0.0.x 不发 Versions）才在放宽后被接受。
	genuine := HelloAck{Version: ToolProtocolVersionV1, Accept: true, NonceB64: NewNonceB64()}
	got, err := InterpretHelloAck(genuine, ToolProtocolVersionV1, SideHelp, SideShare)
	if err != nil {
		t.Fatalf("放宽到 v1 后应接受真实 0.0.x 对端: %v", err)
	}
	if got != ToolProtocolVersionV1 {
		t.Fatalf("应采纳对端选定的 v1，得到 %q", got)
	}
}

// TestInterpretHelloAckRejectsUnknownVersion 对端选一个本端不认识的版本时必须失败，
// 而不是当成某个默认值继续。
func TestInterpretHelloAckRejectsUnknownVersion(t *testing.T) {
	ack := HelloAck{Version: "999", Accept: true, NonceB64: NewNonceB64()}
	if _, err := InterpretHelloAck(ack, ToolProtocolVersionV1, SideHelp, SideShare); err == nil {
		t.Fatal("未知版本必须拒绝")
	}
}

// TestRejectionHintsAreActionable 拒绝理由必须可操作，且指向**能加参数的那一端**。
//
// --min-proto 只存在于新版本里，旧版根本没有这个旗标，所以提示永远该指向本端（拒绝方）。
// 指错地方的话，用户会跑去一台改不了的机器上找参数。
func TestRejectionHintsAreActionable(t *testing.T) {
	_, err := negotiateAsShare(legacyHello(), "")
	if err == nil {
		t.Fatal("默认配置应拒绝 v1")
	}
	var incompat *IncompatibleVersionError
	if !errors.As(err, &incompat) {
		t.Fatalf("应返回 IncompatibleVersionError，得到 %T", err)
	}

	// 给对端看的那份：读者在另一台机器前，必须用角色名指路到 share 端。
	msg := incompat.Message()
	if !strings.Contains(msg, "--min-proto=1") {
		t.Fatalf("对端提示缺少可操作办法: %s", msg)
	}
	if !strings.Contains(msg, SideShare) {
		t.Fatalf("对端提示必须指明该在被协助端加参数: %s", msg)
	}

	// 给本机看的那份：读者就在该动手的机器前，说"本端"。
	hint := incompat.LocalHint()
	if !strings.Contains(hint, "--min-proto=1") {
		t.Fatalf("本机提示缺少可操作办法: %s", hint)
	}
	if !strings.Contains(hint, "本端") {
		t.Fatalf("本机提示应指向本端: %s", hint)
	}
}

// TestSessionKeyIsVersionBound v1 与 v2 的会话密钥必须不同（HKDF info 里带版本）。
// 更要紧的是反向的那一条：协商到 v1 时，两端都必须按 v1 派生，否则握手成功了、
// 密钥却对不上，表现为之后每条请求 decrypt_failed。
func TestSessionKeyIsVersionBound(t *testing.T) {
	v1 := DeriveSessionKey("CODE-1234", "ns", "nh", ToolProtocolVersionV1)
	v2 := DeriveSessionKey("CODE-1234", "ns", "nh", ToolProtocolVersionV2)
	if v1 == v2 {
		t.Fatal("不同协议版本必须派生出不同会话密钥")
	}
	// 空版本按最高版本解释，这是 Session 零值 fail-safe 的基础。
	if DeriveSessionKey("CODE-1234", "ns", "nh", "") != v2 {
		t.Fatal("空版本应按当前最高版本派生")
	}
}

// TestSessionZeroValueIsStrict Session 零值必须表现为最严格的 v2。
//
// 这条是整个改动的安全兜底：任何漏传版本的代码路径，后果是"连不上/解不开"，
// 而不是"静默按无认证的 v1 跑"。
func TestSessionZeroValueIsStrict(t *testing.T) {
	var s Session
	if s.Legacy() {
		t.Fatal("零值 Session 不得被判为 v1")
	}
	for name, got := range map[string]bool{
		"RequireSealedResp":       s.RequireSealedResp(),
		"RequireStreamTerminator": s.RequireStreamTerminator(),
		"AntiReplay":              s.AntiReplay(),
	} {
		if !got {
			t.Fatalf("零值 Session 的 %s 必须为真（fail-safe）", name)
		}
	}
	if s.ReqAAD(1, "exec", 0) == nil {
		t.Fatal("零值 Session 必须产出 AAD")
	}
}

// TestLegacySessionDropsAAD v1 会话不能带 AAD —— 0.0.x 的对端是用 nil AAD 加封的，
// 这里多传一个字节就全解不开。
func TestLegacySessionDropsAAD(t *testing.T) {
	s := Session{Key: [32]byte{1}, Version: ToolProtocolVersionV1}
	if s.ReqAAD(1, "exec", 0) != nil || s.RespAAD(1, true, "", "") != nil || s.StreamAAD(1, 0, "stdout", false) != nil {
		t.Fatal("v1 会话的 AAD 必须为 nil")
	}
	if s.AntiReplay() || s.RequireSealedResp() || s.RequireStreamTerminator() {
		t.Fatal("v1 会话不得启用 v2 的强制项")
	}
}

// TestNormalizeMinProto 非法取值必须报错而不是静默回落 —— 这个旗标控制的是一道安全闸，
// 手误被当成默认值处理的话，用户会以为自己开了兼容模式。
func TestNormalizeMinProto(t *testing.T) {
	if v, err := NormalizeMinProto(""); err != nil || v != DefaultMinProto {
		t.Fatalf("空值应回落到默认值，得到 %q err=%v", v, err)
	}
	if v, err := NormalizeMinProto("1"); err != nil || v != "1" {
		t.Fatalf("1 应被接受，得到 %q err=%v", v, err)
	}
	for _, bad := range []string{"0", "3", "v1", "true"} {
		if _, err := NormalizeMinProto(bad); err == nil {
			t.Fatalf("非法取值 %q 必须报错", bad)
		}
	}
}

// TestPunchKeyIsIndependentOfToolVersion 打洞密钥不能跟着工具协议版本走。
//
// 打洞发生在工具通道握手**之前**，那时还没有协商结果可用；早先的实现把
// ToolProtocolVersion 拼进 HKDF info，等于让一个拿不到的值决定密钥。工具协议将来升到
// v3 时，这个耦合会让打洞无故失败。
func TestPunchKeyIsIndependentOfToolVersion(t *testing.T) {
	if strings.Contains(punchKeyInfo, ToolProtocolVersion) && punchKeyInfo != "rat-p2p-punch-v2" {
		t.Fatalf("punchKeyInfo = %q 疑似又与工具协议版本耦合", punchKeyInfo)
	}
	// 固定值，改动即破坏与已发布版本的打洞兼容。
	if punchKeyInfo != "rat-p2p-punch-v2" {
		t.Fatalf("punchKeyInfo = %q，改这个串会让打洞与 1.0.0 不兼容", punchKeyInfo)
	}
}

// TestRejectionReportsPeerBestSupportedVersion 拒绝文案里的"对端版本"必须取对端通告集里
// **本端认识**的最高版本，不能被一个不认识的版本号占住。
//
// 场景：将来某个对端砍掉了 v2、通告 ["3","1"]。本端默认 min-proto=2，正确结论是"谈不成"，
// 但如果 peerBest 被 "3" 占住（rank 为 -1，导致后续比较全部失败），提示就会变成
// "对端只支持 v3，请升级对端"——把该升级的那一端说反了，还掩盖了"本端加 --min-proto=1
// 其实就能连"这个事实。可操作的提示正是 IncompatibleVersionError 存在的理由。
func TestRejectionReportsPeerBestSupportedVersion(t *testing.T) {
	_, err := NegotiateToolVersion("3", []string{"3", "1"}, ToolProtocolVersionV2, SideShare, SideHelp)
	if err == nil {
		t.Fatal("min-proto=2 遇到只支持 v3/v1 的对端应拒绝")
	}
	var incompat *IncompatibleVersionError
	if !errors.As(err, &incompat) {
		t.Fatalf("应返回 IncompatibleVersionError，得到 %T", err)
	}
	if incompat.Got != ToolProtocolVersionV1 {
		t.Fatalf("Got = %q，应取本端认识的最高版本 v1（不是本端不支持的 %q）", incompat.Got, "3")
	}
	// 文案据此给出正确的建议方向。
	if !strings.Contains(incompat.LocalHint(), "--min-proto=1") {
		t.Fatalf("提示应指出放宽到 v1 可行: %s", incompat.LocalHint())
	}
}

// TestInterpretHelloAckDetectsTamperedDowngrade 兼容模式下仍要挡住"两个 v2 端被打回 v1"。
//
// 这是 --min-proto=1 打开后剩下的那条降级路径，也是最隐蔽的一条：两端都是新版、都带
// --min-proto=1（一旦车队里有一个 0.0.x 客户端，这就是现实配置），不可信的 relay 把
// 发起方的 Hello 改写成 {"version":"1"} 并删掉 versions。应答方看到的提议里确实只有 v1，
// 于是**合法地**谈出 v1 并回 Accept:true —— 单看 chosen 和 minProto，发起方没有任何理由
// 拒绝，降级就此完成，而且事后无从察觉。
//
// 但证据其实在手里：应答方的 HelloAck.Versions 明明白白写着它支持 v2。双方都能用更高
// 版本却谈成了低版本，只可能是提议被动过手脚。真正的 0.0.x 不发 Versions，走不到这条
// 判断，兼容性不受影响。
func TestInterpretHelloAckDetectsTamperedDowngrade(t *testing.T) {
	tampered := HelloAck{
		Version:  ToolProtocolVersionV1,                                  // 应答方"合法"选出的结果
		Versions: []string{ToolProtocolVersionV2, ToolProtocolVersionV1}, // 但它自称支持 v2
		Accept:   true,
		NonceB64: NewNonceB64(),
	}
	if _, err := InterpretHelloAck(tampered, ToolProtocolVersionV1, SideHelp, SideShare); err == nil {
		t.Fatal("双方都支持 v2 却谈成 v1，必须判为提议被篡改并拒绝")
	}

	// 真正的 0.0.x：只回 Version，不带 Versions —— 没有证据，照常接受。
	genuine := HelloAck{Version: ToolProtocolVersionV1, Accept: true, NonceB64: NewNonceB64()}
	got, err := InterpretHelloAck(genuine, ToolProtocolVersionV1, SideHelp, SideShare)
	if err != nil {
		t.Fatalf("真实 0.0.x 对端不该被误判: %v", err)
	}
	if got != ToolProtocolVersionV1 {
		t.Fatalf("应接受 v1，得到 %q", got)
	}

	// 应答方选了双方共同的最高版本：正常，放行。
	fine := HelloAck{
		Version:  ToolProtocolVersionV2,
		Versions: []string{ToolProtocolVersionV2, ToolProtocolVersionV1},
		Accept:   true,
		NonceB64: NewNonceB64(),
	}
	if got, err := InterpretHelloAck(fine, ToolProtocolVersionV1, SideHelp, SideShare); err != nil || got != ToolProtocolVersionV2 {
		t.Fatalf("正常协商不该被拦，得到 %q err=%v", got, err)
	}
}

// TestDefaultMinProtoIsLowestAuthenticatedVersion 默认下限要钉在"最低的认证版本"上，
// 而不是"当前最高版本"。
//
// 两者今天恰好相等，所以这条测试现在看着多余——它防的是下一次升版本：加了 v3 之后，
// 若默认值跟着 ToolProtocolVersion 变成 "3"，每个 v3 端就会默认拒绝每个 v2 端，而 v2
// 是一条完整认证的通道（有 AAD、有抗重放），优雅协商正是这套机制存在的意义。
// 要挡的一直只是没有认证的 v1。
func TestDefaultMinProtoIsLowestAuthenticatedVersion(t *testing.T) {
	if DefaultMinProto != ToolProtocolVersionV2 {
		t.Fatalf("DefaultMinProto = %q，应钉在最低认证版本 v2；新增版本时不要让它跟涨", DefaultMinProto)
	}
	// 默认必须拒绝 v1（无认证），这是它存在的理由。
	if _, err := NegotiateToolVersion(ToolProtocolVersionV1, nil, DefaultMinProto, SideShare, SideHelp); err == nil {
		t.Fatal("默认配置必须拒绝 v1")
	}
}
