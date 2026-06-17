package client

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fakeCaller 实现 toolCaller，用内存 map 模拟 share 端文件系统，
// 支持 write_file(create/append) 与 read_file(offset/length)，用于验证断点续传拼接。
type fakeCaller struct {
	remote map[string][]byte
}

func newFakeCaller() *fakeCaller { return &fakeCaller{remote: map[string][]byte{}} }

func (f *fakeCaller) CallTool(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "write_file":
		var a struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
			Create  bool   `json:"create"`
			Append  bool   `json:"append"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		if a.Append {
			f.remote[a.Path] = append(f.remote[a.Path], a.Content...)
		} else {
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
