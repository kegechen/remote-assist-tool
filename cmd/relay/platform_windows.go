//go:build windows

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

const (
	enableQuickEditMode    = 0x0040
	enableExtendedFlags    = 0x0080
	serviceMenuReturnArg   = "--return-menu"
	seeMaskNoCloseProcess  = 0x00000040
	seeMaskNoAsync         = 0x00000100
	elevatedCommandTimeout = 2 * time.Minute
)

var shellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     unsafe.Pointer
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

func dispatchPlatform(args []string, stdin io.Reader, stdout, stderr io.Writer) (bool, int) {
	isService, err := svc.IsWindowsService()
	if err != nil {
		fmt.Fprintf(stderr, "检测 Windows 服务会话失败: %v\n", err)
		return true, 1
	}
	if isService {
		if err := runWindowsService(); err != nil {
			fmt.Fprintf(stderr, "运行 Windows 服务失败: %v\n", err)
			return true, 1
		}
		return true, 0
	}

	prepareInteractiveConsole()
	if len(args) == 0 {
		return true, runWindowsServiceMenu(stdin, stdout, stderr)
	}
	if args[0] != "service" {
		return false, 0
	}
	return true, runWindowsServiceCommand(args[1:], stdin, stdout, stderr)
}

func prepareInteractiveConsole() {
	handle, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil || handle == 0 || handle == windows.InvalidHandle {
		return
	}
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return
	}
	mode = (mode | enableExtendedFlags) &^ enableQuickEditMode
	_ = windows.SetConsoleMode(handle, mode)
}

func runWindowsServiceCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cleanArgs, returnMenu := stripInternalServiceArgs(args)
	if len(cleanArgs) == 0 {
		printWindowsServiceUsage(stderr)
		return 2
	}
	command := cleanArgs[0]
	commandArgs := cleanArgs[1:]
	var installConfigSource string
	if command == "install" {
		parsedConfigSource, parseErr := parseWindowsServiceInstallArgs(commandArgs, stderr)
		if parseErr != nil {
			fmt.Fprintf(stderr, "service install: %v\n", parseErr)
			return 2
		}
		installConfigSource = parsedConfigSource
		cleanArgs = []string{"install"}
		if installConfigSource != "" {
			cleanArgs = append(cleanArgs, "--config", installConfigSource)
		}
		commandArgs = nil
	}
	mutation := command == "install" || command == "start" || command == "stop" || command == "uninstall"
	if mutation && !windows.GetCurrentProcessToken().IsElevated() {
		relaunchArgs := append([]string{"service"}, cleanArgs...)
		if returnMenu {
			relaunchArgs = append(relaunchArgs, serviceMenuReturnArg)
		}
		exitCode, err := relaunchWindowsElevated(relaunchArgs, elevatedCommandTimeout)
		if err != nil {
			fmt.Fprintf(stderr, "请求管理员权限失败: %v\n", err)
			return 1
		}
		return exitCode
	}

	var err error
	switch command {
	case "install":
		err = installWindowsService(installConfigSource)
		if err == nil {
			paths, _ := defaultWindowsServicePaths()
			fmt.Fprintf(stdout, "服务安装完成。\n程序: %s\n配置: %s\n", paths.executable, paths.configFile)
		}
	case "start":
		err = requireNoServiceArgs(commandArgs, startWindowsService)
		if err == nil {
			fmt.Fprintln(stdout, "服务已启动。")
		}
	case "status":
		if len(commandArgs) != 0 {
			err = fmt.Errorf("status does not accept arguments")
		} else {
			var snapshot windowsServiceSnapshot
			snapshot, err = queryWindowsService()
			if err == nil {
				printWindowsServiceStatus(stdout, snapshot)
			}
		}
	case "stop":
		err = requireNoServiceArgs(commandArgs, stopWindowsService)
		if err == nil {
			fmt.Fprintln(stdout, "服务已停止。")
		}
	case "uninstall":
		err = requireNoServiceArgs(commandArgs, uninstallWindowsService)
		if err == nil {
			paths, _ := defaultWindowsServicePaths()
			fmt.Fprintf(stdout, "服务注册已删除。程序、配置和审计日志已保留：\n%s\n%s\n", paths.installDir, paths.dataDir)
		}
	case "run":
		err = errors.New("service run 只能由 Windows 服务控制管理器调用")
	default:
		printWindowsServiceUsage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "service %s: %v\n", command, err)
		return 1
	}
	if returnMenu {
		return runWindowsServiceMenu(stdin, stdout, stderr)
	}
	return 0
}

