package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
)

func TestLegacyUpdateNotices(t *testing.T) {
	for _, tc := range []struct {
		name                string
		latest, version     string
		helpVersion         string
		shareCap, helpCap   bool
		wantShare, wantHelp bool
	}{
		{"legacy", "v1.1.0", "0.0.7", "0.0.9", false, false, true, true},
		{"legacy_without_version", "v1.1.0", "", "", false, false, true, true},
		{"native", "v1.1.0", "0.0.7", "0.0.9", true, true, false, false},
		{"native_share", "v1.1.0", "0.0.7", "0.0.9", true, false, false, true},
		{"native_help", "v1.1.0", "0.0.7", "0.0.9", false, true, true, false},
		{"new_help_old_share", "v1.1.0", "0.0.7", "1.1.0", false, true, true, false},
		{"published_native_without_capability", "v1.1.0", "1.0.0", "1.0.0", false, false, false, false},
		{"disabled", "", "0.0.7", "0.0.9", false, false, false, false},
		{"current", "v1.1.0", "1.1.0", "1.1.0", false, false, false, false},
		{"newer", "v1.1.0", "1.2.0", "1.2.0", false, false, false, false},
		{"unknown", "v1.1.0", "dev", "dev", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := NewServer(&Config{CodeTTL: time.Minute, Update: proto.UpdateInfo{
				LatestVersion: tc.latest, DownloadURL: update.DefaultDownloadURL, ReleasePage: update.DefaultReleasePage,
			}})
			if err != nil {
				t.Fatal(err)
			}
			shareConn := &reuseProtocolConn{remoteAddr: "203.0.113.7:1"}
			share := &ClientConn{ID: "share", Conn: shareConn}
			const shareHost = "user@share Windows 10 amd64"
			reg, _ := proto.NewMessage(proto.MsgRegisterRequest, &proto.RegisterRequest{
				Version: tc.version, Host: shareHost, SupportsUpdateNotice: tc.shareCap,
			})
			if srv.handleMessage(share, reg) {
				t.Fatal("notice closed share")
			}
			frames := shareConn.messages(t)
			if len(frames) == 0 || frames[0].Type != proto.MsgRegisterResponse {
				t.Fatalf("first frame must remain register_response: %+v", frames)
			}
			var registered proto.RegisterResponse
			if err := proto.DecodePayload(&frames[0], &registered); err != nil {
				t.Fatal(err)
			}
			if len(frames) != 1 {
				t.Fatalf("share frames: %+v", frames)
			}

			helpConn := &reuseProtocolConn{remoteAddr: "198.51.100.9:2"}
			helper := &ClientConn{ID: "help", Conn: helpConn}
			const helpHost = "user@helper uos arm64"
			helpVersion := tc.helpVersion
			join, _ := proto.NewMessage(proto.MsgJoinRequest, &proto.JoinRequest{
				Code: registered.Code, Version: helpVersion, Host: helpHost, SupportsUpdateNotice: tc.helpCap,
			})
			if srv.handleMessage(helper, join) {
				t.Fatal("notice closed help")
			}
			helpFrames := helpConn.messages(t)
			if len(helpFrames) != 1 || helpFrames[0].Type != proto.MsgJoinResponse {
				t.Fatalf("help frames: %+v", helpFrames)
			}
			var joined proto.JoinResponse
			if err := proto.DecodePayload(&helpFrames[0], &joined); err != nil {
				t.Fatal(err)
			}
			if !joined.Success || joined.PeerVersion != tc.version {
				t.Fatalf("join changed: %+v", joined)
			}
			if tc.wantHelp {
				if !strings.HasPrefix(joined.PeerHost, shareHost+"\n") || strings.Count(joined.PeerHost, legacyUpdatePrefix) != 1 {
					t.Fatalf("help must see one local compatibility notice: %s", joined.PeerHost)
				}
				if helpVersion == "" {
					if !strings.Contains(joined.PeerHost, "/releases/tag/v1.1.0") {
						t.Fatalf("unknown help must see release page: %s", joined.PeerHost)
					}
				} else if !strings.Contains(joined.PeerHost, "/v1.1.0/remote-assist-cli-linux-arm64") {
					t.Fatalf("help notice must use the help platform URL: %s", joined.PeerHost)
				}
				if strings.Contains(joined.PeerHost, "/v1.1.0/remote-assist-cli-windows-amd64.exe") {
					t.Fatalf("help notice used the share platform URL: %s", joined.PeerHost)
				}
			} else if joined.PeerHost != shareHost {
				t.Fatalf("host changed without legacy notice: %q", joined.PeerHost)
			}
			if share.Host != shareHost || helper.Host != helpHost {
				t.Fatal("stored identity was modified")
			}
			frames = shareConn.messages(t)
			if frames[len(frames)-1].Type != proto.MsgSessionReady {
				t.Fatalf("pairing stopped: %+v", frames)
			}
			var ready proto.SessionReady
			if err := proto.DecodePayload(&frames[len(frames)-1], &ready); err != nil {
				t.Fatal(err)
			}
			if ready.SessionID != joined.SessionID || ready.PeerVersion != helpVersion {
				t.Fatalf("SessionReady changed: %+v", ready)
			}
			if tc.wantShare {
				if !strings.HasPrefix(ready.PeerHost, helpHost+"\n") || strings.Count(ready.PeerHost, legacyUpdatePrefix) != 1 {
					t.Fatalf("old share must see a visible SessionReady notice: %s", ready.PeerHost)
				}
				if tc.version != "" && !strings.Contains(ready.PeerHost, tc.version) {
					t.Fatalf("share notice must describe the share version: %s", ready.PeerHost)
				}
				if tc.version == "" {
					if !strings.Contains(ready.PeerHost, "/releases/tag/v1.1.0") {
						t.Fatalf("unknown share must see release page: %s", ready.PeerHost)
					}
				} else if !strings.Contains(ready.PeerHost, "/v1.1.0/remote-assist-cli-windows-amd64.exe") {
					t.Fatalf("share notice must use the share platform URL: %s", ready.PeerHost)
				}
				if helpVersion != "" && strings.Contains(ready.PeerHost, "/linux-arm64") {
					t.Fatalf("share notice used the help platform URL: %s", ready.PeerHost)
				}
			} else if ready.PeerHost != helpHost {
				t.Fatalf("SessionReady host changed without legacy notice: %q", ready.PeerHost)
			}
			if shareConn.isClosed() || helpConn.isClosed() || !srv.sessions.IsActiveDataSession(joined.SessionID) {
				t.Fatal("upgrade notice must leave the data plane usable")
			}
		})
	}
}
