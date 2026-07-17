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
		{Name: "connect", Description: "Pair with a remote share by providing the assist code. Must be called first before any other tool.", InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string","description":"Assist code from the share side, e.g. 'ABCD-EFGHIJ' or 'ABCDEFGHIJ'. Not required when no_auth=true."},"server":{"type":"string","description":"Optional relay address override (host:port) for LAN direct connect."},"no_auth":{"type":"boolean","description":"Connect without code when share was started with --no-auth (trusted LAN only)."}}}`)},
		{Name: "exec", Description: "Run a command via argv (NO shell) on the remote. argv[0] must be a real executable — shell builtins and shell syntax do NOT work: 'pwd'/'ls'/'cd'/'dir', pipes, redirects, globs, env-var expansion all fail with error set and exit_code -1. For those, invoke a shell explicitly: argv=[\"powershell\",\"-NoProfile\",\"-Command\",\"...\"] on Windows or argv=[\"sh\",\"-c\",\"...\"] on Unix. If a command fails to start, the 'error' field says why (executable not found, permission denied, ...). Output is capped at 32 KiB per stream by default, keeping the head AND tail with the middle elided (errors and stack traces are usually at the end), flagged with stdout_truncated/stderr_truncated; raise max_output_bytes for the full output.", InputSchema: json.RawMessage(`{"type":"object","required":["argv"],"properties":{"argv":{"type":"array","items":{"type":"string"}},"cwd":{"type":"string"},"timeout_ms":{"type":"integer"},"max_output_bytes":{"type":"integer","description":"Max bytes per stream (stdout/stderr). Default 32768. Raise this to get the full output of a command whose middle matters."}}}`)},
		{Name: "read_file", Description: "Read a remote file. Returns 64 KiB per call by default. bytes_len and eof always describe exactly what was returned — never a truncated view — so you can read a whole file by looping: offset += bytes_len until eof=true. Set length for a larger chunk (max 1 MiB per call).", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"Byte offset to start reading from. To continue a read, pass the previous offset plus the previous bytes_len."},"length":{"type":"integer","description":"Bytes to read this call. Default 65536, max 1048576."}}}`)},
		{Name: "write_file", Description: "Write/overwrite a remote file. Set create=true to allow creating a new file. Set append=true to append; default truncates.", InputSchema: json.RawMessage(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string","contentEncoding":"base64"},"create":{"type":"boolean","description":"Create file if it does not exist. Default false."},"append":{"type":"boolean","description":"Append instead of truncate. Default false."}}}`)},
		{Name: "list_dir", Description: "List a remote directory. Capped at 200 entries by default; set max_entries to raise.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"glob":{"type":"string"},"max_entries":{"type":"integer","description":"Max entries to return. Default 200."}}}`)},
		{Name: "stat", Description: "Stat a remote path.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)},
		{Name: "glob", Description: "Glob remote files.", InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"root":{"type":"string"}}}`)},
		{Name: "grep", Description: "Regex search remote files. Lines capped at 500 chars; max 1000 matches.", InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"root":{"type":"string"},"glob":{"type":"string"},"ignore_case":{"type":"boolean"},"max_matches":{"type":"integer","description":"Max matches. Default 1000."}}}`)},
		{Name: "process_list", Description: "List remote processes. Capped at 50 by default; set max_count to raise.", InputSchema: json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string"},"max_count":{"type":"integer","description":"Max processes to return. Default 50."}}}`)},
		{Name: "tail_log", Description: "Read the last N lines of a remote log file (default 100). Content is returned in the 'lines' field, capped at 64 KiB keeping the NEWEST content (lines_truncated flags this); ask for fewer lines if you hit the cap.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"lines":{"type":"integer"}}}`)},
		{Name: "upload_file", Description: "Push a local file to the remote machine in 512 KiB chunks, auto-retry. For binary blobs > a few KiB. Resume with offset=<bytes> on failure.", InputSchema: json.RawMessage(`{"type":"object","required":["local_path","remote_path"],"properties":{"local_path":{"type":"string","description":"Absolute path on the help-side (local) machine."},"remote_path":{"type":"string","description":"Absolute path on the remote share-side machine."},"offset":{"type":"integer","description":"Resume byte offset. Default 0."}}}`)},
		{Name: "download_file", Description: "Pull a remote file to the local machine in 512 KiB chunks, auto-retry. For crash dumps, log archives, etc. Resume with offset=<bytes> on failure.", InputSchema: json.RawMessage(`{"type":"object","required":["remote_path","local_path"],"properties":{"remote_path":{"type":"string","description":"Absolute path on the remote share-side machine."},"local_path":{"type":"string","description":"Absolute path on the help-side (local) machine."},"offset":{"type":"integer","description":"Resume byte offset. Default 0."}}}`)},
	}
}
