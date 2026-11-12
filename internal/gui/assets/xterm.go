package assets

import "embed"

// StaticFS 离线内嵌的前端静态资源，由 gui 路由 /assets/ 本地提供，不依赖外网 CDN：
//   - xterm/：@xterm/xterm 5.5.0 与 @xterm/addon-fit 0.10.0（MIT，许可证文件随包保留）
//   - pty/：真终端页脚本与样式（pty_term.js / pty_term.css）
//
//go:embed xterm pty
var StaticFS embed.FS
