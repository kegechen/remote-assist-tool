//go:build windows

package logfile

import (
	"errors"
	"os"
	"os/user"

	"golang.org/x/sys/windows"
)

// userTag 是 auto 目录名里区分用户的部分：Windows 没有 uid，用用户名（spec 6.8）。
func userTag() string {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if name == "" {
		name = os.Getenv("USERNAME")
	}
	return cleanUserName(name)
}

// fileOwner 在 Windows 上查不了属主：临时目录本来就是每个用户自己的，见 checkDir。
func fileOwner(os.FileInfo) (owned, known bool) { return false, false }

// processAlive 报告 pid 的进程是否还在运行。打不开但被拒绝访问（别的用户、受保护的进程）也算在运行。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}
