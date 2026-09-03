package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

func grepIn(t *testing.T, ctx context.Context, root string, extra map[string]any) (json.RawMessage, error) {
	t.Helper()
	m := map[string]any{"pattern": "NEEDLE", "root": root}
	for k, v := range extra {
		m[k] = v
	}
	args, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	return NewGrep(sb).Run(ctx, args, nil)
}

// TestGrepStopsOnCanceledContext 守住"调用方不等了就别再扫"。
//
// 修复前 WalkDir 回调完全不看 ctx：工具调用早就超时、隧道也断了，被协助端仍然会把整棵树
// 扫完才罢休——用户那边看到的是命令没反应，机器上却在实打实地转。
func TestGrepStopsOnCanceledContext(t *testing.T) {
	root := t.TempDir()
	// 铺一棵足够大的树，让"没有 ctx 检查"和"有 ctx 检查"在耗时上拉开差距
	body := bytes.Repeat([]byte("filler line that will never match\n"), 400)
	for i := 0; i < 300; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%20))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), body, 0o644)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 一开始就取消：一个字节都不该被扫

	_, err := grepIn(t, ctx, root, nil)
	if err == nil {
		t.Fatal("ctx 已取消，grep 仍然把结果当成完整的返回了")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v，期望包住 context.Canceled", err)
	}
}

// TestGrepStopsMidFileOnDeadline 覆盖单个巨型文件的情形：只在文件粒度上检查取消是不够的，
// 一个几百万行的日志能顶着一次 WalkDir 回调跑很久。
func TestGrepStopsMidFileOnDeadline(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	for i := 0; i < 3_000_000; i++ {
		buf.WriteString("no match here at all\n")
	}
	if err := os.WriteFile(filepath.Join(root, "huge.log"), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf.Reset()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := grepIn(t, ctx, root, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("deadline 已过，grep 仍然把结果当成完整的返回了")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v，期望包住 context.DeadlineExceeded", err)
	}
	// 单文件内部不检查 ctx 的话，这里会一直扫到 300 万行读完为止。
	if elapsed > 5*time.Second {
		t.Fatalf("扫到 %v 才停：文件内部没有检查取消", elapsed)
	}
}

// TestGrepFollowsSymlinkToRegularFile 是上一条的反面守卫：拦非普通文件不能顺手把软链也
// 拦了——"源码目录里放软链"是很常见的布局，一刀切会让人搜不到东西。
func TestGrepFollowsSymlinkToRegularFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	os.WriteFile(target, []byte("line one\nhas NEEDLE here\n"), 0o644)
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("这个环境建不了软链: %v", err)
	}
	out, err := grepIn(t, context.Background(), root, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r GrepResult
	json.Unmarshal(out, &r)
	// 本体和软链各算一次
	if len(r.Matches) != 2 {
		t.Fatalf("期望本体 + 软链各命中一次，实际 %+v", r.Matches)
	}
}

// TestGrepCapsScannedFiles 守住文件数上限。
//
// max_matches 只在"命中够了"时才刹得住车；模式一个都匹配不上的时候它一点用没有，
// root 指到一棵大树上就会把整块盘读一遍。上限触发时必须标 truncated，否则调用方会把
// 一份不完整的结果当成"确实没有"。
func TestGrepCapsScannedFiles(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("f%03d.txt", i)), []byte("has NEEDLE here\n"), 0o644)
	}

	orig := grepMaxFiles
	grepMaxFiles = 5
	t.Cleanup(func() { grepMaxFiles = orig })

	out, err := grepIn(t, context.Background(), root, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r GrepResult
	json.Unmarshal(out, &r)
	if len(r.Matches) != 5 {
		t.Fatalf("扫了 %d 个文件，上限是 %d", len(r.Matches), grepMaxFiles)
	}
	if !r.Truncated {
		t.Fatal("撞上文件数上限却没有标 truncated")
	}
}

// TestGrepDoesNotFlagTruncatedBelowCap 是上一条的反向守卫，防止上限逻辑写反把
// 正常的完整结果误标成截断。
func TestGrepDoesNotFlagTruncatedBelowCap(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("f%03d.txt", i)), []byte("has NEEDLE here\n"), 0o644)
	}
	out, err := grepIn(t, context.Background(), root, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r GrepResult
	json.Unmarshal(out, &r)
	if len(r.Matches) != 5 {
		t.Fatalf("matches=%d want 5", len(r.Matches))
	}
	if r.Truncated {
		t.Fatal("文件数远低于上限却被标成截断")
	}
}
