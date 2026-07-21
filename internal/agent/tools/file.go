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
	AsText bool   `json:"as_text,omitempty"`
}

type ReadFileResult struct {
	Bytes  []byte `json:"bytes"`
	EOF    bool   `json:"eof"`
	AsText bool   `json:"as_text,omitempty"`
}

const (
	readFileMaxChunk     = 1 << 20  // 1 MiB：显式传 length 时的硬上限
	readFileDefaultChunk = 64 << 10 // 64 KiB：未传 length 时的默认块大小
)

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
	// 未显式传 length 时用一个对 MCP 友好的默认块大小，在源头限量而不是事后截断
	// 返回的文本：这样 bytes_len 与 eof 始终与实际返回的内容一致，调用方可以靠
	// offset += bytes_len 一路续读到 eof=true，完整拿到大文件而不丢内容。
	// download_file 等批量传输显式传 length（512 KiB），不受这个默认值影响。
	limit := int64(readFileDefaultChunk)
	if a.Length > 0 {
		limit = a.Length
		if limit > readFileMaxChunk {
			limit = readFileMaxChunk
		}
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if !eof && err != nil {
		return nil, err
	}
	return json.Marshal(ReadFileResult{Bytes: buf[:n], EOF: eof, AsText: a.AsText})
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
