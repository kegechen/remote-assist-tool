package mcp

import "encoding/json"

// Schema 单个工具的 MCP description+inputSchema
type Schema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// AllSchemas 返回 9 个工具的固定 schema
func AllSchemas() []Schema {
	return []Schema{
		{Name: "exec", Description: "Run a command via argv (no shell) on the remote. Stream mode is not supported in v1; output is returned as a single ExecResult after completion.", InputSchema: json.RawMessage(`{"type":"object","required":["argv"],"properties":{"argv":{"type":"array","items":{"type":"string"}},"cwd":{"type":"string"},"timeout_ms":{"type":"integer"}}}`)},
		{Name: "read_file", Description: "Read a remote file.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer"},"length":{"type":"integer"}}}`)},
		{Name: "write_file", Description: "Write/overwrite a remote file.", InputSchema: json.RawMessage(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string","contentEncoding":"base64"},"append":{"type":"boolean"}}}`)},
		{Name: "list_dir", Description: "List a remote directory.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"glob":{"type":"string"}}}`)},
		{Name: "stat", Description: "Stat a remote path.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)},
		{Name: "glob", Description: "Glob remote files.", InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"root":{"type":"string"}}}`)},
		{Name: "grep", Description: "Regex search remote files.", InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"root":{"type":"string"},"glob":{"type":"string"},"ignore_case":{"type":"boolean"}}}`)},
		{Name: "process_list", Description: "List remote processes.", InputSchema: json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string"}}}`)},
		{Name: "tail_log", Description: "Read the last N lines of a remote log file (default 100). Follow mode is not supported in v1.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"lines":{"type":"integer"}}}`)},
	}
}
