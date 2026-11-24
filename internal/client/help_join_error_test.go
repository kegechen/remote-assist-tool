package client

import (
	"errors"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

// TestJoinFailureErrorMapping 验证 join 失败文案映射：
// relay 下发同源占用标记时给专用文案，其余统一给并列原因提示。
func TestJoinFailureErrorMapping(t *testing.T) {
	if err := joinFailureError(proto.ErrJoinSessionBusySameAddr); !errors.Is(err, ErrSessionBusySameAddr) {
		t.Fatalf("同源占用应映射为 ErrSessionBusySameAddr，实际 %v", err)
	}
	// 封禁最先判：不能被归进 ErrJoinFailed，否则会提示用户去核对协助码。
	if err := joinFailureError(proto.BlockedMessage); !errors.Is(err, proto.ErrBlocked) || errors.Is(err, ErrJoinFailed) {
		t.Fatalf("封禁应映射为 proto.ErrBlocked，实际 %v", err)
	}
	for _, raw := range []string{"join failed", "", "something else"} {
		err := joinFailureError(raw)
		if !errors.Is(err, ErrJoinFailed) {
			t.Fatalf("普通失败 %q 应映射为 ErrJoinFailed，实际 %v", raw, err)
		}
		if !strings.Contains(err.Error(), "已有另一个协助端") {
			t.Fatalf("普通失败文案应提示可能已有协助端在线，实际 %q", err.Error())
		}
	}
}
