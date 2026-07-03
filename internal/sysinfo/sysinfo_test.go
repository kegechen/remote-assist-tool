package sysinfo

import (
	"runtime"
	"strings"
	"testing"
)

func TestSummaryShape(t *testing.T) {
	s := Summary()
	if s == "" {
		t.Fatal("Summary() returned empty string")
	}
	// 架构必须在末尾（osName 与 GOARCH 总会追加）。
	if !strings.HasSuffix(s, " "+runtime.GOARCH) {
		t.Errorf("Summary() = %q, want suffix %q", s, " "+runtime.GOARCH)
	}
	// 身份串是单行展示字段，不允许控制字符。
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			t.Errorf("Summary() contains control char %q in %q", r, s)
		}
	}
}

func TestOSNameNonEmpty(t *testing.T) {
	if osName() == "" {
		t.Error("osName() returned empty string")
	}
}
