//go:build windows

package tools

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// descendantsOf 用 Toolhelp 快照计算 root 的全部后代进程号。
func descendantsOf(root int) []int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	parents := map[int]int{}
	if windows.Process32First(snap, &e) != nil {
		return nil
	}
	for {
		parents[int(e.ProcessID)] = int(e.ParentProcessID)
		if windows.Process32Next(snap, &e) != nil {
			break
		}
	}
	set := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for pid, pp := range parents {
			if pid != root && !set[pid] && (pp == root || set[pp]) {
				set[pid] = true
				changed = true
			}
		}
	}
	var out []int
	for pid := range set {
		out = append(out, pid)
	}
	return out
}

func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

// treeTestCommand 在 shell 里启动一个长时间运行的子进程。
func treeTestCommand() (shell string, input string) {
	return "cmd.exe", "ping -n 300 127.0.0.1 >nul\r"
}
