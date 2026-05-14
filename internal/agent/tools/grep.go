package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/remote-assist/tool/internal/agent"
)

type GrepArgs struct {
	Pattern    string `json:"pattern"`
	Root       string `json:"root,omitempty"`
	Glob       string `json:"glob,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	MaxMatches int    `json:"max_matches,omitempty"`
}

type GrepMatch struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type GrepResult struct{ Matches []GrepMatch `json:"matches"` }

type GrepTool struct{ sb *agent.Sandbox }

func NewGrep(sb *agent.Sandbox) *GrepTool { return &GrepTool{sb: sb} }
func (t *GrepTool) Name() string          { return "grep" }

func (t *GrepTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a GrepArgs
	json.Unmarshal(raw, &a)
	pat := a.Pattern
	if a.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	root := a.Root
	if root == "" {
		root = "."
	}
	resolved, err := t.sb.ResolvePath(root)
	if err != nil {
		return nil, err
	}
	max := a.MaxMatches
	if max == 0 {
		max = 1000
	}
	var out []GrepMatch
	filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if a.Glob != "" {
			matched, _ := filepath.Match(a.Glob, d.Name())
			if !matched {
				return nil
			}
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		rel, _ := filepath.Rel(resolved, p)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			if re.MatchString(sc.Text()) {
				out = append(out, GrepMatch{File: rel, Line: lineNo, Text: sc.Text()})
				if len(out) >= max {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return json.Marshal(GrepResult{Matches: out})
}
