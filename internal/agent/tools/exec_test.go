package tools

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

type captureSink struct {
	stdout []byte
	stderr []byte
}

func (c *captureSink) Send(stream string, data []byte) error {
	if stream == "stdout" {
		c.stdout = append(c.stdout, data...)
	} else if stream == "stderr" {
		c.stderr = append(c.stderr, data...)
	}
	return nil
}

func TestExecSyncEchoes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/echo")
	}
	tool := NewExec(nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"/bin/echo", "hello"}})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	json.Unmarshal(out, &r)
	if r.ExitCode != 0 {
		t.Fatalf("exit=%d", r.ExitCode)
	}
	if string(r.Stdout) != "hello\n" {
		t.Fatalf("stdout=%q", r.Stdout)
	}
}


func TestExecTimeoutKills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	tool := NewExec(nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"/bin/sleep", "30"}, "timeout_ms": 200})
	start := time.Now()
	_, err := tool.Run(context.Background(), args, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("did not kill promptly: %v", elapsed)
	}
}

func TestExecEnvAppendsToParentEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	tool := NewExec(nil)
	args, _ := json.Marshal(map[string]any{
		"argv": []string{"/bin/sh", "-c", "echo PATH=$PATH CUSTOM=$CUSTOM"},
		"env":  map[string]string{"CUSTOM": "yes"},
	})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	json.Unmarshal(out, &r)
	s := string(r.Stdout)
	if !strings.Contains(s, "PATH=") || !strings.Contains(s, "CUSTOM=yes") {
		t.Fatalf("got %q", s)
	}
}
