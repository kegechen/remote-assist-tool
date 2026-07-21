//go:build windows

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("REMOTE_ASSIST_TEST_OLD_PROCESS") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("REMOTE_ASSIST_TEST_NEW_PROCESS") == "1" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWindowsUpgradeRenameRollbackAndCommit(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "remote-assist.exe")
	copyFileForUpgradeTest(t, os.Args[0], target)

	old := exec.Command(target)
	old.Env = append(os.Environ(), "REMOTE_ASSIST_TEST_OLD_PROCESS=1")
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	oldWaited := false
	defer func() {
		if !oldWaited {
			_ = old.Process.Kill()
			_, _ = old.Process.Wait()
		}
	}()

	previous, hadPrevious := os.LookupEnv("REMOTE_ASSIST_TEST_NEW_PROCESS")
	if err := os.Setenv("REMOTE_ASSIST_TEST_NEW_PROCESS", "1"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if hadPrevious {
			_ = os.Setenv("REMOTE_ASSIST_TEST_NEW_PROCESS", previous)
		} else {
			_ = os.Unsetenv("REMOTE_ASSIST_TEST_NEW_PROCESS")
		}
	}()

	runStage := func(suffix string) (backup, pidFile string) {
		upgradeDir := filepath.Join(dir, "upgrade-"+suffix)
		backup = target + ".old-" + suffix
		pidFile = filepath.Join(upgradeDir, "new.pid")
		err := runUpgradeStage([]string{
			"--old-pid", strconv.Itoa(old.Process.Pid),
			"--home", filepath.Join(upgradeDir, "home"),
			"--code-file", filepath.Join(upgradeDir, "code.json"),
			"--pid-file", pidFile,
			"--log-file", filepath.Join(upgradeDir, "new.log"),
			"--server", "127.0.0.1:1",
			"--target", target,
			"--backup", backup,
			"--cwd", dir,
			"--", target,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("candidate was not installed at original path: %v", err)
		}
		if _, err := os.Stat(backup); err != nil {
			t.Fatalf("running old executable was not renamed: %v", err)
		}
		return backup, pidFile
	}

	backup, pidFile := runStage("rollback")
	if err := runUpgradeFinalize([]string{
		"--action", "rollback", "--old-pid", strconv.Itoa(old.Process.Pid),
		"--pid-file", pidFile, "--target", target, "--backup", backup,
		"--failed", target + ".failed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("rollback left backup behind: %v", err)
	}

	backup, _ = runStage("commit")
	if err := runUpgradeFinalize([]string{
		"--action", "commit", "--old-pid", strconv.Itoa(old.Process.Pid), "--backup", backup,
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = old.Process.Wait()
	oldWaited = true
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("commit left backup behind: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("commit removed installed candidate: %v", err)
	}
}

func copyFileForUpgradeTest(t *testing.T, source, target string) {
	t.Helper()
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
