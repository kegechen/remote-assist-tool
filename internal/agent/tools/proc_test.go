package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestProcessListIncludesCurrentProcess(t *testing.T) {
	tool := NewProcessList()
	args, _ := json.Marshal(map[string]any{})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ProcessListResult
	json.Unmarshal(out, &r)
	if len(r.Procs) == 0 {
		t.Fatal("expected at least one process")
	}
}
