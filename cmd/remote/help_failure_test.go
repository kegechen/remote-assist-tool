package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// 被 relay 封禁时 help 只打那一句话：不带「连接中断: …」的原始错误，也不建议重新运行
// 或重新 connect——封禁期内重试无效。
func TestHelpFailureTextBlocked(t *testing.T) {
	for _, mcp := range []bool{false, true} {
		err := fmt.Errorf("failed to connect: %w", proto.ErrBlocked)
		got := helpFailureText(err, mcp)
		if got != "\n"+proto.BlockedMessage+"\n" {
			t.Fatalf("mcp=%v: %q，期望只有 %q", mcp, got, proto.BlockedMessage)
		}
	}
}

// 其他错误保持原来的提示。
func TestHelpFailureTextOther(t *testing.T) {
	err := errors.New("EOF")
	if got := helpFailureText(err, false); !strings.Contains(got, "连接中断: EOF") || !strings.Contains(got, "请重新运行 remote help") {
		t.Fatalf("SSH 隧道模式提示被改了：%q", got)
	}
	if got := helpFailureText(err, true); !strings.Contains(got, "连接中断: EOF") || !strings.Contains(got, "重新调用 connect") {
		t.Fatalf("MCP 模式提示被改了：%q", got)
	}
}
