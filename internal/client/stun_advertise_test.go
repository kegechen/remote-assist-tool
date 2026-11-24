package client

import (
	"encoding/json"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/p2p"
	"github.com/remote-assist/tool/internal/proto"
)

// help 加入成功后记下 relay 下发的 STUN；后续创建 P2P manager 时作为候选传入。
func TestJoinRemembersRelaySTUN(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	_ = remote.SetDeadline(time.Now().Add(3 * time.Second))
	go func() {
		var req proto.Message
		if err := json.NewDecoder(remote).Decode(&req); err != nil {
			return
		}
		resp, _ := proto.NewMessage(proto.MsgJoinResponse, &proto.JoinResponse{Success: true, SessionID: "s1",
			STUNServers: []string{"23.95.78.14:3478", "stun.chat.bilibili.com:3478"}})
		_ = json.NewEncoder(remote).Encode(resp)
	}()
	h := NewHelpMode(&Config{}, "TESTCODE", "127.0.0.1:0")
	h.client = &Client{conn: local, enc: json.NewEncoder(local), dec: json.NewDecoder(local)}
	if _, err := h.join(); err != nil {
		t.Fatal(err)
	}
	want := []string{"23.95.78.14:3478", "stun.chat.bilibili.com:3478"}
	if got := h.client.relaySTUNServers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relay STUN = %q, want %q", got, want)
	}
}

type stunRecordingManager struct {
	*fakeShareP2PManager
	stun []string
}

func (m *stunRecordingManager) SetSTUNCandidates(s []string) { m.stun = s }

// 自动模式下没给 --stun：STUN（也是 UDP 中继的地址）跟着当前节点走。UDP 中继只认在它那里注册过的会话，
// 用默认节点的 3478 就中继不了换到别的节点上的会话。给了 --stun、显式模式照旧。
func TestShareSTUNFollowsCurrentNode(t *testing.T) {
	for _, tc := range []struct {
		name, stun string
		follow     bool
		want       string
	}{
		{"自动模式", "", true, "relay-w.example.test:3478"},
		{"自动模式给了 --stun", "stun.example.test:3478", true, "stun.example.test:3478"},
		{"显式模式", "", false, ""},
	} {
		c := NewClient(&Config{ServerAddr: "relay-w.example.test:8443", STUNServer: tc.stun})
		got := "<没建 manager>"
		s := &ShareMode{client: c, code: "CODE", stunFollowsRelay: tc.follow,
			newP2PManager: func(_ p2p.P2PMode, stun, _ string) shareP2PManager {
				got = stun
				return newFakeShareP2PManager(nil)
			}}
		s.makeP2PManager(p2p.P2PModeAuto)
		if got != tc.want {
			t.Errorf("%s: STUN = %q，想要 %q", tc.name, got, tc.want)
		}
	}
}

// share 创建 P2P manager 时把 relay 下发的 STUN 注入进去。
func TestShareP2PManagerGetsRelaySTUN(t *testing.T) {
	c := NewClient(&Config{STUNServer: "relay.example:3478"})
	c.setRelaySTUN([]string{"23.95.78.14:3478"})
	var rec *stunRecordingManager
	s := &ShareMode{client: c, code: "CODE", newP2PManager: func(p2p.P2PMode, string, string) shareP2PManager {
		rec = &stunRecordingManager{fakeShareP2PManager: newFakeShareP2PManager(nil)}
		return rec
	}}
	s.makeP2PManager(p2p.P2PModeAuto)
	if rec == nil || !reflect.DeepEqual(rec.stun, []string{"23.95.78.14:3478"}) {
		t.Fatalf("manager STUN candidates = %+v", rec)
	}
}
