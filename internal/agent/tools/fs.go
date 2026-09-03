package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/remote-assist/tool/internal/agent"
)

type ListDirArgs struct {
	Path       string `json:"path"`
	Recursive  bool   `json:"recursive,omitempty"`
	Glob       string `json:"glob,omitempty"`
	MaxEntries int    `json:"max_entries,omitempty"`
}
type DirEntry struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}
type ListDirResult struct {
	Entries   []DirEntry `json:"entries"`
	Total     int        `json:"total,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

const defaultListDirMax = 200

type ListDirTool struct{ sb *agent.Sandbox }

func NewListDir(sb *agent.Sandbox) *ListDirTool { return &ListDirTool{sb: sb} }
func (t *ListDirTool) Name() string             { return "list_dir" }

func (t *ListDirTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a ListDirArgs
	json.Unmarshal(raw, &a)
	root, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	var out []DirEntry
	maxEntries := a.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultListDirMax
	}
	total := 0
	truncated := false
	walk := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == root {
			return nil
		}
		if a.Glob != "" {
			matched, _ := filepath.Match(a.Glob, d.Name())
			if !matched {
				if d.IsDir() && !a.Recursive {
					return fs.SkipDir
				}
				return nil
			}
		}
		total++
		if len(out) >= maxEntries {
			truncated = true
			if a.Recursive {
				return filepath.SkipAll
			}
			return nil
		}
		// Info() 在 Unix 上是惰性 lstat：条目在 ReadDir/WalkDir 之后被删除时返回
		// (nil, ErrNotExist)。丢掉 err 直接 info.Size() 会 nil 解引用 panic，daemon 的
		// recover 虽然兜住了进程，但整次调用会退化成 remote_panic 并丢光已收集的条目。
		// list 一个正在跑构建的 target/ 或日志轮转目录就能撞上。
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		kind := "other"
		switch {
		case d.IsDir():
			kind = "dir"
		case d.Type().IsRegular():
			kind = "file"
		case d.Type()&fs.ModeSymlink != 0:
			kind = "symlink"
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, DirEntry{Name: rel, Kind: kind, Size: info.Size(), Mtime: info.ModTime().Unix()})
		if d.IsDir() && !a.Recursive && p != root {
			return fs.SkipDir
		}
		return nil
	}
	if a.Recursive {
		filepath.WalkDir(root, walk)
	} else {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, d := range entries {
			walk(filepath.Join(root, d.Name()), d, nil)
		}
	}
	res := ListDirResult{Entries: out, Truncated: truncated}
	// 递归模式一旦命中上限就 SkipAll 停止遍历，剩下还有多少无从得知，此时报 total
	// 只会误导（它恰好等于 maxEntries+1）。非递归模式会走完整个目录，total 是准的。
	if !(a.Recursive && truncated) {
		res.Total = total
	}
	return json.Marshal(res)
}

type StatArgs struct{ Path string `json:"path"` }
type StatResult struct {
	Kind  string `json:"kind"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Mode  uint32 `json:"mode"`
}

type StatTool struct{ sb *agent.Sandbox }

func NewStat(sb *agent.Sandbox) *StatTool { return &StatTool{sb: sb} }
func (t *StatTool) Name() string          { return "stat" }

func (t *StatTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a StatArgs
	json.Unmarshal(raw, &a)
	resolved, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file_not_found: %s", a.Path)
		}
		return nil, err
	}
	kind := "other"
	switch {
	case info.IsDir():
		kind = "dir"
	case info.Mode().IsRegular():
		kind = "file"
	case info.Mode()&fs.ModeSymlink != 0:
		kind = "symlink"
	}
	return json.Marshal(StatResult{Kind: kind, Size: info.Size(), Mtime: info.ModTime().Unix(), Mode: uint32(info.Mode())})
}

type GlobArgs struct {
	Pattern string `json:"pattern"`
	Root    string `json:"root,omitempty"`
}
type GlobResult struct{ Paths []string `json:"paths"` }

type GlobTool struct{ sb *agent.Sandbox }

func NewGlob(sb *agent.Sandbox) *GlobTool { return &GlobTool{sb: sb} }
func (t *GlobTool) Name() string          { return "glob" }

func (t *GlobTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a GlobArgs
	json.Unmarshal(raw, &a)
	root := a.Root
	if root == "" {
		root = "."
	}
	resolved, err := t.sb.ResolvePath(root)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(resolved, a.Pattern))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		// pattern 里的 ".." 会在 Join 时被一起 Clean 掉，绕过上面只针对 root 的那次校验
		// （pattern:"../*.txt" 就能列出 root 外的条目）。这是本包里唯一一处"先校验再拼接"，
		// list_dir/stat/grep 都是拼好再校验。逐个 match 补一次沙箱，让护栏行为一致——
		// 注意 --root 本就只是防手滑护栏而非安全边界（见 Sandbox.ResolvePath 注释），
		// 这里修的是"护栏漏了一个口子"，不是堵安全漏洞。
		if _, err := t.sb.ResolvePath(m); err != nil {
			continue
		}
		rel, _ := filepath.Rel(resolved, m)
		out = append(out, rel)
	}
	return json.Marshal(GlobResult{Paths: out})
}
