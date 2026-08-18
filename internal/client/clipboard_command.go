package client

import "fmt"

type clipboardCommand struct {
	name string
	args []string
	env  []string
}

// selectClipboardCommand 只负责平台/工具选择，便于在 Windows CI 上也覆盖 macOS、
// Linux 的 UTF-8 参数，实际执行由 clipboard_other.go 负责。
func selectClipboardCommand(goos string, available func(string) bool) (clipboardCommand, error) {
	if goos == "darwin" {
		return clipboardCommand{name: "pbcopy", env: []string{"LC_CTYPE=UTF-8"}}, nil
	}

	switch {
	case available("wl-copy"):
		return clipboardCommand{name: "wl-copy", args: []string{"--type", "text/plain;charset=utf-8"}}, nil
	case available("xclip"):
		return clipboardCommand{name: "xclip", args: []string{"-selection", "clipboard", "-target", "UTF8_STRING"}}, nil
	case available("xsel"):
		return clipboardCommand{name: "xsel", args: []string{"--clipboard", "--input"}}, nil
	default:
		return clipboardCommand{}, fmt.Errorf("no clipboard tool (install wl-clipboard / xclip / xsel)")
	}
}
