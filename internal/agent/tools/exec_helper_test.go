package tools

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// exec 相关用例需要一个"行为可控的外部命令"。用 /bin/sh 写脚本的话 Windows 上只能
// t.Skip，而进程树、管道占用这些恰恰是各平台差异最大、最需要覆盖的地方。所以这里用
// 测试二进制自己扮演被执行的命令——标准的 helper process 手法。

const (
	helperModeEnv   = "RAT_EXEC_HELPER_MODE"
	helperMarkerEnv = "RAT_EXEC_HELPER_MARKER"
	helperLinesEnv  = "RAT_EXEC_HELPER_LINES"

	// helperLingerDelay 孙子进程"活过"多久才写存活标记。必须明显长于用例给命令的
	// 超时，这样标记文件的有无就等价于"这个孙子有没有被连坐杀掉"。
	helperLingerDelay = 3 * time.Second
)

// TestExecTreeHelper 不是真正的用例：它被上面那些用例当作可执行文件重新拉起，
// 靠环境变量选择扮演的角色。正常 go test 跑到它时 mode 为空，直接返回。
func TestExecTreeHelper(t *testing.T) {
	switch os.Getenv(helperModeEnv) {
	case "spawn":
		// 派生一个孙子进程，并让它继承 stdout——正是这条被继承的管道会把父进程被杀之后
		// 的 cmd.Wait 吊死。然后自己原地睡到被杀。
		grand := exec.Command(os.Args[0], "-test.run=TestExecTreeHelper")
		grand.Env = append(os.Environ(), helperModeEnv+"=linger")
		grand.Stdout = os.Stdout
		grand.Stderr = os.Stderr
		if err := grand.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "spawn grandchild:", err)
			os.Exit(1)
		}
		time.Sleep(time.Minute)
		os.Exit(0)

	case "linger":
		time.Sleep(helperLingerDelay)
		// 能走到这里就说明没被连坐杀掉。先落盘再说别的：stdout 此时多半已经关了，
		// 测试框架收尾时往里写会直接把进程带走。
		if p := os.Getenv(helperMarkerEnv); p != "" {
			os.WriteFile(p, []byte("survived"), 0o600)
		}
		os.Exit(0)

	case "spew":
		n, _ := strconv.Atoi(os.Getenv(helperLinesEnv))
		w := bufio.NewWriterSize(os.Stdout, 64<<10)
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "line-%d\n", i)
		}
		fmt.Fprintln(w, "TAIL-MARKER")
		w.Flush()
		os.Exit(0)
	}
}

// head / tail 只用于断言失败时打个能看懂的片段，避免把几 KB 输出整块糊进日志。
func head(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

func tail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}

// treeHelperArgv 返回"重新拉起测试二进制并只跑 helper"的 argv。
func treeHelperArgv() []string {
	return []string{os.Args[0], "-test.run=TestExecTreeHelper"}
}

// helperEnv 组装 helper 的角色环境变量。
func treeHelperEnv(mode string, kv ...string) map[string]string {
	env := map[string]string{helperModeEnv: mode}
	for i := 0; i+1 < len(kv); i += 2 {
		env[kv[i]] = kv[i+1]
	}
	return env
}
