package client

import (
	"runtime"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
	"github.com/remote-assist/tool/internal/version"
)

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
