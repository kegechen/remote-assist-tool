//go:build windows

package tools

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// ConPTY 的输入管道被卡住（如粘贴了大段内容、对端不读）时，Ctrl+C 字节送不进去。
// Windows 没有“向前台进程组发信号”的 API，唯一的办法是：附着到 shell 所在的伪控制台，
// 对其发 CTRL_C_EVENT。但 AttachConsole 会改变调用进程自己的控制台，share 进程有自己的控制台
// （日志/交互），不能动，所以由一个一次性的隐藏子进程（同一个可执行文件）去完成。

// ptyCtrlCHelperEnv 内部环境变量：值为目标 shell 的 PID。仅由 Interrupt 在拉起助手子进程时设置，
// 包初始化（早于任何 main / TestMain）时检测到即执行助手逻辑并退出。
const ptyCtrlCHelperEnv = "REMOTE_ASSIST_PTY_CTRLC_HELPER"

// ptyInterruptTimeout 助手子进程的最长执行时间。
var ptyInterruptTimeout = 3 * time.Second

func init() {
	v := os.Getenv(ptyCtrlCHelperEnv)
	if v == "" {
		return
	}
	pid, err := strconv.ParseUint(v, 10, 32)
	if err != nil || pid == 0 {
		os.Exit(2)
	}
	if err := sendCtrlCToConsoleOf(uint32(pid)); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// sendCtrlCToConsoleOf 仅在助手子进程中调用：脱离自己的控制台，附着到 pid 的控制台并发 Ctrl+C。
func sendCtrlCToConsoleOf(pid uint32) error {
	k := windows.NewLazySystemDLL("kernel32.dll")
	k.NewProc("FreeConsole").Call()
	if r, _, err := k.NewProc("AttachConsole").Call(uintptr(pid)); r == 0 {
		return err
	}
	// 本进程忽略 Ctrl+C，否则事件广播给控制台上全部进程，助手自己也会被中断。
	if r, _, err := k.NewProc("SetConsoleCtrlHandler").Call(0, 1); r == 0 {
		return err
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
		return err
	}
	// 给事件分发留一点时间，再脱离控制台。
	time.Sleep(100 * time.Millisecond)
	k.NewProc("FreeConsole").Call()
	return nil
}

// Interrupt 实现 ptyInterrupter。只支持 Ctrl+C(0x03)；Ctrl+Z 在 Windows 没有等价信号。
func (p *winPTY) Interrupt(b byte) bool {
	if b != 0x03 {
		return false
	}
	pid := p.Pid()
	if pid <= 0 {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), ptyInterruptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), ptyCtrlCHelperEnv+"="+strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run() == nil
}
