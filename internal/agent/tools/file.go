package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/remote-assist/tool/internal/agent"
)

type ReadFileArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
}

type ReadFileResult struct {
	Bytes []byte `json:"bytes"`
	EOF   bool   `json:"eof"`
}

const readFileMaxChunk = 1 << 20 // 1 MiB

type ReadFileTool struct{ sb *agent.Sandbox }

func NewReadFile(sb *agent.Sandbox) *ReadFileTool { return &ReadFileTool{sb: sb} }
func (t *ReadFileTool) Name() string              { return "read_file" }

func (t *ReadFileTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a ReadFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	resolved, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file_not_found: %s", a.Path)
		}
		return nil, err
	}
	defer f.Close()
	if a.Offset > 0 {
		if _, err := f.Seek(a.Offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	limit := int64(readFileMaxChunk)
	if a.Length > 0 && a.Length < limit {
		limit = a.Length
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if !eof && err != nil {
		return nil, err
	}
	return json.Marshal(ReadFileResult{Bytes: buf[:n], EOF: eof})
}

type WriteFileArgs struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
	Create  bool   `json:"create,omitempty"`
	Append  bool   `json:"append,omitempty"`
	At      *int64 `json:"at,omitempty"` // 非 nil：在绝对偏移处 pwrite（幂等、可乱序，供流水线并发上传）
}

type WriteFileResult struct {
	BytesWritten int `json:"bytes_written"`
}

type WriteFileTool struct{ sb *agent.Sandbox }

func NewWriteFile(sb *agent.Sandbox) *WriteFileTool { return &WriteFileTool{sb: sb} }
func (t *WriteFileTool) Name() string               { return "write_file" }

func (t *WriteFileTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a WriteFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	resolved, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	mode := fs.FileMode(0644)
	if a.Mode != 0 {
		mode = fs.FileMode(a.Mode)
	}
	// 绝对偏移写（pwrite）：幂等、可乱序，供流水线并发上传。不 truncate、不 append；
	// 文件按需创建/扩展（offset > 当前大小时中间为稀疏 0，由后续 chunk 填补）。
	if a.At != nil {
		f, err := os.OpenFile(resolved, os.O_WRONLY|os.O_CREATE, mode)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		n, err := f.WriteAt(a.Content, *a.At)
		if err != nil {
			return nil, err
		}
		return json.Marshal(WriteFileResult{BytesWritten: n})
	}
	flag := os.O_WRONLY | os.O_TRUNC
	if a.Append {
		flag = os.O_WRONLY | os.O_APPEND
	}
	if !a.Create && !fileExists(resolved) {
		return nil, fmt.Errorf("file_not_found: %s", a.Path)
	}
	flag |= os.O_CREATE
	f, err := os.OpenFile(resolved, flag, mode)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := f.Write(a.Content)
	if err != nil {
		return nil, err
	}
	return json.Marshal(WriteFileResult{BytesWritten: n})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
