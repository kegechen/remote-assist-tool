//go:build linux

package tools

import (
	"os"
	"strconv"
	"strings"
)

// collectTree 返回 root 的全部后代，以及与 root 同会话（sid==root）的进程；不含 root 自身。
// 读 /proc/<pid>/stat：第 4 字段 ppid，第 6 字段 sid（comm 可能含空格/括号，从最后一个 ')' 之后解析）。
func collectTree(root int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	type info struct{ ppid, sid int }
	procs := map[int]info{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		// f[0]=state f[1]=ppid f[2]=pgrp f[3]=session
		if len(f) < 4 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		sid, _ := strconv.Atoi(f[3])
		procs[pid] = info{ppid, sid}
	}
	set := map[int]bool{}
	for pid, in := range procs {
		if pid != root && in.sid == root {
			set[pid] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for pid, in := range procs {
			if pid != root && !set[pid] && (in.ppid == root || set[in.ppid]) {
				set[pid] = true
				changed = true
			}
		}
	}
	out := make([]int, 0, len(set))
	for pid := range set {
		out = append(out, pid)
	}
	return out
}
