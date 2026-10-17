package agent

import "encoding/json"

// TransferEvent 描述一次文件传输分块调用（upload_file / download_file 拆出来的
// write_file / read_file）。share 端据此把成百上千条逐块活动行聚合成一条进度。
type TransferEvent struct {
	Dir    string // "upload"（远端写入）或 "download"（远端读出）
	Path   string
	Offset int64 // 本块的起始偏移
	Bytes  int64 // 本块实际传输的字节数
	Total  int64 // 文件总大小；0 表示未知（旧版 help 端不带这个提示）
	Start  bool  // 从头上传前的 truncate 调用：一次新传输的开始，本身不含数据
	EOF    bool  // 下载读到了文件尾
}

// parseTransfer 判断一次成功的工具调用是不是传输分块，是则提取进度信息。
//
// 判据刻意收窄，避免把普通读写误算进进度：
//   - write_file 带 at：只有 upload_file 的并发分块会用绝对偏移写，旧版 help 端也是如此，
//     所以即便没有 total 也能认出来（只是算不出百分比）；
//   - write_file 不带 at、内容为空、带 total：upload_file 从头传前的 truncate；
//   - read_file 带 total：download_file 的分块。普通 read_file 不带 total，照常逐条显示。
//
// total 只是 help 端给的显示提示，不参与任何校验。
func parseTransfer(tool string, args, result json.RawMessage) (TransferEvent, bool) {
	switch tool {
	case "write_file":
		var a struct {
			Path    string          `json:"path"`
			Content json.RawMessage `json:"content"`
			At      *int64          `json:"at"`
			Total   int64           `json:"total"`
		}
		if json.Unmarshal(args, &a) != nil {
			return TransferEvent{}, false
		}
		n := base64JSONLen(a.Content)
		if a.At != nil {
			return TransferEvent{Dir: "upload", Path: a.Path, Offset: *a.At, Bytes: n, Total: positive(a.Total)}, true
		}
		if a.Total > 0 && n == 0 {
			return TransferEvent{Dir: "upload", Path: a.Path, Total: a.Total, Start: true}, true
		}
	case "read_file":
		var a struct {
			Path   string `json:"path"`
			Offset int64  `json:"offset"`
			Total  int64  `json:"total"`
		}
		if json.Unmarshal(args, &a) != nil || a.Total <= 0 {
			return TransferEvent{}, false
		}
		var r struct {
			Bytes json.RawMessage `json:"bytes"`
			EOF   bool            `json:"eof"`
		}
		if json.Unmarshal(result, &r) != nil {
			return TransferEvent{}, false
		}
		return TransferEvent{Dir: "download", Path: a.Path, Offset: a.Offset, Bytes: base64JSONLen(r.Bytes), Total: a.Total, EOF: r.EOF}, true
	}
	return TransferEvent{}, false
}

func positive(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// base64JSONLen 由 JSON 字符串里的标准 base64 文本算出解码后的字节数，免得为了
// 显示进度把每个 512 KiB 分块再完整解码一遍。非字符串（null 等）按 0 计。
func base64JSONLen(raw json.RawMessage) int64 {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0
	}
	s := raw[1 : len(raw)-1]
	n := int64(len(s)) / 4 * 3
	for i := len(s) - 1; i >= 0 && i >= len(s)-2 && s[i] == '='; i-- {
		n--
	}
	return n
}
