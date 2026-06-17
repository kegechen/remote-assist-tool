package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// countingErrCaller 记录 CallTool 调用次数并固定返回指定错误。
type countingErrCaller struct {
	calls int
	err   error
}

func (c *countingErrCaller) CallTool(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	c.calls++
	return nil, c.err
}

// callToolRetry 遇到 tunnel_lost / not_connected 这类不可恢复错误应立即返回、不重试，
// 避免在隧道已断时白白退避空转 ~6s。
func TestCallToolRetryStopsOnUnrecoverable(t *testing.T) {
	for _, msg := range []string{"tunnel_lost: peer gone", "rpc error: not_connected"} {
		c := &countingErrCaller{err: errors.New(msg)}
		if _, err := callToolRetry(context.Background(), c, "write_file", nil); err == nil {
			t.Fatalf("%q: expected error", msg)
		}
		if c.calls != 1 {
			t.Fatalf("%q: expected 1 call (no retry on unrecoverable), got %d", msg, c.calls)
		}
	}
}
