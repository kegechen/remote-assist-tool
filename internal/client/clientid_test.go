package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

func TestShareInstanceLockRejectsSecondOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), shareLockFileName)
	first, err := lockShareFile(path)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer first.Close()

	second, err := lockShareFile(path)
	if second != nil {
		second.Close()
		t.Fatal("second lock unexpectedly succeeded")
	}
	if !errors.Is(err, ErrShareAlreadyRunning) {
		t.Fatalf("second lock error = %v, want ErrShareAlreadyRunning", err)
	}
}

func TestShareInstanceLockCanBeReacquiredAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), shareLockFileName)
	first, err := lockShareFile(path)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}

	second, err := lockShareFile(path)
	if err != nil {
		t.Fatalf("reacquire lock: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("release second lock: %v", err)
	}
}

func TestShareInstanceLockHandoverKeepsThirdOwnerOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), shareLockFileName)
	oldLock, err := lockShareFile(path)
	if err != nil {
		t.Fatalf("acquire old lock: %v", err)
	}

	result, err := beginShareInstanceLockHandover(path)
	if err != nil {
		oldLock.Close()
		t.Fatalf("begin handover: %v", err)
	}

	select {
	case got := <-result:
		if got.Lock != nil {
			got.Lock.Close()
		}
		oldLock.Close()
		t.Fatalf("successor returned before old released the lock: %v", got.Err)
	case <-time.After(100 * time.Millisecond):
	}

	third, err := acquireShareInstanceLock(path)
	if third != nil {
		third.Close()
		oldLock.Close()
		t.Fatal("third owner acquired the lock during handover")
	}
	if !errors.Is(err, ErrShareAlreadyRunning) {
		oldLock.Close()
		t.Fatalf("third owner error = %v, want ErrShareAlreadyRunning", err)
	}

	if err := oldLock.Close(); err != nil {
		t.Fatalf("release old lock: %v", err)
	}
	var successor io.Closer
	select {
	case got := <-result:
		if got.Err != nil {
			t.Fatalf("successor lock: %v", got.Err)
		}
		successor = got.Lock
	case <-time.After(5 * time.Second):
		t.Fatal("successor did not acquire the released lock")
	}
	defer func() {
		if successor != nil {
			successor.Close()
		}
	}()

	third, err = acquireShareInstanceLock(path)
	if third != nil {
		third.Close()
		t.Fatal("third owner acquired the successor's lock")
	}
	if !errors.Is(err, ErrShareAlreadyRunning) {
		t.Fatalf("third owner after handover error = %v, want ErrShareAlreadyRunning", err)
	}
	if err := successor.Close(); err != nil {
		t.Fatalf("release successor lock: %v", err)
	}
	successor = nil

	fourth, err := acquireShareInstanceLock(path)
	if err != nil {
		t.Fatalf("acquire after completed handover: %v", err)
	}
	if err := fourth.Close(); err != nil {
		t.Fatalf("release fourth lock: %v", err)
	}
}

func TestShareInstanceLockPathIgnoresIsolatedHome(t *testing.T) {
	want, err := shareInstanceLockPath()
	if err != nil {
		t.Fatal(err)
	}
	isolatedHome := t.TempDir()
	t.Setenv("HOME", isolatedHome)
	t.Setenv("USERPROFILE", isolatedHome)
	got, err := shareInstanceLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("lock path changed with isolated HOME: got %q, want %q", got, want)
	}
	if filepath.Dir(got) == isolatedHome {
		t.Fatalf("lock path unexpectedly uses isolated HOME: %q", got)
	}
}

func TestShareInstanceLockAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), shareLockFileName)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShareInstanceLockHelper$")
	cmd.Env = append(os.Environ(), "REMOTE_SHARE_LOCK_HELPER=1", "REMOTE_SHARE_LOCK_PATH="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("helper startup: line=%q err=%v", line, err)
	}
	if lock, err := lockShareFile(path); lock != nil || !errors.Is(err, ErrShareAlreadyRunning) {
		if lock != nil {
			lock.Close()
		}
		stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("second process lock = %v, %v; want ErrShareAlreadyRunning", lock, err)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
}

func TestShareInstanceLockHelper(t *testing.T) {
	if os.Getenv("REMOTE_SHARE_LOCK_HELPER") != "1" {
		return
	}
	lock, err := lockShareFile(os.Getenv("REMOTE_SHARE_LOCK_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Println("locked")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestNewShareInstancesUseIndependentClientIDs(t *testing.T) {
	first := NewShareMode(&Config{}, "127.0.0.1:22", true, agent.SandboxConfig{}, "", "")
	second := NewShareMode(&Config{}, "127.0.0.1:2222", true, agent.SandboxConfig{}, "", "")
	if first.clientID == "" || second.clientID == "" {
		t.Fatal("new instance client ID must not be empty")
	}
	if first.clientID == second.clientID {
		t.Fatalf("new share instances use the same client ID: %q", first.clientID)
	}
	want, err := first.registrationClientID()
	if err != nil {
		t.Fatal(err)
	}
	got, err := first.registrationClientID()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("new instance client ID changed during its process lifetime: %q != %q", got, want)
	}
}

func TestDefaultShareClientIDRemainsPersistent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first, err := GetOrCreateClientID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GetOrCreateClientID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("default share client ID changed: %q != %q", first, second)
	}
	if _, err := os.Stat(filepath.Join(home, clientIDFileName)); err != nil {
		t.Fatalf("persistent client ID file missing: %v", err)
	}
}
