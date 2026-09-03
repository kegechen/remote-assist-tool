//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func runUpgradeCommand(name string, args []string) error {
	switch name {
	case "upgrade-stage":
		return runUpgradeStage(args)
	case "upgrade-finalize":
		return runUpgradeFinalize(args)
	default:
		return fmt.Errorf("unknown command %q", name)
	}
}

func runUpgradeStage(args []string) error {
	fs := flag.NewFlagSet("upgrade-stage", flag.ContinueOnError)
	oldPID := fs.Int("old-pid", 0, "old share PID")
	home := fs.String("home", "", "isolated user profile")
	codeFile := fs.String("code-file", "", "new share code file")
	pidFile := fs.String("pid-file", "", "new share PID file")
	logFile := fs.String("log-file", "", "new share log file")
	server := fs.String("server", "", "effective relay server")
	target := fs.String("target", "", "installed executable path")
	backup := fs.String("backup", "", "old executable backup path")
	cwd := fs.String("cwd", "", "old share working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *oldPID <= 1 || *home == "" || *codeFile == "" || *pidFile == "" || *logFile == "" || *target == "" || *backup == "" {
		return errors.New("old-pid, home, code-file, pid-file, log-file, target and backup are required")
	}
	oldArgv := fs.Args()
	if len(oldArgv) == 0 {
		return errors.New("old share argv is required after --")
	}
	shareArgs, err := upgradedShareArgs(oldArgv, *server, *codeFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*home, 0700); err != nil {
		return fmt.Errorf("create isolated user profile: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(*codeFile), 0700); err != nil {
		return fmt.Errorf("create upgrade directory: %w", err)
	}
	if _, err := os.Stat(*backup); err == nil {
		return fmt.Errorf("backup path already exists: %s", *backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check backup path: %w", err)
	}

	source, err := os.Executable()
	if err != nil {
		return err
	}
	adjacent, err := copyExecutableBeside(source, *target)
	if err != nil {
		return err
	}
	keepAdjacent := false
	defer func() {
		if !keepAdjacent {
			_ = os.Remove(adjacent)
		}
	}()

	if err := os.Rename(*target, *backup); err != nil {
		return fmt.Errorf("rename running old executable to backup: %w", err)
	}
	restoreOld := true
	defer func() {
		if restoreOld {
			_ = os.Rename(*backup, *target)
		}
	}()
	if err := os.Rename(adjacent, *target); err != nil {
		return fmt.Errorf("place candidate at original path: %w", err)
	}
	keepAdjacent = true

	logOut, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		if rollbackErr := rollbackInstalledCandidate(*target, *backup); rollbackErr != nil {
			return fmt.Errorf("open log: %v; restore old executable: %w", err, rollbackErr)
		}
		restoreOld = false
		return fmt.Errorf("open log: %w", err)
	}
	cmd := exec.Command(*target, shareArgs...)
	// 先存原值再改写：隔离只服务于握手期的 ClientID 独立，继任者抢到实例锁后会还原。
	stagedEnv := stashOrigHomeEnv(os.Environ(), "HOME", "USERPROFILE")
	cmd.Env = replaceEnv(replaceEnv(stagedEnv, "HOME", *home), "USERPROFILE", *home)
	if *cwd != "" {
		cmd.Dir = *cwd
	}
	cmd.Stdin = nil
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		logOut.Close()
		if rollbackErr := rollbackInstalledCandidate(*target, *backup); rollbackErr != nil {
			return fmt.Errorf("start new share: %v; restore old executable: %w", err, rollbackErr)
		}
		restoreOld = false
		return fmt.Errorf("start new share: %w", err)
	}
	if err := writeFileAtomic(*pidFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0600); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logOut.Close()
		if rollbackErr := rollbackInstalledCandidate(*target, *backup); rollbackErr != nil {
			return fmt.Errorf("write pid file: %v; restore old executable: %w", err, rollbackErr)
		}
		restoreOld = false
		return fmt.Errorf("write pid file: %w", err)
	}
	_ = cmd.Process.Release()
	_ = logOut.Close()
	restoreOld = false
	return nil
}

func runUpgradeFinalize(args []string) error {
	fs := flag.NewFlagSet("upgrade-finalize", flag.ContinueOnError)
	action := fs.String("action", "", "commit or rollback")
	oldPID := fs.Int("old-pid", 0, "old share PID")
	newPID := fs.Int("new-pid", 0, "new share PID")
	pidFile := fs.String("pid-file", "", "new share PID file")
	target := fs.String("target", "", "installed executable path")
	backup := fs.String("backup", "", "old executable backup path")
	failed := fs.String("failed", "", "failed candidate path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *action {
	case "commit":
		if *oldPID <= 1 || *backup == "" {
			return errors.New("old-pid and backup are required for commit")
		}
		if err := terminateProcessAndWait(*oldPID); err != nil {
			return fmt.Errorf("terminate old share: %w", err)
		}
		if err := removeWithRetry(*backup, 5*time.Second); err != nil {
			return fmt.Errorf("remove old executable backup: %w", err)
		}
		return nil
	case "rollback":
		if *target == "" || *backup == "" || *failed == "" {
			return errors.New("target, backup and failed are required for rollback")
		}
		pid := *newPID
		if pid <= 1 && *pidFile != "" {
			if raw, err := os.ReadFile(*pidFile); err == nil {
				pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			}
		}
		if pid > 1 && pid != *oldPID {
			_ = terminateProcessAndWait(pid)
		}
		if _, err := os.Stat(*backup); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("check old executable backup: %w", err)
		}
		_ = os.Remove(*failed)
		if err := os.Rename(*target, *failed); err != nil {
			return fmt.Errorf("move failed candidate aside: %w", err)
		}
		if err := os.Rename(*backup, *target); err != nil {
			_ = os.Rename(*failed, *target)
			return fmt.Errorf("restore old executable: %w", err)
		}
		_ = os.Remove(*failed)
		return nil
	default:
		return errors.New("action must be commit or rollback")
	}
}

func copyExecutableBeside(source, target string) (string, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open candidate: %w", err)
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(target), ".remote-assist-new-*.exe")
	if err != nil {
		return "", fmt.Errorf("create candidate beside target: %w", err)
	}
	path := out.Name()
	ok := false
	defer func() {
		out.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return "", fmt.Errorf("copy candidate beside target: %w", err)
	}
	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("sync candidate: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

func rollbackInstalledCandidate(target, backup string) error {
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(backup, target)
}

func terminateProcessAndWait(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	if status, waitErr := windows.WaitForSingleObject(handle, 0); waitErr == nil && status != uint32(windows.WAIT_TIMEOUT) {
		return nil
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return err
	}
	status, err := windows.WaitForSingleObject(handle, 10_000)
	if err != nil {
		return err
	}
	if status == uint32(windows.WAIT_TIMEOUT) {
		return errors.New("timed out waiting for process exit")
	}
	return nil
}

func removeWithRetry(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func replaceEnv(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(strings.ToUpper(item), prefix) {
			out = append(out, item)
		}
	}
	return append(out, key+"="+value)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upgrade-write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
