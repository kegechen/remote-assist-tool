package client

import (
	"fmt"
	"time"
)

const mcpSetupGuideURL = "https://github.com/kegechen/remote-assist-tool/blob/master/MCP_SETUP.md"

// formatShareInstructions 生成可完整粘贴给 AI 编码助手的文本。连接字段保持逐行输出，
// 避免人或 Agent 依赖终端装饰解析。
func formatShareInstructions(code, hostInfo string, cfg *Config, expiresAt time.Time) string {
	return fmt.Sprintf(`请通过 remote-debug MCP 连接并协助排查此设备：
1. 调用 connect 工具，code 和 server 分别取下方“协助码”和“中转服务”括号前的 host:port
2. 连接成功后，核对返回的 peer_host 与“本机标识”一致，再开始操作

协助码: %s
本机标识: %s
中转服务: %s
有效期至: %s
（若尚未配置 remote-debug MCP，请先参考：%s，按说明自动下载安装 remote 并配置 MCP，然后再连接。）`,
		formatCode(code),
		hostInfo,
		relayDesc(cfg),
		expiresAt.Local().Format("2006-01-02 15:04:05"),
		mcpSetupGuideURL,
	)
}
