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
	File          string `json:"file"`
	Line          int    `json:"line"`
	Text          string `json:"text"`
	TextTruncated bool   `json:"text_truncated,omitempty"`
}

type GrepResult struct {
	Matches   []GrepMatch `json:"matches"`
	Truncated bool        `json:"truncated,omitempty"`
}

const grepMaxLineLen = 500 // 单行最长字符数（按字节截断，在 UTF-8 边界对齐）

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
		truncated := false
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
					text := sc.Text()
					textTrunc := false
					if len(text) > grepMaxLineLen {
						text = TruncateHead(text, grepMaxLineLen)
						textTrunc = true
					}
					out = append(out, GrepMatch{File: rel, Line: lineNo, Text: text, TextTruncated: textTrunc})
					if len(out) >= max {
						truncated = true
						return filepath.SkipAll
					}
				}
			}
			return nil
		})
		return json.Marshal(GrepResult{Matches: out, Truncated: truncated})
}
