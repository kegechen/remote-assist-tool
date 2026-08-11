//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const windowsServiceDataSDDL = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;LS)"

type windowsServiceSnapshot struct {
	installed       bool
	state           svc.State
	processID       uint32
	win32ExitCode   uint32
	serviceExitCode uint32
	binaryPath      string
	account         string
	startType       uint32
	delayedAuto     bool
}

func queryWindowsService() (windowsServiceSnapshot, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return windowsServiceSnapshot{}, fmt.Errorf("open service manager: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	serviceHandle, err := openWindowsService(scm, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return windowsServiceSnapshot{}, nil
	}
	if err != nil {
		return windowsServiceSnapshot{}, fmt.Errorf("open service: %w", err)
	}
	service := &mgr.Service{Name: windowsServiceName, Handle: serviceHandle}
	defer service.Close()

	status, err := service.Query()
	if err != nil {
		return windowsServiceSnapshot{}, fmt.Errorf("query service: %w", err)
	}
	config, err := service.Config()
	if err != nil {
		return windowsServiceSnapshot{}, fmt.Errorf("query service config: %w", err)
	}
	return windowsServiceSnapshot{
		installed:       true,
		state:           status.State,
		processID:       status.ProcessId,
		win32ExitCode:   status.Win32ExitCode,
		serviceExitCode: status.ServiceSpecificExitCode,
		binaryPath:      config.BinaryPathName,
		account:         config.ServiceStartName,
		startType:       config.StartType,
		delayedAuto:     config.DelayedAutoStart,
	}, nil
}

func installWindowsService(configSource string) error {
	snapshot, err := queryWindowsService()
	if err != nil {
		return err
	}
	if snapshot.installed {
		return errors.New("service is already installed")
	}
	paths, err := defaultWindowsServicePaths()
	if err != nil {
		return err
	}
	if configSource != "" {
		configSource, err = filepath.Abs(configSource)
		if err != nil {
			return err
		}
	}
	if err := secureWindowsServiceDataDirectory(paths.dataDir); err != nil {
		return err
	}
	if err := ensureWindowsServiceConfig(paths, configSource); err != nil {
		return fmt.Errorf("prepare service config: %w", err)
	}
	if err := installExecutable(paths.executable); err != nil {
		return err
	}
	if err := ensureWindowsEventSource(); err != nil {
		return fmt.Errorf("register event source: %w", err)
	}

	scmHandle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CREATE_SERVICE)
	if err != nil {
		return fmt.Errorf("open service manager for install: %w", err)
	}
	manager := &mgr.Mgr{Handle: scmHandle}
	defer manager.Disconnect()

	service, err := manager.CreateService(windowsServiceName, paths.executable, mgr.Config{
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: `NT AUTHORITY\LocalService`,
		DisplayName:      windowsServiceDisplayName,
		Description:      windowsServiceDescription,
		DelayedAutoStart: true,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}, "service", "run")
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	keepService := false
	defer func() {
		if !keepService {
			_ = service.Delete()
		}
		_ = service.Close()
	}()

	recovery := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}
	if err := service.SetRecoveryActions(recovery, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("enable recovery for service failures: %w", err)
	}
	keepService = true
	return nil
}

func startWindowsService() error {
	service, closeService, err := openManagedWindowsService(windows.SERVICE_START | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer closeService()
	status, err := service.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Running {
		return nil
	}
	if status.State == svc.StartPending {
		return waitForWindowsServiceState(service, svc.Running, 30*time.Second)
	}
	if status.State != svc.Stopped {
		return fmt.Errorf("cannot start service while state is %s", windowsServiceStateName(status.State))
	}
	if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start service: %w", err)
	}
	return waitForWindowsServiceState(service, svc.Running, 30*time.Second)
}

