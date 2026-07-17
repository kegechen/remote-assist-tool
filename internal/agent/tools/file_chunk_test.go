package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

// read_file 靠源头限量而不是事后截断文本，为的是 bytes_len/eof 与返回内容严格一致，
// 调用方能靠 offset += bytes_len 一路读到 eof=true 拿到完整文件。
// 这是「省 token 不能挡住定位问题」的底线：大文件必须仍然读得全。
func TestReadFileChunkedReadRecoversWholeFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big.log")

	// 200 KiB，带中文以便顺带覆盖多字节字符跨块边界的情况
	var want bytes.Buffer
	for want.Len() < 200*1024 {
		want.WriteString("排查日志 line with 中文 padding 0123456789\n")
	}
	if err := os.WriteFile(p, want.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewReadFile(sb)

	var got bytes.Buffer
	offset := 0
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("续读没有收敛，可能 eof 永远不为 true")
		}
		args, _ := json.Marshal(map[string]any{"path": p, "offset": offset})
		out, err := tool.Run(context.Background(), args, nil)
		if err != nil {
			t.Fatalf("offset=%d 读失败: %v", offset, err)
		}
		var r ReadFileResult
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatal(err)
		}
		got.Write(r.Bytes)
		// 调用方唯一能依赖的续读依据：本次实际返回的字节数
		offset += len(r.Bytes)
		if r.EOF {
			break
		}
		if len(r.Bytes) == 0 {
			t.Fatal("未到 eof 却返回 0 字节，续读会死循环")
		}
	}

	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Errorf("续读拿到 %d 字节，原文件 %d 字节——内容不完整", got.Len(), want.Len())
	}
}

// 未传 length 时用 64 KiB 默认块，避免一次把 1 MiB 塞进 context。
func TestReadFileDefaultChunkSize(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 300*1024), 0644); err != nil {
		t.Fatal(err)
	}
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewReadFile(sb)

	args, _ := json.Marshal(map[string]any{"path": p})
	out, _ := tool.Run(context.Background(), args, nil)
	var r ReadFileResult
	json.Unmarshal(out, &r)

	if len(r.Bytes) != readFileDefaultChunk {
		t.Errorf("默认块应为 %d 字节，得到 %d", readFileDefaultChunk, len(r.Bytes))
	}
	if r.EOF {
		t.Error("文件还没读完，eof 不应为 true——否则调用方会以为拿全了")
	}
}

// download_file 显式传 length=512 KiB，默认块大小不能影响它。
func TestReadFileExplicitLengthHonored(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 700*1024), 0644); err != nil {
		t.Fatal(err)
	}
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewReadFile(sb)

	args, _ := json.Marshal(map[string]any{"path": p, "length": 512 * 1024})
	out, _ := tool.Run(context.Background(), args, nil)
	var r ReadFileResult
	json.Unmarshal(out, &r)

	if len(r.Bytes) != 512*1024 {
		t.Errorf("显式 length 应被采纳，期望 %d 字节，得到 %d", 512*1024, len(r.Bytes))
	}
}

// write_file 的 content 字段是 base64：Go 端类型是 []byte，encoding/json 按 base64 解码。
// 客户端（GUI、脚本）直接发明文是 bug——多数会报 illegal base64，但恰好落在 base64
// 字母表且长度为 4 倍数的短内容会被静默解成垃圾字节写进文件，无声损坏用户数据。
// 这个测试把该契约钉住：改动它就要同步改所有调用方的编码。
func TestWriteFileContentIsBase64(t *testing.T) {
	root := t.TempDir()
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewWriteFile(sb)

	p := filepath.Join(root, "ok.txt")
	args, _ := json.Marshal(map[string]any{"path": p, "content": "5Lit5paH", "create": true}) // base64("中文")
	if _, err := tool.Run(context.Background(), args, nil); err != nil {
		t.Fatalf("base64 content 应被接受: %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "中文" {
		t.Errorf("base64 应被解码成原文，得到 %q", got)
	}

	// 明文 "test" 恰好是合法 base64，不会报错但会解出垃圾——正因如此前端必须显式编码
	p2 := filepath.Join(root, "trap.txt")
	args2, _ := json.Marshal(map[string]any{"path": p2, "content": "test", "create": true})
	if _, err := tool.Run(context.Background(), args2, nil); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got, _ := os.ReadFile(p2); string(got) == "test" {
		t.Error("content 被当成明文写入了——契约已变，所有调用方的 base64 编码需同步调整")
	}
}

// 显式 length 也不能突破 1 MiB 硬上限。
func TestReadFileLengthCappedAtMax(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "huge.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 3*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewReadFile(sb)

	args, _ := json.Marshal(map[string]any{"path": p, "length": 8 * 1024 * 1024})
	out, _ := tool.Run(context.Background(), args, nil)
	var r ReadFileResult
	json.Unmarshal(out, &r)

	if len(r.Bytes) != readFileMaxChunk {
		t.Errorf("length 应被压到 %d 字节上限，得到 %d", readFileMaxChunk, len(r.Bytes))
	}
}
