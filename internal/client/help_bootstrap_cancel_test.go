package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingCaller：第一个 write_file 卡住直到 release 关闭，其余立即成功。
// 目的是把派发循环稳定地钉在 `sem <- struct{}{}`（uploadConcurrency=1，槽位被首个
// worker 占着）上，这样测试取消外层 ctx 时必然走进 select 的 <-cctx.Done() 分支。
// 注意这里刻意不监听 ctx：若 write_file 自己也因取消而返回错误，就会先触发 setErr，
// 循环走的是 hasErr 分支而非 Done 分支，测试会变成竞态且测不到目标缺陷。
type blockingCaller struct {
	mu      sync.Mutex
	remote  map[string][]byte
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingCaller() *blockingCaller {
	return &blockingCaller{
		remote:  map[string][]byte{},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (c *blockingCaller) CallTool(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if name != "write_file" {
		return json.Marshal(struct{}{})
	}
	var a struct {
		Path    string `json:"path"`
		Content []byte `json:"content"`
		At      *int64 `json:"at"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	first := false
	c.once.Do(func() { first = true; close(c.started) })
	if first {
		<-c.release
	}
	c.mu.Lock()
	if a.At != nil {
		end := int(*a.At) + len(a.Content)
		data := c.remote[a.Path]
		if end > len(data) {
			nd := make([]byte, end)
			copy(nd, data)
			data = nd
		}
		copy(data[*a.At:], a.Content)
		c.remote[a.Path] = data
	} else {
		c.remote[a.Path] = append([]byte(nil), a.Content...)
	}
	c.mu.Unlock()
	return json.Marshal(struct{}{})
}

// 回归：上传途中外层 ctx 被取消（notifications/cancelled、GUI 超时）时，doUploadFile
// 必须及时返回错误。
//
// 修复前 select 的 <-cctx.Done() 分支是空的，于是：
//   - hasErr() 仍为 false，循环继续为后续 chunk 起 worker，而这些 worker 的
//     `defer <-sem` 没有配对的 put（Done 分支没往 sem 里放东西），sem 容量只有 1，
//     它们全都永久阻塞在 <-sem 上 → wg.Wait() 死锁，整个 MCP 调用挂死；
//   - 即便侥幸跳出，firstErr==nil 会让函数返回 {Bytes: total} 谎报成功，而 total 里
//     含着从未写到远端的 chunk。续传是按远端 stat size 去重的，这个假成功会把后续
//     续传点带偏。
//
// 因此本测试同时断言「不挂死」和「不谎报成功」两件事。
func TestUploadReturnsErrorOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	content := make([]byte, fileTransferChunk*4+321) // 多个 chunk，确保取消时还有待派发的块
	for i := range content {
		content[i] = byte((i*29 + 11) % 251)
	}
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}

	fc := newBlockingCaller()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args, _ := json.Marshal(fileTransferArgs{LocalPath: src, RemotePath: "/r/cancel"})
	type outcome struct {
		res json.RawMessage
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := (&HelpMCPBootstrap{}).doUploadFile(ctx, fc, args)
		done <- outcome{res, err}
	}()

	<-fc.started // 首块已进 worker，槽位被占，派发循环必定阻塞在 sem 上
	cancel()
	time.Sleep(50 * time.Millisecond) // 给循环时间走到 <-cctx.Done() 分支
	close(fc.release)                 // 放开首个 worker，让 wg.Wait 有机会返回

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("取消后应返回错误，实际 err=nil res=%s（谎报上传成功会带坏续传去重）", got.res)
		}
		if !strings.Contains(got.err.Error(), "cancel") {
			t.Errorf("错误信息应说明是取消，实际: %v", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("doUploadFile 在 ctx 取消后未返回（sem 无配对 put 导致 wg.Wait 永久阻塞）")
	}
}
