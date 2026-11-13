package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowPTY 在 fakePTY 之上模拟“对端不读输入”：block 非 nil 时 Write 阻塞到 block 关闭；
// delay>0 时每次 Write 睡一会儿（模拟吞吐慢）。记录所有写入与 Interrupt 调用。
type slowPTY struct {
	*fakePTY
	mu         sync.Mutex
	block      chan struct{}
	delay      time.Duration
	written    bytes.Buffer
	interrupts []byte
	canIntr    bool
}

func (p *slowPTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	blk, d := p.block, p.delay
	p.mu.Unlock()
	if blk != nil {
		<-blk
	}
	if d > 0 {
		time.Sleep(d)
	}
	p.mu.Lock()
	p.written.Write(b)
	p.mu.Unlock()
	return len(b), nil
}

// Interrupt 实现可选的 ptyInterrupter（unix 下由 TIOCGPGRP+kill 实现）。
func (p *slowPTY) Interrupt(b byte) bool {
	if !p.canIntr {
		return false
	}
	p.mu.Lock()
	p.interrupts = append(p.interrupts, b)
	p.mu.Unlock()
	return true
}

func (p *slowPTY) got() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.written.Bytes()...)
}

func startSlowSession(t *testing.T, canIntr bool) (*ptyWriteTool, *slowPTY, func()) {
	t.Helper()
	m, open, write, _, _, _ := newFakeTools(t, false)
	sp := &slowPTY{canIntr: canIntr}
	m.start = func(spec ptySpec) (ptyProc, error) {
		sp.fakePTY = newFakePTY(spec)
		return sp, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		open.Run(ctx, json.RawMessage(`{"session":"u1"}`), &ptyCaptureSink{})
		close(done)
	}()
	waitFor(t, func() bool { return m.Count() == 1 }, "会话登记")
	return write, sp, func() {
		sp.mu.Lock()
		if sp.block != nil {
			select {
			case <-sp.block:
			default:
				close(sp.block)
			}
		}
		sp.mu.Unlock()
		cancel()
		<-done
	}
}

func writeArgs(data []byte) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"session": "u1", "data": data})
	return b
}

// 大块写入被卡住（对端 PTY 缓冲满）时，Ctrl+C 不能排在它后面等到超时，应经紧急通道立即送达。
func TestPTYCtrlCBypassesBlockedWrite(t *testing.T) {
	oldWait := ptyUrgentLockWait
	ptyUrgentLockWait = 100 * time.Millisecond
	defer func() { ptyUrgentLockWait = oldWait }()
	write, sp, stop := startSlowSession(t, true)
	defer stop()
	sp.mu.Lock()
	sp.block = make(chan struct{})
	sp.mu.Unlock()

	go write.Run(context.Background(), writeArgs(bytes.Repeat([]byte("x"), 8192)), nil) // 卡在 proc.Write
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	if _, err := write.Run(context.Background(), writeArgs([]byte{0x03}), nil); err != nil {
		t.Fatalf("Ctrl+C 写入失败: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Ctrl+C 被卡了 %v", d)
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.interrupts) != 1 || sp.interrupts[0] != 0x03 {
		t.Fatalf("应通过紧急通道送出 0x03，实际 %v", sp.interrupts)
	}
}

