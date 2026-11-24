package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRunHelpMCPStdoutContainsOnlyJSONRPC(t *testing.T) {
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdinReader.Close()
		stdinWriter.Close()
		t.Fatal(err)
	}

	originalStdin, originalStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinReader, stdoutWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout = originalStdin, originalStdout
		stdinReader.Close()
		stdinWriter.Close()
		stdoutReader.Close()
		stdoutWriter.Close()
	})

	output := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(stdoutReader)
		output <- data
		readErr <- err
	}()

	requests := []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n",
	)
	if _, err := stdinWriter.Write(requests); err != nil {
		t.Fatal(err)
	}
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}

	runHelp([]string{"--mcp-stdio", "--p2p", "disabled"})
	if err := stdoutWriter.Close(); err != nil {
		t.Fatal(err)
	}
	data := <-output
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}

	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("stdout returned %d non-empty lines, want 2 JSON-RPC responses: %q", len(lines), data)
	}
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("stdout contains a non-JSON MCP frame: %q", line)
		}
	}
}

// MCP 进程没写 --server、也没设 REMOTE_RELAY_SERVER 时是自动模式（spec 6.1），要接上节点列表：之后不带 server 的
// connect 按协助码找节点。接上时会读 REMOTE_RELAY_DIRECTORY，设成不合法的地址会提示一次，借此看出接没接上；
// stdin 一开始就关掉，不会去连 relay。
func TestRunHelpMCPAutoModeUsesNodeList(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"自动模式", []string{"--mcp-stdio", "--p2p", "disabled"}, true},
		{"写了 --server", []string{"--mcp-stdio", "--p2p", "disabled", "--server", "relay.example.test"}, false},
		{"--plain", []string{"--mcp-stdio", "--p2p", "disabled", "--plain"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REMOTE_RELAY_SERVER", "")
			t.Setenv("REMOTE_RELAY_DIRECTORY", "http://directory.example.test/v1/directory")
			stderr := runHelpCapturingStderr(t, tc.args)
			if got := strings.Contains(stderr, "REMOTE_RELAY_DIRECTORY"); got != tc.want {
				t.Fatalf("接上节点列表 = %v，想要 %v；stderr:\n%s", got, tc.want, stderr)
			}
		})
	}
}

// runHelpCapturingStderr 在 stdin 已关闭的情况下跑一次 runHelp，返回它写到 stderr 的内容。
func runHelpCapturingStderr(t *testing.T, args []string) string {
	t.Helper()
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdinWriter.Close()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin, originalStdout, originalStderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdinReader, stdoutWriter, stderrWriter
	t.Cleanup(func() {
		os.Stdin, os.Stdout, os.Stderr = originalStdin, originalStdout, originalStderr
		stdinReader.Close()
		stdoutReader.Close()
		stderrReader.Close()
	})
	go io.Copy(io.Discard, stdoutReader)
	stderrOut := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(stderrReader)
		stderrOut <- data
	}()

	runHelp(args)
	os.Stdin, os.Stdout, os.Stderr = originalStdin, originalStdout, originalStderr
	stdoutWriter.Close()
	stderrWriter.Close()
	return string(<-stderrOut)
}
