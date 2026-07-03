//go:build windows

package sysinfo

import (
	"fmt"
	"syscall"
	"unsafe"
)

// rtlOSVersionInfo 对应 ntdll RTL_OSVERSIONINFOW（截取到 BuildNumber 之后的
// 定长字段即可，CSDVersion 数组保留以满足结构体大小校验）。
type rtlOSVersionInfo struct {
	osVersionInfoSize uint32
	majorVersion      uint32
	minorVersion      uint32
	buildNumber       uint32
	platformID        uint32
	csdVersion        [128]uint16
}

// osName 用 ntdll!RtlGetVersion 取真实版本（GetVersionEx 会被兼容性遮蔽）。
// Windows 11 与 10 的 MajorVersion 都是 10，以 build >= 22000 区分。
// Server 版本 build 号体系不同（如 Server 2022 = 20348），会显示为
// "Windows 10"，展示用途可接受。
func osName() string {
	var info rtlOSVersionInfo
	info.osVersionInfoSize = uint32(unsafe.Sizeof(info))
	proc := syscall.NewLazyDLL("ntdll.dll").NewProc("RtlGetVersion")
	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&info)))
	if ret != 0 { // STATUS_SUCCESS == 0
		return "Windows"
	}
	switch {
	case info.majorVersion == 10 && info.buildNumber >= 22000:
		return "Windows 11"
	case info.majorVersion == 10:
		return "Windows 10"
	default:
		return fmt.Sprintf("Windows %d", info.majorVersion)
	}
}
