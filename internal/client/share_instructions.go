package client

import (
	"fmt"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
)

const mcpSetupGuideURL = "https://github.com/kegechen/remote-assist-tool/blob/master/MCP_SETUP.md"

// formatShareInstructions 生成可完整粘贴给 AI 编码助手的文本。连接字段保持逐行输出，
// 避免人或 Agent 依赖终端装饰解析。
//
// node 是自动模式下所在的列表节点（显式模式、没有可用列表时为 nil，见 spec 6.7）：传输说明改成
// 「证书已按节点列表校验」，第三方节点在下面加一行运营者。「中转服务」一行的格式不变，MCP 靠它解析。
func formatShareInstructions(code, hostInfo string, cfg *Config, node *directory.Node, expiresAt time.Time) string {
	expiry := "本进程存活期间一直有效（重启 share 后更换）"
	if !expiresAt.IsZero() {
		expiry = expiresAt.Local().Format("2006-01-02 15:04:05")
	}
	relay, operator := relayDesc(cfg), ""
	if node != nil {
		relay = fmt.Sprintf("%s (TLS，证书已按节点列表校验)", cfg.ServerAddr)
		if !node.Official {
			// 运营者来自验过签的节点证书，已排除控制字符和空白。
			operator = fmt.Sprintf("节点运营者: %s（非官方节点）\n", node.Operator)
		}
	}
	return fmt.Sprintf(`请通过 remote-debug MCP 连接并协助排查此设备：
1. 调用 connect 工具，code 和 server 分别取下方“协助码”和“中转服务”括号前的 host:port
2. 连接成功后，核对返回的 peer_host 与“本机标识”一致，再开始操作

协助码: %s
本机标识: %s
中转服务: %s
%s有效期至: %s
（若尚未配置 remote-debug MCP，请先参考：%s，按说明自动下载安装 remote 并配置 MCP，然后再连接。）`,
		formatCode(code),
		hostInfo,
		relay,
		operator,
		expiry,
		mcpSetupGuideURL,
	)
}

// updateLinks 是升级提示用的下载链接模板和发布页（spec 6.6）。第三方节点（official=false）下发的不用，
// 换成内置的官方地址，免得用户被引去下载别人打包的程序；官方节点和显式模式（node 为 nil）照用 relay 下发的。
func updateLinks(node *directory.Node, info proto.UpdateInfo) (downloadURL, releasePage string) {
	if node != nil && !node.Official {
		return update.DefaultDownloadURL, update.DefaultReleasePage
	}
	return info.DownloadURL, info.ReleasePage
}
