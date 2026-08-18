//go:build windows

package client

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	clipboardUnicodeText = 13
	globalMemoryMoveable = 0x0002
	clipboardOpenTimeout = 2 * time.Second
)

var (
	clipboardUser32           = windows.NewLazySystemDLL("user32.dll")
	clipboardKernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procOpenClipboard         = clipboardUser32.NewProc("OpenClipboard")
	procCloseClipboard        = clipboardUser32.NewProc("CloseClipboard")
	procEmptyClipboard        = clipboardUser32.NewProc("EmptyClipboard")
	procSetClipboardData      = clipboardUser32.NewProc("SetClipboardData")
	procClipboardGlobalAlloc  = clipboardKernel32.NewProc("GlobalAlloc")
	procClipboardGlobalLock   = clipboardKernel32.NewProc("GlobalLock")
	procClipboardGlobalUnlock = clipboardKernel32.NewProc("GlobalUnlock")
	procClipboardGlobalFree   = clipboardKernel32.NewProc("GlobalFree")
	procClipboardMoveMemory   = clipboardKernel32.NewProc("RtlMoveMemory")
)

func copyToClipboardPlatform(text string) error {
	utf16Text, err := windows.UTF16FromString(text)
	if err != nil {
		return fmt.Errorf("encode clipboard text as UTF-16: %w", err)
	}

	byteCount := uintptr(len(utf16Text) * 2)
	globalMemory, _, callErr := procClipboardGlobalAlloc.Call(globalMemoryMoveable, byteCount)
	if globalMemory == 0 {
		return windowsClipboardError("GlobalAlloc", callErr)
	}
	ownedByClipboard := false
	defer func() {
		if !ownedByClipboard {
			procClipboardGlobalFree.Call(globalMemory)
		}
	}()

	lockedMemory, _, callErr := procClipboardGlobalLock.Call(globalMemory)
	if lockedMemory == 0 {
		return windowsClipboardError("GlobalLock", callErr)
	}
	procClipboardMoveMemory.Call(lockedMemory, uintptr(unsafe.Pointer(&utf16Text[0])), byteCount)
	runtime.KeepAlive(utf16Text)
	procClipboardGlobalUnlock.Call(globalMemory)

	if err := openClipboardWithRetry(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()

	result, _, callErr := procEmptyClipboard.Call()
	if result == 0 {
		return windowsClipboardError("EmptyClipboard", callErr)
	}
	result, _, callErr = procSetClipboardData.Call(clipboardUnicodeText, globalMemory)
	if result == 0 {
		return windowsClipboardError("SetClipboardData", callErr)
	}
	ownedByClipboard = true
	return nil
}

func openClipboardWithRetry() error {
	deadline := time.Now().Add(clipboardOpenTimeout)
	var lastErr error
	for {
		result, _, callErr := procOpenClipboard.Call(0)
		if result != 0 {
			return nil
		}
		lastErr = windowsClipboardError("OpenClipboard", callErr)
		if !time.Now().Before(deadline) {
			return lastErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func windowsClipboardError(operation string, err error) error {
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return fmt.Errorf("%s failed", operation)
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}
