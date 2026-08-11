//go:build !windows

package main

import "io"

func dispatchPlatform(_ []string, _ io.Reader, _, _ io.Writer) (bool, int) {
	return false, 0
}

func prepareInteractiveConsole() {}
