package client

// copyToClipboard 把文本写入系统剪贴板（跨平台，尽力而为）。
//   - Windows: Win32 CF_UNICODETEXT，不经过控制台代码页
//   - macOS:   pbcopy（stdin UTF-8）
//   - Linux:   wl-copy / xclip / xsel（stdin UTF-8，任一存在即用）
//
// 失败返回 error 由调用方决定是否提示；协助码复制是锦上添花，不应阻断协助流程。
func copyToClipboard(text string) error {
	return copyToClipboardPlatform(text)
}
