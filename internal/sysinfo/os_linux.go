//go:build linux

package sysinfo

import (
	"bufio"
	"os"
	"strings"
)

// osName 读 /etc/os-release 的 ID（uos、kylin、ubuntu、debian……），
// 读不到时回落 "linux"。用 ID 而非 PRETTY_NAME：短、稳定、够区分发行版。
func osName() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "linux"
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		v, ok := strings.CutPrefix(line, "ID=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		if v != "" {
			return v
		}
	}
	return "linux"
}
