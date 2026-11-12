package client

import "testing"

func TestFormatAndNormalizeCode(t *testing.T) {
	if got := formatCode("ABCDEFGHJKMN"); got != "ABCD-EFGH-JKMN" {
		t.Errorf("formatCode=%q", got)
	}
	// 显示格式与无分隔格式归一化后必须一致，help 端输入时 - 可有可无
	for _, in := range []string{"ABCD-EFGH-JKMN", "ABCDEFGHJKMN", "ABCD EFGH JKMN"} {
		if got := normalizeCode(in); got != "ABCDEFGHJKMN" {
			t.Errorf("normalizeCode(%q)=%q", in, got)
		}
	}
}
