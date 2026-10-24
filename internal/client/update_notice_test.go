package client

import (
	"encoding/json"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
	"github.com/remote-assist/tool/internal/version"
)

// Capture actual requests: a client that can render UpdateInfo must opt out of legacy display.
func TestClientsAdvertiseUpdateNoticeSupport(t *testing.T) {
	for _, role := range []string{"share", "help"} {
		t.Run(role, func(t *testing.T) {
			local, remote := net.Pipe()
			defer local.Close()
			defer remote.Close()
			_ = remote.SetDeadline(time.Now().Add(3 * time.Second))
			client := &Client{conn: local, enc: json.NewEncoder(local), dec: json.NewDecoder(local)}
			captured := make(chan proto.Message, 1)
			go func() {
				var request proto.Message
				if err := json.NewDecoder(remote).Decode(&request); err != nil {
					return
				}
				captured <- request
				// Reject before registration UI/clipboard work; only the request capability is under test.
				response, _ := proto.NewMessage(proto.MsgJoinResponse, &proto.JoinResponse{Error: "test complete"})
				if role == "share" {
					response, _ = proto.NewMessage(proto.MsgError, &proto.ErrorMessage{Code: "TEST", Message: "test complete"})
				}
				_ = json.NewEncoder(remote).Encode(response)
			}()
			if role == "share" {
				s := NewShareMode(&Config{}, "127.0.0.1:0", true, agent.SandboxConfig{}, "", "")
				s.client = client
				if err := s.register(); err == nil {
					t.Fatal("expected test rejection")
				}
			} else {
				h := NewHelpMode(&Config{}, "TESTCODE", "127.0.0.1:0")
				h.client = client
				if _, err := h.join(); err == nil {
					t.Fatal("expected test rejection")
				}
			}
			select {
			case request := <-captured:
				var fields map[string]any
				if err := json.Unmarshal(request.Payload, &fields); err != nil {
					t.Fatal(err)
				}
				if fields["supports_update_notice"] != true {
					t.Fatalf("missing native notice capability: %+v", fields)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("request not captured")
			}
		})
	}
}

func TestUpdateNotices(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = "v1.0.1"

	info := proto.UpdateInfo{LatestVersion: "v1.1.0", DownloadURL: update.DefaultDownloadURL, ReleasePage: update.DefaultReleasePage}

	// 本机与对端都落后：本机按 runtime 平台，对端按其身份串平台
	self, peer := updateNotices(&proto.JoinResponse{Success: true, PeerVersion: "0.0.7", PeerHost: "u@pc uos arm64", UpdateInfo: info})
	if self == nil || !strings.Contains(self.URL, "remote-assist-cli-"+runtime.GOOS+"-"+runtime.GOARCH) {
		t.Errorf("self notice: %+v", self)
	}
	if peer == nil || !strings.HasSuffix(peer.URL, "remote-assist-cli-linux-arm64") || peer.Current != "0.0.7" {
		t.Errorf("peer notice: %+v", peer)
	}

	// 旧 relay 不下发：都不提示
	self, peer = updateNotices(&proto.JoinResponse{Success: true, PeerVersion: "0.0.7"})
	if self != nil || peer != nil {
		t.Errorf("no update info should yield no notice: %+v %+v", self, peer)
	}

	// 都已是最新
	version.Version = "v1.1.0"
	self, peer = updateNotices(&proto.JoinResponse{Success: true, PeerVersion: "1.1.0", UpdateInfo: info})
	if self != nil || peer != nil {
		t.Errorf("up to date should yield no notice: %+v %+v", self, peer)
	}
}
