package client

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
	"github.com/remote-assist/tool/internal/update"
)

const mcpSetupGuideURL = "https://github.com/kegechen/remote-assist-tool/blob/master/MCP_SETUP.md"

// formatShareInstructions 生成可完整粘贴给 AI 编码助手的文本。连接字段保持逐行输出，
// 避免人或 Agent 依赖终端装饰解析。
//
// node 是自动模式下所在的列表节点（显式模式、没有可用列表时为 nil，见 spec 6.7）：地址写列表里的（listedAddr），
// 传输说明改成「证书已按节点列表校验」，第三方节点在下面加一行运营者。「中转服务」一行的格式不变，MCP 靠它解析。
func formatShareInstructions(code, hostInfo string, cfg *Config, node *directory.Node, expiresAt time.Time) string {
	expiry := "本进程存活期间一直有效（重启 share 后更换）"
	if !expiresAt.IsZero() {
		expiry = expiresAt.Local().Format("2006-01-02 15:04:05")
	}
	relay, operator := relayDesc(cfg), ""
	if node != nil {
		relay = relayDescOnNode(listedAddr(node, cfg.ServerAddr))
		if op := operatorNote(node); op != "" {
			operator = op + "\n"
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

// listedAddr 是给协助端用的节点 n 的地址：实际连的 actual 是列表里的地址（TCP 或 QUIC，不分大小写）时照写列表里的
// 写法；不是时（本机专用地址：内网、Tailscale 地址，协助端多半连不上，spec 6.5；或 --relay-hint 给的不在列表里的地址）
// 写列表里的第一个地址，TCP 优先（验过签的节点至少有一个 TCP 地址，实际就是第一个 TCP 地址）。
func listedAddr(n *directory.Node, actual string) string {
	if a, ok := listSpelling(n, actual); ok {
		return a
	}
	if all := slices.Concat(n.TCP, n.QUIC); len(all) > 0 {
		return all[0]
	}
	return actual // 验过签的节点至少有一个地址，到不了这里
}

// listSpelling 返回 addr 在节点 n 的列表地址（TCP、QUIC）里的写法（不分大小写）；addr 不是列表地址时 ok 为 false。
func listSpelling(n *directory.Node, addr string) (spelling string, ok bool) {
	for _, a := range slices.Concat(n.TCP, n.QUIC) {
		if strings.EqualFold(a, addr) {
			return a, true
		}
	}
	return "", false
}

// relayDescOnNode 是连在列表节点上时中转服务的写法（spec 6.7）：地址加「证书已按节点列表校验」。
func relayDescOnNode(addr string) string {
	return fmt.Sprintf("%s (TLS，证书已按节点列表校验)", addr)
}

// operatorNote 是第三方节点（official=false）的运营者说明，官方节点为空串（spec 6.7）。
func operatorNote(n *directory.Node) string {
	if n.Official {
		return ""
	}
	// 运营者来自验过签的节点证书，已排除控制字符和空白。
	return fmt.Sprintf("节点运营者: %s（非官方节点）", n.Operator)
}

// updateLinks 是升级提示用的下载链接模板和发布页（spec 6.6）。第三方节点（official=false）下发的不用，
// 换成内置的官方地址，免得用户被引去下载别人打包的程序；官方节点和显式模式（node 为 nil）照用 relay 下发的。
func updateLinks(node *directory.Node, info proto.UpdateInfo) (downloadURL, releasePage string) {
	if node != nil && !node.Official {
		return update.DefaultDownloadURL, update.DefaultReleasePage
	}
	return info.DownloadURL, info.ReleasePage
}
