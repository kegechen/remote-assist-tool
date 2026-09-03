package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

// grepMaxFiles 单次 grep 最多真正打开扫描的文件数。
//
// 没有上限时，一句 root="/" 或者 root 落在 node_modules / .git 上方的 grep 会把整块盘
// 读一遍：max_matches 只在"匹配够了"时才刹得住车，模式压根匹配不上的时候它一点用都没有。
// 2 万个文件已经覆盖绝大多数真实代码库，超出的部分按截断上报，调用方能看出结果不全。
//
// 用 var 而非 const 只为让测试调到个位数；运行期不改。
var grepMaxFiles = 20000

// grepCtxCheckLines 每扫多少行回头看一眼 ctx。单个巨型文件（日志、打包产物）能顶着
// 一次回调跑很久，只在文件粒度上检查取消是不够的。
const grepCtxCheckLines = 4096

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
	scanned := 0
	// canceled 单独记：WalkDir 只认 SkipAll/SkipDir，把 ctx.Err() 直接返回会被当成
	// "遍历失败"吞掉，分不清是取消还是别的。
	var canceled error
	filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// 调用方已经不等了（工具调用超时、隧道断了、GUI 关了）就立刻收工。一次覆盖大树的
		// grep 能跑好几分钟，结果早已无人接收，白白占着被协助端的 CPU 和磁盘。
		if err := ctx.Err(); err != nil {
			canceled = err
			return filepath.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		if a.Glob != "" {
			matched, _ := filepath.Match(a.Glob, d.Name())
			if !matched {
				return nil
			}
		}
		if !grepScannable(p, d) {
			return nil
		}
		if scanned >= grepMaxFiles {
			truncated = true
			return filepath.SkipAll
		}
		scanned++
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
			if lineNo%grepCtxCheckLines == 0 {
				if err := ctx.Err(); err != nil {
					canceled = err
					return filepath.SkipAll
				}
			}
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
	if canceled != nil {
		// 宁可报错也不把半截结果当完整的交出去：调用方看到 truncated 只会理解成
		// "命中太多"，没法区分"根本没搜完"。
		return nil, fmt.Errorf("grep 已取消: %w", canceled)
	}
	return json.Marshal(GrepResult{Matches: out, Truncated: truncated})
}

// grepScannable 判断这个条目能不能当普通文本文件安全地读。
//
// WalkDir 会把 FIFO、字符设备、unix socket 一视同仁地交上来，而 os.Open 一个没有写端的
// FIFO 会一直阻塞——整次 grep 就此挂死，连回调开头的 ctx 检查都轮不到（它卡在 Open 里，
// 根本回不到循环）。字符设备更糟：/dev/zero、/dev/urandom 能无穷无尽地喂数据。
//
// 符号链接单独放行：WalkDir 用 Lstat，指向普通源码文件的软链在这里显示为 ModeSymlink，
// 一刀切掉会让"源码目录里有软链"这种常见布局搜不到东西。跟一次 Stat 确认落点是普通文件
// 即可，顺带也挡住了指向 FIFO / 设备的软链。
func grepScannable(path string, d fs.DirEntry) bool {
	mode := d.Type()
	if mode.IsRegular() {
		return true
	}
	if mode&fs.ModeSymlink == 0 {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
