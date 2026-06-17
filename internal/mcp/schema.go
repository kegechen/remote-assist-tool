package mcp

import "encoding/json"

// Schema 单个工具的 MCP description+inputSchema
type Schema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// AllSchemas 返回 MCP 暴露的全部工具 schema：
//   - connect：本地处理（启动隧道）
//   - 9 个透传工具：exec / read_file / write_file / list_dir / stat / glob / grep / process_list / tail_log
//     由 share 端实现，host 端 bridge 透传
//   - 2 个 host 端复合工具：upload_file / download_file
//     循环调用 read_file / write_file 协议，share 端零改动
func AllSchemas() []Schema {
	return []Schema{
		{Name: "connect", Description: "Pair with a remote share by providing the assist code. Must be called first before any other tool. The other tools will return 'not_connected' until this succeeds. Optionally override the relay server address (server='host:port') when the user says the share is on a specific LAN address — this is how 'share --standalone' is used. Set no_auth=true to connect without a code when the share was started with --no-auth (trusted LAN only).", InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string","description":"Assist code from the share side, e.g. 'ABCD-EFGHIJ' or 'ABCDEFGHIJ' (hyphen optional). Not required when no_auth=true."},"server":{"type":"string","description":"Optional relay address override (host:port). Use when the user says the share is at a specific LAN IP, e.g. '192.168.137.13:8443'. Skips the default relay in .mcp.json."},"no_auth":{"type":"boolean","description":"Set true to connect without an assist code. The share side must have been started with --no-auth. DANGER: no authentication — use only on trusted private LANs."}}}`)},
		{Name: "exec", Description: "Run a command via argv (no shell) on the remote. Stream mode is not supported in v1; output is returned as a single ExecResult after completion.", InputSchema: json.RawMessage(`{"type":"object","required":["argv"],"properties":{"argv":{"type":"array","items":{"type":"string"}},"cwd":{"type":"string"},"timeout_ms":{"type":"integer"}}}`)},
		{Name: "read_file", Description: "Read a remote file. Returns up to 1 MiB per call; set offset to read further chunks until eof=true.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer"},"length":{"type":"integer"}}}`)},
		{Name: "write_file", Description: "Write/overwrite a remote file. Set create=true to allow creating a new file when it does not exist (default false → only writes existing files). Set append=true to append; default truncates. upload_file relies on this contract.", InputSchema: json.RawMessage(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string","contentEncoding":"base64"},"create":{"type":"boolean","description":"Create the file if it does not exist. Default false."},"append":{"type":"boolean","description":"Append to existing content instead of truncating. Default false."}}}`)},
		{Name: "list_dir", Description: "List a remote directory.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"glob":{"type":"string"}}}`)},
		{Name: "stat", Description: "Stat a remote path.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)},
		{Name: "glob", Description: "Glob remote files.", InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"root":{"type":"string"}}}`)},
		{Name: "grep", Description: "Regex search remote files.", InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"root":{"type":"string"},"glob":{"type":"string"},"ignore_case":{"type":"boolean"}}}`)},
		{Name: "process_list", Description: "List remote processes.", InputSchema: json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string"}}}`)},
		{Name: "tail_log", Description: "Read the last N lines of a remote log file (default 100). Follow mode is not supported in v1.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"lines":{"type":"integer"}}}`)},
		{Name: "upload_file", Description: "Push a local (help-side) file to the remote (share-side) machine in 512 KiB chunks. Use this for binary blobs > a few KiB — DLL/EXE replacement, deb/zip uploads, etc. Implemented host-side via repeated write_file calls; share-side runtime needs no change. Each chunk auto-retries; on hard failure the error reports the byte offset — reconnect and re-call with offset=<bytes> to resume.", InputSchema: json.RawMessage(`{"type":"object","required":["local_path","remote_path"],"properties":{"local_path":{"type":"string","description":"Absolute path on the help-side (local) machine."},"remote_path":{"type":"string","description":"Absolute path on the remote share-side machine. Will be created/overwritten."},"offset":{"type":"integer","description":"Resume byte offset: skip the first N bytes (local seeked, remote appended). Default 0 = full upload from scratch."}}}`)},
		{Name: "download_file", Description: "Pull a remote (share-side) file to the help-side (local) machine in 512 KiB chunks. Use this for crash dumps, log archives, or any blob > a few KiB you want as a real local file rather than as base64 in your context window. Implemented host-side via repeated read_file calls; share-side runtime needs no change. Each chunk auto-retries; on hard failure the error reports the byte offset — reconnect and re-call with offset=<bytes> to resume.", InputSchema: json.RawMessage(`{"type":"object","required":["remote_path","local_path"],"properties":{"remote_path":{"type":"string","description":"Absolute path on the remote share-side machine."},"local_path":{"type":"string","description":"Absolute path on the help-side (local) machine. Will be created/overwritten."},"offset":{"type":"integer","description":"Resume byte offset: skip the first N bytes (local seeked, remote read from offset). Default 0 = full download from scratch."}}}`)},
	}
}
