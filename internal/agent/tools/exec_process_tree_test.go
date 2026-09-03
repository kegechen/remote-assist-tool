package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runWithin 在后台跑 fn，超时就直接 Fatalf。回归发生时（Wait 被孙子进程吊死）用例会
// 干脆地失败，而不是把整个包挂到 go test 的 10 分钟超时。
func runWithin(t *testing.T, budget time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(budget):
		t.Fatalf("%s 在 %s 内没有返回", what, budget)
	}
}

// TestExecKillsWholeProcessTree 覆盖 exec 超时取消的两条语义：
//
//  1. 被杀的是整棵子树，不只是直接子进程。`bash -lc`、`npm run` 这类壳层才是 exec 的
//     常态，只杀壳层会把编译器、dev server 留成孤儿继续吃被协助端的 CPU。
//  2. Run 本身要及时返回。孙子进程继承了 stdout 写端，只要它还活着管道就读不到 EOF，
//     cmd.Wait 会无限期挂住——工具调用既不给结果也不给错误，daemon 侧的槽位就此漏掉。
//
// 修复前：CommandContext 默认只 Kill 直接子进程，孙子进程活满 3 秒写下存活标记，
// 而 cmd.Output() 一直等不到管道 EOF，本用例会卡在 runWithin 的预算上失败。
func TestExecKillsWholeProcessTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived.txt")

	args, _ := json.Marshal(map[string]any{
		"argv":       treeHelperArgv(),
		"env":        treeHelperEnv("spawn", helperMarkerEnv, marker),
		"timeout_ms": 700,
	})

	tool := NewExec(nil)
	start := time.Now()
	var err error
	runWithin(t, 8*time.Second, "exec.Run", func() {
		_, err = tool.Run(context.Background(), args, nil)
	})
	if err == nil {
		t.Fatal("期望超时错误")
	}
	// 700ms 超时 + 2s WaitDelay 兜底；正常情况下杀掉整棵树后管道立刻 EOF，远不到这个数。
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run 返回太慢（被孙子进程占着的管道吊住了）：%v", elapsed)
	}

	// 给孙子进程留足"如果没被杀就该写标记"的时间再判定。
	time.Sleep(helperLingerDelay + 2*time.Second)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("孙子进程活过了超时：取消只杀掉了直接子进程，整棵进程树没有被连坐")
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("stat marker: %v", statErr)
	}
}

// TestExecTruncatesLargeOutputCrossPlatform 与 exec_bounded_test.go 里那条同名用例
// 覆盖同一件事，但用 helper 进程产出输出，因此 Windows 上也真的会跑。
func TestExecTruncatesLargeOutputCrossPlatform(t *testing.T) {
	const max = 4096
	args, _ := json.Marshal(map[string]any{
		"argv":             treeHelperArgv(),
		"env":              treeHelperEnv("spew", helperLinesEnv, "20000"),
		"max_output_bytes": max,
	})
	tool := NewExec(nil)
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !r.StdoutTruncated {
		t.Fatalf("期望被截断，实际 stdout len=%d", len(r.Stdout))
	}
	if len(r.Stdout) > max {
		t.Fatalf("stdout len=%d 超过 max=%d", len(r.Stdout), max)
	}
	s := string(r.Stdout)
	if !strings.HasPrefix(s, "line-0\n") {
		t.Fatalf("头部丢了: %q", head(s))
	}
	if !strings.Contains(s, "TAIL-MARKER") {
		t.Fatalf("尾部丢了: %q", tail(s))
	}
}
