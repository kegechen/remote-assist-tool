//go:build windows

package mcp

import (
	"context"
	"io"
	"testing"

	"golang.org/x/sys/windows"
)

func TestServeTreatsWindowsHostPipeClosureAsNormalShutdown(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "broken pipe", err: windows.ERROR_BROKEN_PIPE},
		{name: "no data", err: windows.ERROR_NO_DATA},
		{name: "pipe not connected", err: windows.ERROR_PIPE_NOT_CONNECTED},
		{name: "operation aborted", err: windows.ERROR_OPERATION_ABORTED},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := &terminalErrorReader{err: test.err}
			if err := NewServer(nil).Serve(context.Background(), input, io.Discard); err != nil {
				t.Fatalf("host pipe closure should be a normal shutdown: %v", err)
			}
		})
	}
}
