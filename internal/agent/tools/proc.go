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
	Filter   string `json:"filter,omitempty"`
	MaxCount int    `json:"max_count,omitempty"`
}

type ProcInfo struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	CmdLine string `json:"cmdline,omitempty"`
	User    string `json:"user,omitempty"`
}

type ProcessListResult struct {
	Procs     []ProcInfo `json:"procs"`
	Total     int        `json:"total,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

const defaultProcessMaxCount = 50

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
		// 注：ps 输出本身已把 args 内的多空格压缩为单空格，cmdline 字段无法 100% 还原原始值；v1 acceptable。
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

	maxCount := a.MaxCount
	if maxCount <= 0 {
		maxCount = defaultProcessMaxCount
	}
	total := len(procs)
	truncated := false
	if len(procs) > maxCount {
		procs = procs[:maxCount]
		truncated = true
	}
	return json.Marshal(ProcessListResult{Procs: procs, Total: total, Truncated: truncated})
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
