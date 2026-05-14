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
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
	Glob      string `json:"glob,omitempty"`
}
type DirEntry struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}
type ListDirResult struct {
	Entries []DirEntry `json:"entries"`
}

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
		info, _ := d.Info()
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
	return json.Marshal(ListDirResult{Entries: out})
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
		rel, _ := filepath.Rel(resolved, m)
		out = append(out, rel)
	}
	return json.Marshal(GlobResult{Paths: out})
}
