//go:build !windows

package tools

import (
	"io"
	"strings"
	"testing"
	"time"
)

// 真实 PTY：Interrupt(0x03) 经 TIOCGPGRP 找到前台进程组并递送 SIGINT，不经过输入管道。
func TestUnixPTYInterruptKillsForeground(t *testing.T) {
	p, err := startPTY(ptySpec{Shell: "/bin/sh", Args: []string{"-i"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Skipf("无法启动 PTY: %v", err)
	}
	defer p.Close()
	in, ok := p.(ptyInterrupter)
	if !ok {
		t.Fatal("unixPTY 应实现 ptyInterrupter")
	}
	out := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), "DONE:130") {
				out <- sb.String()
				return
			}
			if err == io.EOF || err != nil {
				out <- sb.String()
				return
			}
		}
	}()
	// 前台跑 sleep；被 SIGINT 杀掉后 $? 为 130（交互 shell 里 SIGINT 会放弃整行，所以分两次输入）。
	if _, err := p.Write([]byte("sleep 60\r")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // 等 sleep 成为前台进程组
	if !in.Interrupt(0x03) {
		t.Fatal("Interrupt 返回 false")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := p.Write([]byte("echo DONE:$?\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-out:
		if !strings.Contains(s, "DONE:130") {
			t.Fatalf("sleep 未被 SIGINT 中断: %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内未被中断")
	}
	if in.Interrupt('x') {
		t.Error("非控制字符不应触发信号")
	}
}
