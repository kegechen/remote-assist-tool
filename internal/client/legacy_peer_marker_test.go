package client

import (
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// 只有「对端明确只支持 v1、本端要求更高」才带标记；其它版本不兼容、对端回传文本都不带。
func TestLegacyPeerMarker(t *testing.T) {
	legacy := &proto.IncompatibleVersionError{LocalSide: proto.SideHelp, PeerSide: proto.SideShare, Want: "2", Got: "1"}
	hint := mcpIncompatHint(legacy)
	if !strings.HasPrefix(hint, LegacyPeerMarker) {
		t.Fatalf("旧版对端的指引应以标记开头: %s", hint)
	}
	if !IsLegacyPeerError("handshake failed: " + hint) {
		t.Fatal("应识别为旧版")
	}
	// 本端已是最低下限：没有可降，不是降级场景
	if IsLegacyPeerError("handshake failed: " + mcpIncompatHint(&proto.IncompatibleVersionError{Want: "1", Got: "1"})) {
		t.Fatal("Want=1 不应识别为可降级")
	}
	// 对端未知版本
	if IsLegacyPeerError("handshake failed: " + mcpIncompatHint(&proto.IncompatibleVersionError{Want: "2", Got: ""})) {
		t.Fatal("未知版本不应识别为旧版")
	}
	// 对端回传的原文里夹带标记不算
	for _, m := range []string{
		"handshake failed: 对端拒绝工具通道握手：" + LegacyPeerMarker,
		"join failed: " + LegacyPeerMarker,
		"",
	} {
		if IsLegacyPeerError(m) {
			t.Fatalf("不应识别: %q", m)
		}
	}
}
