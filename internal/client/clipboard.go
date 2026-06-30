package client

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// copyToClipboard 把文本写入系统剪贴板（跨平台，尽力而为）。
//   - Windows: clip
//   - macOS:   pbcopy
//   - Linux:   wl-copy / xclip / xsel（任一存在即用；都没有则返回错误）
//
// 失败返回 error 由调用方决定是否提示；协助码复制是锦上添花，不应阻断协助流程。
func copyToClipboard(text string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("clip")
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		switch {
		case hasBin("wl-copy"):
			cmd = exec.Command("wl-copy")
		case hasBin("xclip"):
			cmd = exec.Command("xclip", "-selection", "clipboard")
		case hasBin("xsel"):
			cmd = exec.Command("xsel", "--clipboard", "--input")
		default:
			return fmt.Errorf("no clipboard tool (install wl-clipboard / xclip / xsel)")
		}
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
