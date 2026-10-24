package relay

import (
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
)

const legacyUpdatePrefix = "[升级提示] "

// legacyShareSessionReadyHost appends the compatibility notice to the field
// that old share clients already print after a helper joins. Sending a made-up
// message type is ignored by old clients (and only logs to stderr), so the
// notice must travel in the established SessionReady payload instead.
func (s *Server) legacyShareSessionReadyHost(share *ClientConn, peerVersion, peerHost string) string {
	if share == nil || !needsLegacyUpdateNotice(share.Version, share.supportsUpdateNotice) {
		return peerHost
	}
	display := peerHost
	info := s.updateInfo()
	if notice, ok := legacyNotice(share.Version, share.Host, info); ok {
		display += "\n" + legacyUpdatePrefix + notice.String()
	}
	return display
}

// legacyHelpUpdateHost 为没有提示能力的 help 在已有显示字段后附加说明。0.0.7 的
// CLI 会打印 PeerHost，MCP 还会把它放进 connect 结果；未知消息在 help 里会被忽略。
// 只修改发给这个旧 help 的响应副本，不改真实主机/版本、会话快照或看板数据。
// 新客户端保留原始 PeerHost，自行展示 UpdateInfo 和结构化 update_notice。
func (s *Server) legacyHelpUpdateHost(client *ClientConn, peerVersion, peerHost string) string {
	if client == nil || !needsLegacyUpdateNotice(client.Version, client.supportsUpdateNotice) {
		return peerHost
	}
	info := s.updateInfo()
	display := peerHost
	if notice, ok := legacyNotice(client.Version, client.Host, info); ok {
		display += "\n" + legacyUpdatePrefix + "协助端本机：" + notice.String()
	}
	return display
}

// needsLegacyUpdateNotice limits the compatibility path to clients that
// predate structured UpdateInfo. The version floor protects released 1.x
// clients that do not send the newly added capability bit.
func needsLegacyUpdateNotice(version string, supportsUpdateNotice bool) bool {
	if supportsUpdateNotice {
		return false
	}
	if version == "" {
		return true
	}
	n, ok := update.ParseVersion(version)
	return ok && n[0] == 0
}

func legacyNotice(version, host string, info proto.UpdateInfo) (update.Notice, bool) {
	if version != "" {
		return update.CheckPeer(version, host, info.LatestVersion, info.DownloadURL, info.ReleasePage)
	}
	return update.CheckUnknown(info.LatestVersion, info.ReleasePage)
}
