//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestGrepSkipsFIFO 守住"不要去 open 非普通文件"。
//
// 没有写端的 FIFO，os.Open 会一直阻塞在那儿；卡住的不只是这一个文件——整次 grep 都回不到
// 循环，连回调开头新加的 ctx 检查都轮不到，工具调用就此永久挂死。WalkDir 会把 FIFO、
// 字符设备、unix socket 一视同仁地交上来，所以这不是理论问题：用户 root 指到 $HOME 或
// /tmp 上，撞见一个别的程序留下的命名管道就够了。
func TestGrepSkipsFIFO(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "real.txt"), []byte("has NEEDLE here\n"), 0o644)
	// 名字排在 real.txt 前面，保证 WalkDir 先走到它
	fifo := filepath.Join(root, "aaa.fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo 不可用: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	var out json.RawMessage
	var err error
	go func() {
		defer close(done)
		out, err = grepIn(t, ctx, root, nil)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("grep 卡在 FIFO 的 os.Open 上了")
	}
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r GrepResult
	json.Unmarshal(out, &r)
	if len(r.Matches) != 1 || !strings.Contains(r.Matches[0].Text, "NEEDLE") {
		t.Fatalf("FIFO 之后的普通文件没被搜到: %+v", r.Matches)
	}
}
