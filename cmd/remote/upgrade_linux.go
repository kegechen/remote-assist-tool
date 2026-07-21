//go:build linux

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
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
	home := fs.String("home", "", "isolated HOME")
	codeFile := fs.String("code-file", "", "new share code file")
	pidFile := fs.String("pid-file", "", "new share PID file")
	logFile := fs.String("log-file", "", "new share log file")
	server := fs.String("server", "", "effective relay server")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *oldPID <= 1 || *home == "" || *codeFile == "" || *pidFile == "" || *logFile == "" {
		return errors.New("old-pid, home, code-file, pid-file and log-file are required")
	}

	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(*oldPID), "cmdline"))
	if err != nil {
		return fmt.Errorf("read old command line: %w", err)
	}
	parts := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	oldArgv := make([]string, 0, len(parts))
	for _, part := range parts {
		oldArgv = append(oldArgv, string(part))
	}
	shareArgs, err := upgradedShareArgs(oldArgv, *server, *codeFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*home, 0700); err != nil {
		return fmt.Errorf("create isolated HOME: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(*codeFile), 0700); err != nil {
		return fmt.Errorf("create upgrade directory: %w", err)
	}
	logOut, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		logOut.Close()
		return err
	}
	cmd := exec.Command(exe, shareArgs...)
	cmd.Env = replaceEnv(os.Environ(), "HOME", *home)
	cmd.Stdin = nil
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logOut.Close()
		return fmt.Errorf("start new share: %w", err)
	}
	pid := cmd.Process.Pid
	if err := writeFileAtomic(*pidFile, []byte(strconv.Itoa(pid)+"\n"), 0600); err != nil {
		_ = cmd.Process.Kill()
		logOut.Close()
		return fmt.Errorf("write pid file: %w", err)
	}
	_ = cmd.Process.Release()
	return logOut.Close()
}

func runUpgradeFinalize(args []string) error {
	fs := flag.NewFlagSet("upgrade-finalize", flag.ContinueOnError)
	source := fs.String("source", "", "uploaded binary")
	target := fs.String("target", "", "installed binary path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" || *target == "" {
		return errors.New("source and target are required")
	}
	return replaceExecutable(*source, *target)
}

func replaceExecutable(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	mode := os.FileMode(0755)
	if st, err := os.Stat(target); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".remote-assist-replace-*")
	if err != nil {
		return fmt.Errorf("create replacement beside target: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		tmp.Close()
		if cleanup {
			os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return fmt.Errorf("copy replacement: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("preserve executable mode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync replacement: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("atomic replace %q: %w", target, err)
	}
	cleanup = false
	// Linux 允许删除仍在运行的映像；new share 会继续运行，下一次标准启动走 target。
	if filepath.Clean(source) != filepath.Clean(target) {
		_ = os.Remove(source)
	}
	return nil
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if len(item) >= len(prefix) && item[:len(prefix)] == prefix {
			continue
		}
		out = append(out, item)
	}
	return append(out, prefix+value)
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
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
