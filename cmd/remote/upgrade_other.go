//go:build !linux && !windows

package main

import "fmt"

func runUpgradeCommand(name string, args []string) error {
	return fmt.Errorf("%s is currently supported only on Linux", name)
}