func parseWindowsServiceInstallArgs(args []string, output io.Writer) (string, error) {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	fs.SetOutput(output)
	configSource := fs.String("config", "", "import service config JSON")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *configSource == "" {
		return "", nil
	}
	return filepath.Abs(*configSource)
}

func runWindowsServiceMenu(stdin io.Reader, stdout, stderr io.Writer) int {
	scanner := bufio.NewScanner(stdin)
	for {
		snapshot, err := queryWindowsService()
		if err != nil {
			fmt.Fprintf(stderr, "查询服务状态失败: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "\nRemote Assist Relay")
		printWindowsServiceStatus(stdout, snapshot)
		fmt.Fprintln(stdout)
		if !snapshot.installed {
			fmt.Fprintln(stdout, "1. 安装服务")
		} else if snapshot.state == svc.Stopped {
			fmt.Fprintln(stdout, "2. 启动服务")
		} else if snapshot.state == svc.Running {
			fmt.Fprintln(stdout, "4. 停止服务")
		}
		fmt.Fprintln(stdout, "3. 查看状态")
		if snapshot.installed {
			fmt.Fprintln(stdout, "5. 卸载服务")
		}
		if !snapshot.installed || snapshot.state == svc.Stopped {
			fmt.Fprintln(stdout, "6. 前台运行")
		}
		fmt.Fprintln(stdout, "0. 退出")
		fmt.Fprint(stdout, "请选择: ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				fmt.Fprintf(stderr, "读取输入失败: %v\n", err)
				return 1
			}
			return 0
		}
		choice := strings.TrimSpace(scanner.Text())
		switch choice {
		case "0":
			return 0
		case "1":
			if snapshot.installed {
				fmt.Fprintln(stderr, "服务已经安装。")
				continue
			}
			if exitMenu := runMenuMutation([]string{"install"}, stdin, stdout, stderr); exitMenu {
				return 0
			}
		case "2":
			if !snapshot.installed || snapshot.state != svc.Stopped {
				fmt.Fprintln(stderr, "当前状态不能启动服务。")
				continue
			}
			if exitMenu := runMenuMutation([]string{"start"}, stdin, stdout, stderr); exitMenu {
				return 0
			}
		case "3":
			continue
		case "4":
			if !snapshot.installed || snapshot.state != svc.Running {
				fmt.Fprintln(stderr, "当前状态不能停止服务。")
				continue
			}
			if exitMenu := runMenuMutation([]string{"stop"}, stdin, stdout, stderr); exitMenu {
				return 0
			}
		case "5":
			if !snapshot.installed {
				fmt.Fprintln(stderr, "服务尚未安装。")
				continue
			}
			fmt.Fprint(stdout, "将停止并删除服务注册，程序、配置和日志会保留。确认卸载？[y/N]: ")
			if !scanner.Scan() || !strings.EqualFold(strings.TrimSpace(scanner.Text()), "y") {
				fmt.Fprintln(stdout, "已取消。")
				continue
			}
			if exitMenu := runMenuMutation([]string{"uninstall"}, stdin, stdout, stderr); exitMenu {
				return 0
			}
		case "6":
			if snapshot.installed && snapshot.state != svc.Stopped {
				fmt.Fprintln(stderr, "请先停止服务再前台运行。")
				continue
			}
			args := []string(nil)
			if snapshot.installed {
				paths, pathErr := defaultWindowsServicePaths()
				if pathErr != nil {
					fmt.Fprintf(stderr, "读取服务路径失败: %v\n", pathErr)
					continue
				}
				config, configErr := loadWindowsServiceConfig(paths.configFile, defaultWindowsServiceConfig(paths))
				if configErr != nil {
					fmt.Fprintf(stderr, "读取服务配置失败: %v\n", configErr)
					continue
				}
				args = config.relayArgs()
			}
			return runForegroundRelay(args, stdout, stderr)
		default:
			fmt.Fprintln(stderr, "无效选择。")
		}
	}
}

func runMenuMutation(args []string, stdin io.Reader, stdout, stderr io.Writer) bool {
	if !windows.GetCurrentProcessToken().IsElevated() {
		relaunchArgs := append([]string{"service"}, args...)
		relaunchArgs = append(relaunchArgs, serviceMenuReturnArg)
		_, err := relaunchWindowsElevated(relaunchArgs, 0)
		if err != nil {
			fmt.Fprintf(stderr, "请求管理员权限失败: %v\n", err)
			return false
		}
		return true
	}
	_ = runWindowsServiceCommand(args, stdin, stdout, stderr)
	return false
}

func printWindowsServiceStatus(output io.Writer, snapshot windowsServiceSnapshot) {
	paths, _ := defaultWindowsServicePaths()
	if !snapshot.installed {
		fmt.Fprintln(output, "服务状态: 未安装")
		fmt.Fprintf(output, "计划安装位置: %s\n", paths.executable)
		fmt.Fprintf(output, "计划配置位置: %s\n", paths.configFile)
		return
	}
	fmt.Fprintf(output, "服务状态: 已安装 / %s\n", windowsServiceStateName(snapshot.state))
	if snapshot.processID != 0 {
		fmt.Fprintf(output, "进程 PID: %d\n", snapshot.processID)
	}
	startType := windowsServiceStartTypeName(snapshot.startType)
	if snapshot.delayedAuto && snapshot.startType == windows.SERVICE_AUTO_START {
		startType += "（延迟启动）"
	}
	fmt.Fprintf(output, "启动方式: %s\n", startType)
	fmt.Fprintf(output, "服务账户: %s\n", snapshot.account)
	fmt.Fprintf(output, "服务命令: %s\n", snapshot.binaryPath)
	fmt.Fprintf(output, "配置文件: %s\n", paths.configFile)
	if snapshot.state == svc.Stopped && (snapshot.win32ExitCode != 0 || snapshot.serviceExitCode != 0) {
		fmt.Fprintf(output, "上次退出码: win32=%d service=%d\n", snapshot.win32ExitCode, snapshot.serviceExitCode)
	}
}

func windowsServiceStartTypeName(startType uint32) string {
	switch startType {
	case windows.SERVICE_AUTO_START:
		return "自动"
	case windows.SERVICE_DEMAND_START:
		return "手动"
	case windows.SERVICE_DISABLED:
		return "已禁用"
	default:
		return fmt.Sprintf("未知（%d）", startType)
	}
}

func requireNoServiceArgs(args []string, action func() error) error {
	if len(args) != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(args, " "))
	}
	return action()
}

