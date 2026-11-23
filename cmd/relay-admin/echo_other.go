//go:build !windows && !linux && !darwin

package main

import "os"

// disableEcho 在其它平台上不支持，调用方会提示口令将显示在屏幕上。
func disableEcho(*os.File) (func(), bool) { return nil, false }