// 没有紧急通道（如 Windows）且写入被卡住时，应当在超时内报错而不是无限排队。
func TestPTYWriteBlockedFailsFastWithoutInterrupter(t *testing.T) {
	oldU, oldW := ptyUrgentLockWait, ptyWriteLockWait
	ptyUrgentLockWait, ptyWriteLockWait = 80*time.Millisecond, 300*time.Millisecond
	defer func() { ptyUrgentLockWait, ptyWriteLockWait = oldU, oldW }()
	write, sp, stop := startSlowSession(t, false)
	defer stop()
	sp.mu.Lock()
	sp.block = make(chan struct{})
	sp.mu.Unlock()
	go write.Run(context.Background(), writeArgs(bytes.Repeat([]byte("x"), 100)), nil)
	time.Sleep(50 * time.Millisecond)

	for _, data := range [][]byte{{0x03}, []byte("ls\r")} {
		start := time.Now()
		_, err := write.Run(context.Background(), writeArgs(data), nil)
		if err == nil || !strings.Contains(err.Error(), "pty_write_blocked") {
			t.Fatalf("data=%q 应报 pty_write_blocked，实际 %v", data, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("等待过久 %v", d)
		}
	}
}

// 大段粘贴按小块逐块拿锁，块间让位：途中到达的 Ctrl+C 应插在粘贴中间送达，而不是等整段写完。
func TestPTYBulkWriteYieldsToCtrlC(t *testing.T) {
	write, sp, stop := startSlowSession(t, false)
	defer stop()
	sp.mu.Lock()
	sp.delay = 3 * time.Millisecond
	sp.mu.Unlock()
	paste := bytes.Repeat([]byte("a"), 64*1024) // 16 块 × 3ms ≈ 50ms+
	pasteDone := make(chan error, 1)
	go func() { _, err := write.Run(context.Background(), writeArgs(paste), nil); pasteDone <- err }()
	waitFor(t, func() bool { return len(sp.got()) > 0 }, "粘贴开始写入")
	if _, err := write.Run(context.Background(), writeArgs([]byte{0x03}), nil); err != nil {
		t.Fatal(err)
	}
	if err := <-pasteDone; err != nil {
		t.Fatal(err)
	}
	got := sp.got()
	if len(got) != len(paste)+1 {
		t.Fatalf("总字节=%d 期望 %d", len(got), len(paste)+1)
	}
	pos := bytes.IndexByte(got, 0x03)
	if pos < 0 || pos >= len(paste)-ptyWriteChunk {
		t.Fatalf("0x03 位置=%d，应插在粘贴中间（总 %d），说明排在了整段写入之后", pos, len(got))
	}
	if bytes.Count(got, []byte("a")) != len(paste) {
		t.Fatal("粘贴内容不完整")
	}
}

func TestIsUrgentInput(t *testing.T) {
	yes := [][]byte{{0x03}, {0x1a}, {0x1c}, []byte("ab\x03")}
	no := [][]byte{{}, []byte("ls\r"), {0x04}, bytes.Repeat([]byte{0x03}, 200)}
	for _, b := range yes {
		if !isUrgentInput(b) {
			t.Errorf("%q 应是紧急输入", b)
		}
	}
	for _, b := range no {
		if isUrgentInput(b) {
			t.Errorf("%q 不应是紧急输入", b)
		}
	}
}

// 一次输入里有多个 Ctrl+C（连按 / 粘贴）时，每种控制信号只递送一次：
// Windows 上每次递送都要拉起一个辅助进程，逐字节递送会串行拉起一串进程拖住调用。
func TestPTYInterruptOncePerSignalKind(t *testing.T) {
	oldWait := ptyUrgentLockWait
	ptyUrgentLockWait = 100 * time.Millisecond
	defer func() { ptyUrgentLockWait = oldWait }()
	write, sp, stop := startSlowSession(t, true)
	defer stop()
	sp.mu.Lock()
	sp.block = make(chan struct{})
	sp.mu.Unlock()

	go write.Run(context.Background(), writeArgs(bytes.Repeat([]byte("x"), 8192)), nil) // 卡在 proc.Write
	time.Sleep(50 * time.Millisecond)

	if _, err := write.Run(context.Background(), writeArgs([]byte{0x03, 0x03, 0x03, 0x1a, 0x03}), nil); err != nil {
		t.Fatalf("紧急写入失败: %v", err)
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if string(sp.interrupts) != string([]byte{0x03, 0x1a}) {
		t.Fatalf("每种控制信号应只递送一次，实际 %v", sp.interrupts)
	}
}
