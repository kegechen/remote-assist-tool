//go:build !windows

package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const clipboardCommandTimeout = 5 * time.Second

func copyToClipboardPlatform(text string) error {
	spec, err := selectClipboardCommand(runtime.GOOS, func(name string) bool {
		_, lookupErr := exec.LookPath(name)
		return lookupErr == nil
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), clipboardCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, spec.name, spec.args...)
	if len(spec.env) > 0 {
		cmd.Env = append(os.Environ(), spec.env...)
	}
	cmd.Stdin = strings.NewReader(text) // Go 字符串按 UTF-8 原样写入 stdin。
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("clipboard command %s timed out: %w", spec.name, ctx.Err())
		}
		return fmt.Errorf("clipboard command %s failed: %w", spec.name, err)
	}
	return nil
}
