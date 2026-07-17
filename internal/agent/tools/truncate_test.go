package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 截断必须在 UTF-8 边界对齐：旧实现会在多字节字符的 lead byte 处停下并把它保留，
// 切出 "你\xe5" 这种非法序列，中文输出一截断就变乱码。
func TestTruncateNeverSplitsRune(t *testing.T) {
	inputs := []string{"你好世界", "abc你好", "日本語テスト", "🎉🎉🎉", "a🎉b"}
	for _, s := range inputs {
		for max := 0; max <= len(s)+2; max++ {
			if got := TruncateHead(s, max); !utf8.ValidString(got) {
				t.Errorf("TruncateHead(%q,%d) 产生非法 UTF-8: % x", s, max, []byte(got))
			}
			if got := TruncateTail(s, max); !utf8.ValidString(got) {
				t.Errorf("TruncateTail(%q,%d) 产生非法 UTF-8: % x", s, max, []byte(got))
			}
		}
	}
}

func TestTruncateHeadKeepsPrefix(t *testing.T) {
	if got := TruncateHead("你好世界", 4); got != "你" {
		t.Errorf("TruncateHead 应保留能放下的最长前缀，得到 %q", got)
	}
	if got := TruncateHead("abc", 10); got != "abc" {
		t.Errorf("未超限时应原样返回，得到 %q", got)
	}
}

// tail_log 的语义是看最新内容，截断必须丢头部而不是尾部。
func TestTruncateTailKeepsNewest(t *testing.T) {
	if got := TruncateTail("你好世界", 4); got != "界" {
		t.Errorf("TruncateTail 应保留能放下的最长后缀，得到 %q", got)
	}
	log := "old line\nmid line\nNEWEST line"
	got := TruncateTail(log, 11)
	if !strings.Contains(got, "NEWEST") {
		t.Errorf("TruncateTail 丢掉了最新内容: %q", got)
	}
}

// exec 的报错/堆栈几乎总在末尾，截断必须同时保住头和尾。
func TestTruncateMiddleKeepsHeadAndTail(t *testing.T) {
	s := "HEAD_MARKER" + strings.Repeat("x", 5000) + "TAIL_MARKER"
	got, truncated := TruncateMiddle(s, 200)
	if !truncated {
		t.Fatal("超限时应报告已截断")
	}
	if len(got) > 200 {
		t.Errorf("截断结果 %d 字节，超过上限 200", len(got))
	}
	if !strings.Contains(got, "HEAD_MARKER") {
		t.Errorf("丢掉了头部: %q", got)
	}
	if !strings.Contains(got, "TAIL_MARKER") {
		t.Errorf("丢掉了尾部（报错通常在这里）: %q", got)
	}
	if !strings.Contains(got, "max_output_bytes") {
		t.Errorf("省略标记应告诉调用方如何拿到完整输出: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("产生非法 UTF-8: % x", []byte(got))
	}
}

func TestTruncateMiddleNoopWhenUnderLimit(t *testing.T) {
	s := "short output"
	got, truncated := TruncateMiddle(s, 1000)
	if truncated || got != s {
		t.Errorf("未超限时应原样返回且不标截断，得到 %q trunc=%v", got, truncated)
	}
}

// max 小到放不下省略标记时不能把标记本身撑爆预算。
func TestTruncateMiddleTinyMax(t *testing.T) {
	s := strings.Repeat("x", 1000)
	got, truncated := TruncateMiddle(s, 10)
	if !truncated {
		t.Fatal("应报告已截断")
	}
	if len(got) > 10 {
		t.Errorf("截断结果 %d 字节，超过上限 10", len(got))
	}
}