func stripInternalServiceArgs(args []string) (clean []string, returnMenu bool) {
	for _, arg := range args {
		switch arg {
		case serviceMenuReturnArg:
			returnMenu = true
		default:
			clean = append(clean, arg)
		}
	}
	return clean, returnMenu
}

func relaunchWindowsElevated(args []string, timeout time.Duration) (int, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, err
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = syscall.EscapeArg(arg)
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	file, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return 0, err
	}
	parameters, err := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return 0, err
	}
	directory, err := windows.UTF16PtrFromString(filepath.Dir(executable))
	if err != nil {
		return 0, err
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: parameters,
		lpDirectory:  directory,
		nShow:        windows.SW_SHOWNORMAL,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	result, _, callErr := shellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return 0, callErr
		}
		return 0, errors.New("ShellExecuteExW failed")
	}
	if info.hProcess == 0 {
		return 0, errors.New("elevated process handle is unavailable")
	}
	defer windows.CloseHandle(info.hProcess)
	waitMilliseconds := uint32(windows.INFINITE)
	if timeout > 0 {
		waitMilliseconds = uint32(timeout / time.Millisecond)
	}
	if event, err := windows.WaitForSingleObject(info.hProcess, waitMilliseconds); err != nil {
		return 0, err
	} else if event == uint32(windows.WAIT_TIMEOUT) {
		return 0, fmt.Errorf("timed out after %s waiting for elevated process", timeout)
	} else if event != windows.WAIT_OBJECT_0 {
		return 0, fmt.Errorf("unexpected elevated process wait result: %d", event)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &exitCode); err != nil {
		return 0, err
	}
	return int(exitCode), nil
}

func printWindowsServiceUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage:")
	fmt.Fprintln(output, "  relay.exe service install [--config <config.json>]")
	fmt.Fprintln(output, "  relay.exe service start|status|stop|uninstall")
}
