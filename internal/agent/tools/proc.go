package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/remote-assist/tool/internal/agent"
)

type ProcessListArgs struct {
	Filter string `json:"filter,omitempty"`
}

type ProcInfo struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	CmdLine string `json:"cmdline,omitempty"`
	User    string `json:"user,omitempty"`
}

type ProcessListResult struct{ Procs []ProcInfo `json:"procs"` }

type ProcessListTool struct{}

func NewProcessList() *ProcessListTool  { return &ProcessListTool{} }
func (t *ProcessListTool) Name() string { return "process_list" }

func (t *ProcessListTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a ProcessListArgs
	json.Unmarshal(raw, &a)
	var procs []ProcInfo
	if runtime.GOOS == "windows" {
		out, err := exec.CommandContext(ctx, "tasklist", "/FO", "CSV", "/NH").Output()
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := splitCSV(line)
			if len(fields) < 2 {
				continue
			}
			pid, _ := strconv.Atoi(fields[1])
			procs = append(procs, ProcInfo{PID: pid, Name: fields[0]})
		}
	} else {
		out, err := exec.CommandContext(ctx, "ps", "-eo", "pid=,user=,comm=,args=").Output()
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			parts := strings.Fields(line)
			if len(parts) < 3 {
				continue
			}
			pid, _ := strconv.Atoi(parts[0])
			procs = append(procs, ProcInfo{PID: pid, User: parts[1], Name: parts[2], CmdLine: strings.Join(parts[3:], " ")})
		}
	}
	if a.Filter != "" {
		filtered := procs[:0]
		for _, p := range procs {
			if strings.Contains(p.Name, a.Filter) || strings.Contains(p.CmdLine, a.Filter) {
				filtered = append(filtered, p)
			}
		}
		procs = filtered
	}
	return json.Marshal(ProcessListResult{Procs: procs})
}

func splitCSV(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
		case r == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	return out
}
