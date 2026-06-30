package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeCaller 实现 toolCaller，用内存 map 模拟 share 端文件系统，
// 支持 write_file(create/append/at-WriteAt) 与 read_file(offset/length)、stat。
// 加锁模拟 share 端并发 goroutine（go d.handleReq）对同一文件的并发访问。
type fakeCaller struct {
	mu     sync.Mutex
	remote map[string][]byte
}

func newFakeCaller() *fakeCaller { return &fakeCaller{remote: map[string][]byte{}} }

func (f *fakeCaller) CallTool(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "write_file":
		var a struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
			Create  bool   `json:"create"`
			Append  bool   `json:"append"`
			At      *int64 `json:"at"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		switch {
		case a.At != nil:
			// WriteAt：按需扩展 + 绝对偏移写（模拟 share 端 pwrite 不重叠并发写）
			end := int(*a.At) + len(a.Content)
			data := f.remote[a.Path]
			if end > len(data) {
				nd := make([]byte, end)
				copy(nd, data)
				data = nd
			}
			copy(data[*a.At:], a.Content)
			f.remote[a.Path] = data
		case a.Append:
			f.remote[a.Path] = append(f.remote[a.Path], a.Content...)
		default:
			f.remote[a.Path] = append([]byte(nil), a.Content...) // create/truncate
		}
		return json.Marshal(struct{}{})
	case "read_file":
		var a struct {
			Path   string `json:"path"`
			Offset int64  `json:"offset"`
			Length int64  `json:"length"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		data := f.remote[a.Path]
		off := a.Offset
		if off > int64(len(data)) {
			off = int64(len(data))
		}
		end := off + a.Length
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		return json.Marshal(struct {
			Bytes []byte `json:"bytes"`
			EOF   bool   `json:"eof"`
		}{Bytes: data[off:end], EOF: end >= int64(len(data))})
	case "stat":
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		data, ok := f.remote[a.Path]
		if !ok {
			return nil, errors.New("file_not_found")
		}
		return json.Marshal(struct {
			Size int64 `json:"size"`
		}{Size: int64(len(data))})
	}
	return nil, nil
}

func TestUploadDownloadResume(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	// 跨多 chunk（>2×512KiB）+ 非整块尾巴，覆盖首块/中间块/尾块
	content := make([]byte, fileTransferChunk*2+12345)
	for i := range content {
		content[i] = byte(i % 251)
	}
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	b := &HelpMCPBootstrap{}
	ctx := context.Background()
	half := int64(fileTransferChunk + 100)

	// 1) 全量 upload → 远端内容应等于源
	fc := newFakeCaller()
	args, _ := json.Marshal(fileTransferArgs{LocalPath: src, RemotePath: "/r/a"})
	if _, err := b.doUploadFile(ctx, fc, args); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !bytes.Equal(fc.remote["/r/a"], content) {
		t.Fatalf("full upload mismatch: got %d want %d", len(fc.remote["/r/a"]), len(content))
	}

	// 2) 续传 upload：远端已有前 half，offset 续传后半 → 拼接应等于源
	fc.remote["/r/b"] = append([]byte(nil), content[:half]...)
	args2, _ := json.Marshal(fileTransferArgs{LocalPath: src, RemotePath: "/r/b", Offset: half})
	if _, err := b.doUploadFile(ctx, fc, args2); err != nil {
		t.Fatalf("resume upload: %v", err)
	}
	if !bytes.Equal(fc.remote["/r/b"], content) {
		t.Fatalf("resume upload mismatch: got %d want %d", len(fc.remote["/r/b"]), len(content))
	}

	// 3) 全量 download → 本地内容应等于源
	dst := filepath.Join(dir, "dl.bin")
	dargs, _ := json.Marshal(fileTransferArgs{LocalPath: dst, RemotePath: "/r/a"})
	if _, err := b.doDownloadFile(ctx, fc, dargs); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, content) {
		t.Fatalf("download mismatch")
	}

	// 4) 续传 download：本地已有前 half，offset 续传后半 → 拼接应等于源
	dst2 := filepath.Join(dir, "dl2.bin")
	if err := os.WriteFile(dst2, content[:half], 0o644); err != nil {
		t.Fatal(err)
	}
	dargs2, _ := json.Marshal(fileTransferArgs{LocalPath: dst2, RemotePath: "/r/a", Offset: half})
	if _, err := b.doDownloadFile(ctx, fc, dargs2); err != nil {
		t.Fatalf("resume download: %v", err)
	}
	if got, _ := os.ReadFile(dst2); !bytes.Equal(got, content) {
		t.Fatalf("resume download mismatch")
	}
}

// 续传必须以「远端实际大小」为准：模拟某 chunk 已在远端写入成功、但响应在隧道断时
// 丢失，导致调用方传入的 offset 落后于远端真实进度。修复前会从落后的 offset 重复
// Append、文件膨胀、md5 不符；修复后 doUploadFile 先 stat 远端、以真实大小为续传点，
// 结果应精确等于源、无重复 chunk。
func TestUploadResumeDedupsByRemoteSize(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	content := make([]byte, fileTransferChunk*2+777)
	for i := range content {
		content[i] = byte((i*7 + 3) % 251)
	}
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	fc := newFakeCaller()
	// 远端实际已写入前 2 个 chunk（成功），但调用方只记得传到了 1 个 chunk（响应丢失）
	remoteHave := int64(fileTransferChunk * 2)
	staleOffset := int64(fileTransferChunk)
	fc.remote["/r/x"] = append([]byte(nil), content[:remoteHave]...)

	args, _ := json.Marshal(fileTransferArgs{LocalPath: src, RemotePath: "/r/x", Offset: staleOffset})
	if _, err := (&HelpMCPBootstrap{}).doUploadFile(context.Background(), fc, args); err != nil {
		t.Fatalf("resume upload: %v", err)
	}
	if len(fc.remote["/r/x"]) != len(content) {
		t.Fatalf("remote size=%d want=%d（续传应按远端真实大小去重，不得重复写 chunk）", len(fc.remote["/r/x"]), len(content))
	}
	if !bytes.Equal(fc.remote["/r/x"], content) {
		t.Fatal("续传后远端内容与源不一致")
	}
}

// 流水线并发上传：大文件 10+ chunk 跨并发窗口，每块并发 write_file(绝对 offset 不重叠)。
// 验证并发写同一文件不重叠区域的正确性——最终字节必须精确等于源（用 -race 跑还能查竞争）。
func TestUploadPipelineConcurrent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	content := make([]byte, fileTransferChunk*10+98765) // 10+ chunk，超过 uploadConcurrency 触发滑动窗口
	for i := range content {
		content[i] = byte((i*131 + 7) % 251)
	}
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	fc := newFakeCaller()
	args, _ := json.Marshal(fileTransferArgs{LocalPath: src, RemotePath: "/r/p"})
	if _, err := (&HelpMCPBootstrap{}).doUploadFile(context.Background(), fc, args); err != nil {
		t.Fatalf("pipeline upload: %v", err)
	}
	if len(fc.remote["/r/p"]) != len(content) {
		t.Fatalf("size=%d want=%d", len(fc.remote["/r/p"]), len(content))
	}
	if !bytes.Equal(fc.remote["/r/p"], content) {
		t.Fatal("并发流水线上传后内容与源不一致")
	}
}
