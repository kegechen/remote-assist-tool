//go:build !windows && !linux

package sysinfo

import "runtime"

func osName() string { return runtime.GOOS }
