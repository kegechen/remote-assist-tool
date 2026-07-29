package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
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
