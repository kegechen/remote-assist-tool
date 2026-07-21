package tools

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/remote-assist/tool/internal/agent"
)

type FileMD5Args struct {
	Path string `json:"path"`
}

type FileMD5Result struct {
	MD5  string `json:"md5"`
	Size int64  `json:"size"`
}

type FileMD5Tool struct{ sb *agent.Sandbox }

func NewFileMD5(sb *agent.Sandbox) *FileMD5Tool { return &FileMD5Tool{sb: sb} }
func (t *FileMD5Tool) Name() string             { return "file_md5" }

func (t *FileMD5Tool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var args FileMD5Args
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.Path == "" {
		return nil, fmt.Errorf("path required")
	}
	resolved, err := t.sb.ResolvePath(args.Path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", args.Path)
	}
	hash := md5.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: f}); err != nil {
		return nil, err
	}
	return json.Marshal(FileMD5Result{MD5: hex.EncodeToString(hash.Sum(nil)), Size: stat.Size()})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}