func stopWindowsService() error {
	service, closeService, err := openManagedWindowsService(windows.SERVICE_STOP | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer closeService()
	return stopManagedWindowsService(service, 30*time.Second)
}

func uninstallWindowsService() error {
	service, closeService, err := openManagedWindowsService(windows.DELETE | windows.SERVICE_STOP | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer closeService()
	if err := stopManagedWindowsService(service, 30*time.Second); err != nil {
		return err
	}
	if err := service.Delete(); err != nil {
		return fmt.Errorf("delete service registration: %w", err)
	}
	return nil
}

func stopManagedWindowsService(service *mgr.Service, timeout time.Duration) error {
	status, err := service.Query()
	if err != nil {
		return err
	}
	switch status.State {
	case svc.Stopped:
		return nil
	case svc.StopPending:
		return waitForWindowsServiceState(service, svc.Stopped, timeout)
	case svc.Running:
		if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("stop service: %w", err)
		}
		return waitForWindowsServiceState(service, svc.Stopped, timeout)
	default:
		return fmt.Errorf("cannot stop service while state is %s", windowsServiceStateName(status.State))
	}
}

func waitForWindowsServiceState(service *mgr.Service, target svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == target {
			return nil
		}
		if target == svc.Running && status.State == svc.Stopped {
			return fmt.Errorf("service stopped during startup (win32_exit=%d service_exit=%d)", status.Win32ExitCode, status.ServiceSpecificExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for service state %s (current: %s)", windowsServiceStateName(target), windowsServiceStateName(status.State))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func openManagedWindowsService(access uint32) (*mgr.Service, func(), error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, fmt.Errorf("open service manager: %w", err)
	}
	serviceHandle, err := openWindowsService(scm, access)
	if err != nil {
		windows.CloseServiceHandle(scm)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, errors.New("service is not installed")
		}
		return nil, nil, err
	}
	service := &mgr.Service{Name: windowsServiceName, Handle: serviceHandle}
	closeFn := func() {
		_ = service.Close()
		_ = windows.CloseServiceHandle(scm)
	}
	return service, closeFn, nil
}

func openWindowsService(scm windows.Handle, access uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(windowsServiceName)
	if err != nil {
		return 0, err
	}
	return windows.OpenService(scm, name, access)
}

func windowsServiceStateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start pending"
	case svc.StopPending:
		return "stop pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue pending"
	case svc.PausePending:
		return "pause pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown (%d)", state)
	}
}

func installExecutable(destination string) error {
	source, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return fmt.Errorf("create install directory: %w", err)
	}
	if sameFile(source, destination) {
		return nil
	}

	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open executable: %w", err)
	}
	defer in.Close()
	temp, err := os.CreateTemp(filepath.Dir(destination), ".relay-*.exe")
	if err != nil {
		return fmt.Errorf("create installed executable: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err = io.Copy(temp, in); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("copy executable: %w", err)
	}
	if err := replaceFileWindows(tempPath, destination); err != nil {
		return fmt.Errorf("install executable: %w", err)
	}
	return nil
}

func sameFile(first, second string) bool {
	firstInfo, err := os.Stat(first)
	if err != nil {
		return false
	}
	secondInfo, err := os.Stat(second)
	return err == nil && os.SameFile(firstInfo, secondInfo)
}

func replaceFileWindows(source, destination string) error {
	sourcePtr, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPtr, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourcePtr, destinationPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func secureWindowsServiceDataDirectory(path string) error {
	securityDescriptor, err := windows.SecurityDescriptorFromString(windowsServiceDataSDDL)
	if err != nil {
		return fmt.Errorf("build service data directory ACL: %w", err)
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil {
		return fmt.Errorf("read service data directory ACL: %w", err)
	}
	owner, _, err := securityDescriptor.Owner()
	if err != nil {
		return fmt.Errorf("read service data directory owner: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create service data parent directory: %w", err)
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	securityAttributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: securityDescriptor,
	}
	if err := windows.CreateDirectory(pathPtr, &securityAttributes); err != nil {
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return fmt.Errorf("create service data directory: %w", err)
		}
		currentDescriptor, descriptorErr := windows.GetNamedSecurityInfo(
			path,
			windows.SE_FILE_OBJECT,
			windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION),
		)
		if descriptorErr != nil {
			return fmt.Errorf("read existing service data directory security: %w", descriptorErr)
		}
		if currentDescriptor.String() != securityDescriptor.String() {
			return fmt.Errorf("service data directory already exists without the required protected ACL: %s", path)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect service data directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("service data path is not a directory: %s", path)
	}

	securityInformation := windows.SECURITY_INFORMATION(
		windows.OWNER_SECURITY_INFORMATION |
			windows.DACL_SECURITY_INFORMATION |
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err := filepath.WalkDir(path, func(currentPath string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		currentPathPtr, err := windows.UTF16PtrFromString(currentPath)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(currentPathPtr)
		if err != nil {
			return fmt.Errorf("read attributes for %s: %w", currentPath, err)
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("service data directory must not contain a reparse point: %s", currentPath)
		}
		if err := windows.SetNamedSecurityInfo(currentPath, windows.SE_FILE_OBJECT, securityInformation, owner, nil, dacl, nil); err != nil {
			return fmt.Errorf("set ACL on %s: %w", currentPath, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("secure service data directory: %w", err)
	}
	return nil
}

func ensureWindowsEventSource() error {
	const base = `SYSTEM\CurrentControlSet\Services\EventLog\Application`
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, base+`\`+windowsEventSource, registry.QUERY_VALUE)
	if err == nil {
		return key.Close()
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	return eventlog.InstallAsEventCreate(windowsEventSource, eventlog.Info|eventlog.Warning|eventlog.Error)
}
