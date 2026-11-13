//go:build !windows && !linux

package tools

import (
	"os/exec"
	"strconv"
	"strings"
)

// collectTree 通过 ps 取父子关系，返回 root 的全部后代（不含 root）。
func collectTree(root int) []int {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	ppids := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		pp, e2 := strconv.Atoi(f[1])
		if e1 == nil && e2 == nil {
			ppids[pid] = pp
		}
	}
	set := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for pid, pp := range ppids {
			if pid != root && !set[pid] && (pp == root || set[pp]) {
				set[pid] = true
				changed = true
			}
		}
	}
	res := make([]int, 0, len(set))
	for pid := range set {
		res = append(res, pid)
	}
	return res
}
