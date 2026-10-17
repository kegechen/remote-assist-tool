package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// recordingCaller 在 fakeCaller 之上记下每次调用的工具名和参数。
type recordingCaller struct {
	*fakeCaller
	mu    sync.Mutex
	calls []recordedCall
}

type recordedCall struct {
	name string
	args json.RawMessage
}

func (r *recordingCaller) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	r.mu.Lock()
	r.calls = append(r.calls, recordedCall{name, append(json.RawMessage(nil), args...)})
	r.mu.Unlock()
	return r.fakeCaller.CallTool(ctx, name, args)
}

// upload_file / download_file 的每个分块都要带上文件总大小 total，share 端才能
// 显示百分比和剩余时间（见 agent.parseTransfer）。
func TestFileTransferChunksCarryTotalHint(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.bin")
	content := make([]byte, fileTransferChunk*2+777)
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	want := int64(len(content))
	b := &HelpMCPBootstrap{}
	rc := &recordingCaller{fakeCaller: newFakeCaller()}

	args, _ := json.Marshal(fileTransferArgs{LocalPath: src, RemotePath: "/r/a"})
	if _, err := b.doUploadFile(context.Background(), rc, args); err != nil {
		t.Fatalf("upload: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "dst.bin")
	args, _ = json.Marshal(fileTransferArgs{LocalPath: dst, RemotePath: "/r/a"})
	if _, err := b.doDownloadFile(context.Background(), rc, args); err != nil {
		t.Fatalf("download: %v", err)
	}

	seen := map[string]int{}
	for _, c := range rc.calls {
		if c.name != "write_file" && c.name != "read_file" {
			continue
		}
		var a struct {
			Total int64 `json:"total"`
		}
		json.Unmarshal(c.args, &a)
		if a.Total != want {
			t.Errorf("%s 参数缺少 total=%d：%.120s", c.name, want, c.args)
		}
		seen[c.name]++
	}
	// 1 次 truncate + 3 个分块；下载 3 个分块
	if seen["write_file"] != 4 || seen["read_file"] != 3 {
		t.Fatalf("调用次数不对：%v", seen)
	}
}
