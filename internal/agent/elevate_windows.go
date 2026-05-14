//go:build windows

package agent

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	shell32        = syscall.NewLazyDLL("shell32.dll")
	procShellExecW = shell32.NewProc("ShellExecuteW")
)

// RelaunchElevated 用 runas 重新启动自身，传入 --elevated-child 旗标避免无限递归
func RelaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := append([]string{}, os.Args[1:]...)
	args = append(args, "--elevated-child")
	param := strings.Join(args, " ")

	verb, _ := syscall.UTF16PtrFromString("runas")
	exeP, _ := syscall.UTF16PtrFromString(exe)
	paramP, _ := syscall.UTF16PtrFromString(param)

	ret, _, callErr := procShellExecW.Call(
		0, uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(exeP)),
		uintptr(unsafe.Pointer(paramP)),
		0, 1, // SW_SHOWNORMAL
	)
	if ret <= 32 {
		return fmt.Errorf("ShellExecuteW failed: ret=%d err=%v", ret, callErr)
	}
	os.Exit(0)
	return nil
}

// currentProcess 返回当前进程伪句柄（Windows 约定: -1）
var kernel32           = syscall.NewLazyDLL("kernel32.dll")
var procGetCurrentProc = kernel32.NewProc("GetCurrentProcess")

func currentProcessHandle() syscall.Handle {
	h, _, _ := procGetCurrentProc.Call()
	return syscall.Handle(h)
}

// IsElevated 检查当前进程 token 是否 elevated
func IsElevated() bool {
	var token syscall.Token
	if err := syscall.OpenProcessToken(currentProcessHandle(), syscall.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	var elevation uint32
	var sz uint32
	if err := syscall.GetTokenInformation(token, 20 /*TokenElevation*/, (*byte)(unsafe.Pointer(&elevation)), uint32(unsafe.Sizeof(elevation)), &sz); err != nil {
		return false
	}
	return elevation != 0
}
