package gui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/gui/assets"
)

// connect 结果里的 p2p_path 要带进 GUI 状态：经 relay:3478 的 UDP 中继不是 P2P 直连，
// 「传输」一栏不能写成「P2P 直连」。
func TestConnectMetadataCarriesP2PPath(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"{\"connected\":true,\"p2p\":true,\"p2p_path\":\"udp_relay\"}"}]}`)
	got, err := decodeConnectMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.P2P || got.P2PPath != "udp_relay" {
		t.Fatalf("metadata = %+v, want p2p with path udp_relay", got)
	}
}

func TestTransportLabelDistinguishesUDPRelay(t *testing.T) {
	js := string(assets.IndexHTML)
	if !strings.Contains(js, `r.p2p_path==='udp_relay'?'UDP 中继'`) {
		t.Error("GUI 传输一栏应把 p2p_path=udp_relay 显示为「UDP 中继」，而不是「P2P 直连」")
	}
}
