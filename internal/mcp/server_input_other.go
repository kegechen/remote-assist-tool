//go:build !windows

package mcp

func isPlatformClosedInputError(error) bool { return false }
