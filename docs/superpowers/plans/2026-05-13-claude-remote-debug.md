# Claude Code 远程调试通道 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在现有 relay + P2P + 协助码 基础上新增一条结构化"工具通道"，let local Claude Code 通过 `remote help --mcp-stdio`（本地 MCP server）对远端 `remote share` 进行 exec / 文件 / 进程 / 日志 等操作，取代 SSH 在远程调试场景的角色。

**Architecture:** Share 端启动工具 daemon（9 个工具 + 沙箱）。Help 端启动 MCP stdio server，Claude Code 通过 `.mcp.json` 拉起。新工具消息（`MsgToolReq` / `MsgToolResp` / `MsgStreamChunk` / `MsgCancel`）作为顶层 `proto.Message.Type` 走现有 relay 连接，与 `MsgTunnelData`（SSH 流）并存。消息 payload 用 session_key（HKDF(协助码‖nonce_share‖nonce_help)）做 XChaCha20-Poly1305 AEAD。MVP 工具通道走 relay，P2P 仍服务 SSH 旧用法。

**Tech Stack:** Go 1.21+, JSON 顶层 proto，`golang.org/x/crypto/chacha20poly1305` (AEAD), `golang.org/x/crypto/hkdf` (KDF), `github.com/fsnotify/fsnotify` (tail_log follow)，最小自实现 MCP JSON-RPC subset（initialize / tools/list / tools/call / notifications/cancelled），现有 `internal/proto` `internal/client` `internal/logger/audit`。

**Spec：** `docs/superpowers/specs/2026-05-13-claude-remote-debug-design.md`

---

## File Structure

新增（每个文件一个明确职责）：

| 文件 | 职责 |
|---|---|
| `internal/proto/tool.go` | 6 个新 MessageType 常量 + ToolReq/ToolResp/StreamChunk/Cancel/Hello/HelloAck struct |
| `internal/proto/aead.go` | XChaCha20-Poly1305 包装：Seal/Open helpers |
| `internal/proto/handshake.go` | HKDF 派生 session_key + 握手状态机 |
| `internal/agent/agent.go` | Tool 接口 + Registry + Dispatcher（接收 ToolReq、调用具体 tool、回 ToolResp/StreamChunk） |
| `internal/agent/sandbox.go` | --root 路径校验 + --allow/deny-exec 策略 |
| `internal/agent/tools/exec.go` | `exec` 工具（同步 + 流式） |
| `internal/agent/tools/file.go` | `read_file` / `write_file` |
| `internal/agent/tools/fs.go` | `list_dir` / `stat` / `glob` |
| `internal/agent/tools/grep.go` | `grep` |
| `internal/agent/tools/proc.go` | `process_list` |
| `internal/agent/tools/log.go` | `tail_log` |
| `internal/agent/elevate_windows.go` | Windows ShellExecuteW runas（build tag: windows） |
| `internal/agent/elevate_other.go` | 非 Windows stub（build tag: !windows） |
| `internal/mcp/server.go` | MCP stdio JSON-RPC server（initialize/tools/list/tools/call/notifications） |
| `internal/mcp/bridge.go` | MCP tools/call ↔ ToolReq 双向翻译 + 流式 progress |
| `internal/mcp/schema.go` | 9 个工具的 JSON Schema（与 agent 同源） |

修改：

| 文件 | 修改内容 |
|---|---|
| `internal/proto/message.go` | 加 4 个 MessageType 常量（沿用现有 const 风格） |
| `internal/client/share.go` | 在 `handleTunnel` / `handleTunnelP2P` 的消息分发处增加 ToolReq/Cancel 分支转给 agent；启动时拉起 agent.Daemon |
| `internal/client/help.go` | 当 `--mcp-stdio` 设置时，跳过 TCP listener，启动 MCP server；分发处增加 ToolResp/StreamChunk 分支送给 MCP bridge |
| `cmd/remote/main.go` | share 加 `--root --allow-exec --deny-exec --elevate --unsafe-full-system`；help 加 `--mcp-stdio --mcp-port --legacy-ssh` |
| `README.md` | 加 "Claude Code 远程调试" 一节 |

---

## Task 1: 新增工具消息常量与结构体

**Files:**
- Modify: `internal/proto/message.go` (add 4 constants near existing const block)
- Create: `internal/proto/tool.go`
- Test: `internal/proto/tool_test.go`

- [ ] **Step 1: 写失败测试 `internal/proto/tool_test.go`**

```go
package proto

import (
	"encoding/json"
	"testing"
)

func TestToolReqRoundtrip(t *testing.T) {
	req := ToolReq{
		ID:         42,
		Tool:       "exec",
		ArgsJSON:   json.RawMessage(`{"argv":["ls"]}`),
		DeadlineMs: 5000,
	}
	msg, err := NewMessage(MsgToolReq, &req)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	raw, _ := json.Marshal(msg)
	parsed, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if parsed.Type != MsgToolReq {
		t.Fatalf("type mismatch: %s", parsed.Type)
	}
	var got ToolReq
	if err := DecodePayload(parsed, &got); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if got.ID != 42 || got.Tool != "exec" || got.DeadlineMs != 5000 {
		t.Fatalf("got %+v", got)
	}
}

func TestStreamChunkFinMarker(t *testing.T) {
	c := StreamChunk{ID: 1, Seq: 7, Fin: true, Data: []byte("hello")}
	msg, _ := NewMessage(MsgStreamChunk, &c)
	raw, _ := json.Marshal(msg)
	parsed, _ := ParseMessage(raw)
	var got StreamChunk
	DecodePayload(parsed, &got)
	if !got.Fin || got.Seq != 7 || string(got.Data) != "hello" {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/proto/ -run TestTool -v`
Expected: FAIL（`undefined: ToolReq` 等）

- [ ] **Step 3: 实现 `internal/proto/tool.go`**

```go
package proto

import "encoding/json"

// ToolReq Claude Code 通过 help 端发起的工具调用请求
type ToolReq struct {
	ID         uint64          `json:"id"`
	Tool       string          `json:"tool"`
	ArgsJSON   json.RawMessage `json:"args"`        // 已 AEAD 解密后的工具参数 JSON
	DeadlineMs uint32          `json:"deadline_ms"` // 0 = 工具默认
}

// ToolResp share 端处理完的应答（或流的终止帧）
type ToolResp struct {
	ID         uint64          `json:"id"`
	OK         bool            `json:"ok"`
	ResultJSON json.RawMessage `json:"result,omitempty"`
	ErrorCode  string          `json:"error_code,omitempty"`
	ErrorMsg   string          `json:"error_msg,omitempty"`
}

// StreamChunk exec stream=true / tail_log follow / 大文件分块
type StreamChunk struct {
	ID     uint64 `json:"id"`
	Seq    uint32 `json:"seq"`
	Fin    bool   `json:"fin"`
	Stream string `json:"stream,omitempty"` // "stdout" | "stderr" | "" (binary)
	Data   []byte `json:"data,omitempty"`
}

// Cancel 取消指定 in-flight 请求
type Cancel struct {
	ID     uint64 `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// Hello / HelloAck 工具通道版本与能力协商
type Hello struct {
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	NonceB64     string   `json:"nonce_b64"` // base64(16 random bytes)
}

type HelloAck struct {
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	NonceB64     string   `json:"nonce_b64"`
	Accept       bool     `json:"accept"`
	ErrorMsg     string   `json:"error_msg,omitempty"`
}
```

并在 `internal/proto/message.go` 现有 const 块尾部加：

```go
	// Claude Code 工具通道
	MsgToolHello     MessageType = "tool_hello"
	MsgToolHelloAck  MessageType = "tool_hello_ack"
	MsgToolReq       MessageType = "tool_req"
	MsgToolResp      MessageType = "tool_resp"
	MsgToolStream    MessageType = "tool_stream"
	MsgToolCancel    MessageType = "tool_cancel"
```

注意：常量名是 `MsgToolReq` 但 `tool_test.go` 也用此名，要核对一致。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/proto/ -v`
Expected: 全部 PASS（原有测试也不应破坏）

- [ ] **Step 5: 提交**

```powershell
git add internal/proto/message.go internal/proto/tool.go internal/proto/tool_test.go
git commit -m @'
feat(proto): 添加 Claude Code 工具通道消息类型

新增 MsgToolHello/HelloAck/Req/Resp/Stream/Cancel 6 个 MessageType
常量，以及对应的 ToolReq/ToolResp/StreamChunk/Cancel/Hello/HelloAck
结构体。沿用现有 JSON proto 包装风格，不影响既有 MsgTunnelData
SSH 流。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 2: AEAD 包装（XChaCha20-Poly1305）

**Files:**
- Create: `internal/proto/aead.go`
- Test: `internal/proto/aead_test.go`

- [ ] **Step 1: 写失败测试**

```go
package proto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestAEADSealOpenRoundtrip(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	plain := []byte(`{"argv":["ls","-l"]}`)
	ct, err := AEADSeal(&key, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	out, err := AEADOpen(&key, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("roundtrip mismatch: got %s", out)
	}
}

func TestAEADOpenRejectsTamper(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	ct, _ := AEADSeal(&key, []byte("hello"))
	ct[len(ct)-1] ^= 0x01
	if _, err := AEADOpen(&key, ct); err == nil {
		t.Fatal("expected open to fail on tampered ciphertext")
	}
}

func TestAEADOpenRejectsWrongKey(t *testing.T) {
	var k1, k2 [32]byte
	rand.Read(k1[:])
	rand.Read(k2[:])
	ct, _ := AEADSeal(&k1, []byte("hello"))
	if _, err := AEADOpen(&k2, ct); err == nil {
		t.Fatal("expected open to fail under wrong key")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/proto/ -run TestAEAD -v`
Expected: FAIL `undefined: AEADSeal`

- [ ] **Step 3: 实现 `internal/proto/aead.go`**

```go
package proto

import (
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// AEADSeal 用 32 字节 key 对明文做 XChaCha20-Poly1305 加密。
// 返回的密文格式：[24B nonce][ct+tag]
func AEADSeal(key *[32]byte, plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("new aead: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("rand nonce: %w", err)
	}
	out := append([]byte{}, nonce...)
	out = aead.Seal(out, nonce, plain, nil)
	return out, nil
}

// AEADOpen 解密 AEADSeal 输出的密文。
func AEADOpen(key *[32]byte, ct []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("new aead: %w", err)
	}
	if len(ct) < aead.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce := ct[:aead.NonceSize()]
	body := ct[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("aead open: %w", err)
	}
	return plain, nil
}
```

`go.mod` 已经间接依赖 `golang.org/x/crypto`（项目用了 TLS）；如未引入：

```powershell
go get golang.org/x/crypto/chacha20poly1305
go mod tidy
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/proto/ -v -run TestAEAD`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/proto/aead.go internal/proto/aead_test.go go.mod go.sum
git commit -m @'
feat(proto): 加 XChaCha20-Poly1305 AEAD 包装函数

为工具通道引入认证加密：AEADSeal 输出 [24B nonce][ct+tag]，
AEADOpen 校验失败即拒绝。测试覆盖正常往返、密文篡改、错误密钥。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 3: 握手与会话密钥派生

**Files:**
- Create: `internal/proto/handshake.go`
- Test: `internal/proto/handshake_test.go`

- [ ] **Step 1: 写失败测试**

```go
package proto

import (
	"bytes"
	"testing"
)

func TestDeriveSessionKeyDeterministic(t *testing.T) {
	k1 := DeriveSessionKey("CODE-1234", "NONCE_S", "NONCE_H")
	k2 := DeriveSessionKey("CODE-1234", "NONCE_S", "NONCE_H")
	if !bytes.Equal(k1[:], k2[:]) {
		t.Fatal("expected deterministic derivation")
	}
}

func TestDeriveSessionKeyDiffersByNonce(t *testing.T) {
	k1 := DeriveSessionKey("CODE-1234", "A", "B")
	k2 := DeriveSessionKey("CODE-1234", "A", "C")
	if bytes.Equal(k1[:], k2[:]) {
		t.Fatal("expected different keys for different help nonces")
	}
}

func TestNewHelloHasRandomNonce(t *testing.T) {
	h1 := NewHello()
	h2 := NewHello()
	if h1.NonceB64 == h2.NonceB64 {
		t.Fatal("expected fresh random nonces")
	}
	if len(h1.NonceB64) < 16 {
		t.Fatalf("nonce too short: %s", h1.NonceB64)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/proto/ -run TestDerive -v`
Expected: FAIL `undefined: DeriveSessionKey`

- [ ] **Step 3: 实现 `internal/proto/handshake.go`**

```go
package proto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"

	"golang.org/x/crypto/hkdf"
)

// ToolProtocolVersion 工具通道协议版本（首版）
const ToolProtocolVersion = "1"

// DeriveSessionKey 以协助码 + 两端 nonce 派生 32 字节会话密钥。
// 不可逆，相同输入得到相同密钥；任一输入变化得到完全不同密钥。
func DeriveSessionKey(code, nonceShare, nonceHelp string) [32]byte {
	salt := []byte(nonceShare + "|" + nonceHelp)
	info := []byte("rat-tool-v" + ToolProtocolVersion)
	hk := hkdf.New(sha256.New, []byte(code), salt, info)
	var key [32]byte
	io.ReadFull(hk, key[:])
	return key
}

// NewHello 生成带随机 nonce 的 Hello
func NewHello() Hello {
	var n [16]byte
	rand.Read(n[:])
	return Hello{
		Version:      ToolProtocolVersion,
		Capabilities: []string{"exec", "read_file", "write_file", "list_dir", "stat", "glob", "grep", "process_list", "tail_log"},
		NonceB64:     base64.StdEncoding.EncodeToString(n[:]),
	}
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/proto/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/proto/handshake.go internal/proto/handshake_test.go
git commit -m @'
feat(proto): HKDF 派生工具通道会话密钥 + Hello 构造器

ToolProtocolVersion=1；DeriveSessionKey(code, nonceShare, nonceHelp)
用 HKDF-SHA256 派生 32 字节 AEAD key。NewHello 生成随机 16B nonce
并声明工具集 capabilities。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 4: 沙箱（路径校验 + exec 策略）

**Files:**
- Create: `internal/agent/sandbox.go`
- Test: `internal/agent/sandbox_test.go`

- [ ] **Step 1: 写失败测试**

```go
package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSandboxAllowsInsideRoot(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(SandboxConfig{Root: root})
	inside := filepath.Join(root, "a", "b.txt")
	os.MkdirAll(filepath.Dir(inside), 0755)
	os.WriteFile(inside, []byte("x"), 0644)
	if _, err := sb.ResolvePath(inside); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
}

func TestSandboxRejectsOutside(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(SandboxConfig{Root: root})
	outside := filepath.Join(filepath.Dir(root), "elsewhere.txt")
	if _, err := sb.ResolvePath(outside); err == nil {
		t.Fatal("expected reject for path outside root")
	}
}

func TestSandboxRejectsDotDotEscape(t *testing.T) {
	root := t.TempDir()
	sb := NewSandbox(SandboxConfig{Root: root})
	escape := filepath.Join(root, "..", "..", "etc")
	if _, err := sb.ResolvePath(escape); err == nil {
		t.Fatal("expected reject for ..-escape")
	}
}

func TestExecPolicyDenyList(t *testing.T) {
	sb := NewSandbox(SandboxConfig{DenyExec: []string{"rm", "shutdown"}})
	if err := sb.CheckExec([]string{"rm", "-rf", "/"}); err == nil {
		t.Fatal("expected deny for rm")
	}
	if err := sb.CheckExec([]string{"ls"}); err != nil {
		t.Fatalf("expected allow ls, got %v", err)
	}
}

func TestExecPolicyAllowList(t *testing.T) {
	sb := NewSandbox(SandboxConfig{AllowExec: []string{"go", "git"}})
	if err := sb.CheckExec([]string{"go", "test"}); err != nil {
		t.Fatalf("expected allow go, got %v", err)
	}
	if err := sb.CheckExec([]string{"curl"}); err == nil {
		t.Fatal("expected deny curl not in allowlist")
	}
}

func TestSandboxUnsafeBypass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path semantics differ on windows; covered separately")
	}
	sb := NewSandbox(SandboxConfig{Unsafe: true})
	if _, err := sb.ResolvePath("/tmp"); err != nil {
		t.Fatalf("unsafe should allow anywhere, got %v", err)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/ -v`
Expected: FAIL `package agent does not exist` 等

- [ ] **Step 3: 实现 `internal/agent/sandbox.go`**

```go
package agent

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SandboxConfig share 启动时来自 CLI 的策略
type SandboxConfig struct {
	Root      string   // 文件操作必须在此子树内；空 = 拒绝所有文件操作
	AllowExec []string // 若非空，argv[0] 必须在此列表（basename 比较）
	DenyExec  []string // argv[0] 命中即拒绝（basename 比较）
	Unsafe    bool     // 关闭全部沙箱（启动时强制红色横幅 + 倒计时确认）
}

// Sandbox 封装路径/exec 决策
type Sandbox struct {
	cfg  SandboxConfig
	root string // EvalSymlinks 后的绝对 root
}

// NewSandbox 注意：root 若指定，构造时已 EvalSymlinks + Abs；运行时短路 stale root 重算无意义
func NewSandbox(cfg SandboxConfig) *Sandbox {
	sb := &Sandbox{cfg: cfg}
	if cfg.Root != "" {
		if abs, err := filepath.Abs(cfg.Root); err == nil {
			if eval, err := filepath.EvalSymlinks(abs); err == nil {
				sb.root = eval
			} else {
				sb.root = abs
			}
		}
	}
	return sb
}

// ResolvePath 校验并返回规范化路径；不存在的目标走父目录解析（write 路径）
func (s *Sandbox) ResolvePath(p string) (string, error) {
	if s.cfg.Unsafe {
		abs, err := filepath.Abs(p)
		return abs, err
	}
	if s.root == "" {
		return "", fmt.Errorf("path_outside_root: no --root configured")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(abs)
	// 对已存在路径走 EvalSymlinks；不存在的（write_file 创建）退化到 lexical
	resolved := cleaned
	if eval, err := filepath.EvalSymlinks(cleaned); err == nil {
		resolved = eval
	}
	rel, err := filepath.Rel(s.root, resolved)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("path_outside_root: %s", p)
	}
	return resolved, nil
}

// CheckExec argv[0] 的 basename 比较
func (s *Sandbox) CheckExec(argv []string) error {
	if s.cfg.Unsafe {
		return nil
	}
	if len(argv) == 0 {
		return fmt.Errorf("exec_denied: empty argv")
	}
	name := filepath.Base(argv[0])
	for _, d := range s.cfg.DenyExec {
		if d == name {
			return fmt.Errorf("exec_denied: %s in deny list", name)
		}
	}
	if len(s.cfg.AllowExec) > 0 {
		for _, a := range s.cfg.AllowExec {
			if a == name {
				return nil
			}
		}
		return fmt.Errorf("exec_denied: %s not in allow list", name)
	}
	return nil
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/sandbox.go internal/agent/sandbox_test.go
git commit -m @'
feat(agent): 沙箱（--root 路径校验 + --allow/deny-exec 策略）

Sandbox.ResolvePath 用 Abs + EvalSymlinks + Rel 三段校验防 ..-逃逸与
symlink 越狱；不存在路径退化到 lexical 校验（支持 write_file 创建）。
CheckExec 按 argv[0] basename 比较 allow/deny 列表，Unsafe=true 整体
旁路。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 5: Tool 接口 + Registry + Dispatcher

**Files:**
- Create: `internal/agent/agent.go`
- Test: `internal/agent/agent_test.go`

- [ ] **Step 1: 写失败测试**

```go
package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

type fakeTool struct{ name string }

func (f *fakeTool) Name() string { return f.name }
func (f *fakeTool) Run(ctx context.Context, in json.RawMessage, out StreamSink) (json.RawMessage, error) {
	return json.RawMessage(`{"echo":"` + f.name + `"}`), nil
}

func TestRegistryDispatchOK(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeTool{name: "ping"})
	resp := r.Dispatch(context.Background(), &proto.ToolReq{ID: 1, Tool: "ping", ArgsJSON: json.RawMessage(`{}`)}, nil)
	if !resp.OK {
		t.Fatalf("expected ok, got %+v", resp)
	}
	if string(resp.ResultJSON) != `{"echo":"ping"}` {
		t.Fatalf("result: %s", resp.ResultJSON)
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	r := NewRegistry()
	resp := r.Dispatch(context.Background(), &proto.ToolReq{ID: 1, Tool: "ghost"}, nil)
	if resp.OK || resp.ErrorCode != "unknown_tool" {
		t.Fatalf("expected unknown_tool err, got %+v", resp)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/ -run TestRegistry -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/agent/agent.go`**

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/remote-assist/tool/internal/proto"
)

// Tool 单个工具的执行单元
type Tool interface {
	Name() string
	// Run 同步返回结果；如果工具支持流式输出，通过 sink 推送 StreamChunk，最终 ResultJSON 可为 nil
	Run(ctx context.Context, args json.RawMessage, sink StreamSink) (json.RawMessage, error)
}

// StreamSink agent 注入的流输出通道（Dispatcher 负责把数据封成 MsgToolStream 帧）
type StreamSink interface {
	Send(stream string, data []byte) error // stream = "stdout" | "stderr" | ""
}

// Registry 名称到 Tool 的注册表
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	r.tools[t.Name()] = t
	r.mu.Unlock()
}

// Dispatch 同步执行（流式工具内部用 sink 推 chunks，外部仍返回 ToolResp）
func (r *Registry) Dispatch(ctx context.Context, req *proto.ToolReq, sink StreamSink) proto.ToolResp {
	r.mu.RLock()
	t, ok := r.tools[req.Tool]
	r.mu.RUnlock()
	if !ok {
		return proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "unknown_tool", ErrorMsg: fmt.Sprintf("tool %q not registered", req.Tool)}
	}
	defer func() {
		if r := recover(); r != nil {
			// see Daemon.handle for capture; here we re-panic to bubble to outer recover
			panic(r)
		}
	}()
	out, err := t.Run(ctx, req.ArgsJSON, sink)
	if err != nil {
		code, msg := classifyError(err)
		return proto.ToolResp{ID: req.ID, OK: false, ErrorCode: code, ErrorMsg: msg}
	}
	return proto.ToolResp{ID: req.ID, OK: true, ResultJSON: out}
}

// classifyError 把工具返回的 error 映射到 spec §5.3 错误码
func classifyError(err error) (code, msg string) {
	s := err.Error()
	switch {
	case contains(s, "path_outside_root"):
		return "path_outside_root", s
	case contains(s, "exec_denied"):
		return "exec_denied", s
	case contains(s, "deadline_exceeded"), contains(s, "context deadline exceeded"):
		return "deadline_exceeded", s
	case contains(s, "permission denied"):
		return "permission_denied", s
	case contains(s, "no such file"), contains(s, "file does not exist"):
		return "file_not_found", s
	default:
		return "internal_error", s
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && (s == sub || stringsHas(s, sub)) }

// 小内联避免引 strings
func stringsHas(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

注：实际项目可改用 `strings.Contains`；为保持 file 自洽这里内联。后续 Task 6+ 引入 tool 实现后会自然 import strings，到时可清理。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/ -v -run TestRegistry`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/agent.go internal/agent/agent_test.go
git commit -m @'
feat(agent): Tool 接口、Registry、Dispatcher 骨架

Tool { Name, Run(ctx, args, sink) }；Registry.Dispatch 同步执行并把
错误按 spec §5.3 映射到标准错误码。流式工具通过 StreamSink 推 chunk。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 6: `exec` 工具（同步 + 流式 + 取消）

**Files:**
- Create: `internal/agent/tools/exec.go`
- Test: `internal/agent/tools/exec_test.go`

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

type captureSink struct {
	stdout []byte
	stderr []byte
}

func (c *captureSink) Send(stream string, data []byte) error {
	if stream == "stdout" {
		c.stdout = append(c.stdout, data...)
	} else if stream == "stderr" {
		c.stderr = append(c.stderr, data...)
	}
	return nil
}

func TestExecSyncEchoes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/echo")
	}
	tool := NewExec(nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"/bin/echo", "hello"}})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	json.Unmarshal(out, &r)
	if r.ExitCode != 0 {
		t.Fatalf("exit=%d", r.ExitCode)
	}
	if string(r.Stdout) != "hello\n" {
		t.Fatalf("stdout=%q", r.Stdout)
	}
}

func TestExecStreamPushesChunks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	tool := NewExec(nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"/bin/echo", "line"}, "stream": true})
	sink := &captureSink{}
	out, err := tool.Run(context.Background(), args, sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var r ExecResult
	json.Unmarshal(out, &r)
	if r.ExitCode != 0 || string(sink.stdout) != "line\n" {
		t.Fatalf("exit=%d stdout=%q", r.ExitCode, sink.stdout)
	}
}

func TestExecTimeoutKills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	tool := NewExec(nil)
	args, _ := json.Marshal(map[string]any{"argv": []string{"/bin/sleep", "30"}, "timeout_ms": 200})
	start := time.Now()
	_, err := tool.Run(context.Background(), args, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("did not kill promptly: %v", elapsed)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/tools/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/agent/tools/exec.go`**

```go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

type ExecArgs struct {
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutMs  uint32            `json:"timeout_ms,omitempty"`
	Stream     bool              `json:"stream,omitempty"`
	StdinBytes []byte            `json:"stdin,omitempty"`
}

type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
}

// ExecTool 通过 argv 列表（不经过 shell）执行命令
type ExecTool struct{ sb *agent.Sandbox }

func NewExec(sb *agent.Sandbox) *ExecTool { return &ExecTool{sb: sb} }
func (e *ExecTool) Name() string          { return "exec" }

const defaultExecTimeout = 5 * time.Minute

func (e *ExecTool) Run(ctx context.Context, raw json.RawMessage, sink agent.StreamSink) (json.RawMessage, error) {
	var a ExecArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if len(a.Argv) == 0 {
		return nil, fmt.Errorf("argv required")
	}
	if e.sb != nil {
		if err := e.sb.CheckExec(a.Argv); err != nil {
			return nil, err
		}
	}
	timeout := defaultExecTimeout
	if a.TimeoutMs > 0 {
		timeout = time.Duration(a.TimeoutMs) * time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, a.Argv[0], a.Argv[1:]...)
	if a.Cwd != "" {
		cmd.Dir = a.Cwd
	}
	if len(a.Env) > 0 {
		envv := make([]string, 0, len(a.Env))
		for k, v := range a.Env {
			envv = append(envv, k+"="+v)
		}
		cmd.Env = envv
	}
	if len(a.StdinBytes) > 0 {
		// 简单赋值 stdin（小输入），大输入留 v2
		cmd.Stdin = bytesReader(a.StdinBytes)
	}

	if a.Stream && sink != nil {
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		var wg sync.WaitGroup
		pump := func(name string, r io.Reader) {
			defer wg.Done()
			buf := make([]byte, 32*1024)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					sink.Send(name, append([]byte{}, buf[:n]...))
				}
				if err != nil {
					return
				}
			}
		}
		wg.Add(2)
		go pump("stdout", stdout)
		go pump("stderr", stderr)
		err := cmd.Wait()
		wg.Wait()
		exitCode := exitCodeOf(err)
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("deadline_exceeded: exec timed out")
		}
		return json.Marshal(ExecResult{ExitCode: exitCode})
	}

	// 同步收集
	out, err := cmd.Output()
	stderr := capturedStderr(err)
	exitCode := exitCodeOf(err)
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("deadline_exceeded: exec timed out")
	}
	return json.Marshal(ExecResult{ExitCode: exitCode, Stdout: out, Stderr: stderr})
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

func capturedStderr(err error) []byte {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.Stderr
	}
	return nil
}

// 极小 bytes.NewReader 包装；避免直接引 bytes 包来保持 file 自洽
type bytesReader []byte

func (b bytesReader) Read(p []byte) (int, error) {
	n := copy(p, b)
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}
```

注：`bytesReader` 是一次性读，对 MVP 够；后续如需多次读，换 `bytes.NewReader`。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/tools/ -v -run TestExec`
Expected: PASS（Windows 上 skip 即 SKIP）

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/tools/exec.go internal/agent/tools/exec_test.go
git commit -m @'
feat(agent): exec 工具 (同步/流式/超时)

argv 列表直 exec.Command 不过 shell；timeout_ms 用 context；stream=true
通过 StreamSink 实时推 stdout/stderr 字节。复用 sandbox CheckExec。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 7: `read_file` / `write_file` 工具

**Files:**
- Create: `internal/agent/tools/file.go`
- Test: `internal/agent/tools/file_test.go`

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

func TestReadFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x.txt")
	os.WriteFile(p, []byte("hello world"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewReadFile(sb)
	args, _ := json.Marshal(map[string]any{"path": p})
	out, err := tool.Run(context.Background(), args, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var r ReadFileResult
	json.Unmarshal(out, &r)
	if string(r.Bytes) != "hello world" || !r.EOF {
		t.Fatalf("got %+v", r)
	}
}

func TestReadFileOutsideRootRejected(t *testing.T) {
	root := t.TempDir()
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewReadFile(sb)
	args, _ := json.Marshal(map[string]any{"path": filepath.Join(filepath.Dir(root), "evil")})
	if _, err := tool.Run(context.Background(), args, nil); err == nil {
		t.Fatal("expected path_outside_root")
	}
}

func TestWriteFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "out.txt")
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewWriteFile(sb)
	args, _ := json.Marshal(map[string]any{"path": p, "content": []byte("yo"), "create": true})
	if _, err := tool.Run(context.Background(), args, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "yo" {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/tools/ -v -run TestReadFile`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/agent/tools/file.go`**

```go
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/remote-assist/tool/internal/agent"
)

type ReadFileArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"` // 0 = until EOF or 1MiB cap，取较小
}

type ReadFileResult struct {
	Bytes []byte `json:"bytes"`
	EOF   bool   `json:"eof"`
}

const readFileMaxChunk = 1 << 20 // 1 MiB；大文件由 caller 分多次

type ReadFileTool struct{ sb *agent.Sandbox }

func NewReadFile(sb *agent.Sandbox) *ReadFileTool { return &ReadFileTool{sb: sb} }
func (t *ReadFileTool) Name() string              { return "read_file" }

func (t *ReadFileTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a ReadFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	resolved, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file_not_found: %s", a.Path)
		}
		return nil, err
	}
	defer f.Close()
	if a.Offset > 0 {
		if _, err := f.Seek(a.Offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	limit := int64(readFileMaxChunk)
	if a.Length > 0 && a.Length < limit {
		limit = a.Length
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if !eof && err != nil {
		return nil, err
	}
	return json.Marshal(ReadFileResult{Bytes: buf[:n], EOF: eof})
}

type WriteFileArgs struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`   // 默认 0644
	Create  bool   `json:"create,omitempty"` // 默认 true；false 时文件不存在则失败
	Append  bool   `json:"append,omitempty"`
}

type WriteFileResult struct {
	BytesWritten int `json:"bytes_written"`
}

type WriteFileTool struct{ sb *agent.Sandbox }

func NewWriteFile(sb *agent.Sandbox) *WriteFileTool { return &WriteFileTool{sb: sb} }
func (t *WriteFileTool) Name() string               { return "write_file" }

func (t *WriteFileTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a WriteFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	resolved, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	mode := fs.FileMode(0644)
	if a.Mode != 0 {
		mode = fs.FileMode(a.Mode)
	}
	flag := os.O_WRONLY | os.O_TRUNC
	if a.Append {
		flag = os.O_WRONLY | os.O_APPEND
	}
	if !a.Create && !fileExists(resolved) {
		return nil, fmt.Errorf("file_not_found: %s", a.Path)
	}
	flag |= os.O_CREATE
	f, err := os.OpenFile(resolved, flag, mode)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := f.Write(a.Content)
	if err != nil {
		return nil, err
	}
	return json.Marshal(WriteFileResult{BytesWritten: n})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/tools/ -v -run TestReadFile`
Run: `go test ./internal/agent/tools/ -v -run TestWriteFile`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/tools/file.go internal/agent/tools/file_test.go
git commit -m @'
feat(agent): read_file / write_file 工具（沙箱化）

read_file 一次最多 1MiB；offset/length 支持分块；不存在返回
file_not_found。write_file 默认 0644+CREATE+TRUNC，支持 append=true。
全部走 Sandbox.ResolvePath，越界返 path_outside_root。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 8: `list_dir` / `stat` / `glob` 工具

**Files:**
- Create: `internal/agent/tools/fs.go`
- Test: `internal/agent/tools/fs_test.go`

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

func TestListDir(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0644)
	os.MkdirAll(filepath.Join(root, "sub"), 0755)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewListDir(sb)
	args, _ := json.Marshal(map[string]any{"path": root})
	out, _ := tool.Run(context.Background(), args, nil)
	var r ListDirResult
	json.Unmarshal(out, &r)
	if len(r.Entries) != 2 {
		t.Fatalf("entries: %+v", r.Entries)
	}
}

func TestStatFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x")
	os.WriteFile(p, []byte("hi"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewStat(sb)
	args, _ := json.Marshal(map[string]any{"path": p})
	out, _ := tool.Run(context.Background(), args, nil)
	var r StatResult
	json.Unmarshal(out, &r)
	if r.Size != 2 || r.Kind != "file" {
		t.Fatalf("got %+v", r)
	}
}

func TestGlob(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "x.go"), nil, 0644)
	os.WriteFile(filepath.Join(root, "y.go"), nil, 0644)
	os.WriteFile(filepath.Join(root, "z.txt"), nil, 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewGlob(sb)
	args, _ := json.Marshal(map[string]any{"pattern": "*.go", "root": root})
	out, _ := tool.Run(context.Background(), args, nil)
	var r GlobResult
	json.Unmarshal(out, &r)
	if len(r.Paths) != 2 {
		t.Fatalf("paths: %+v", r.Paths)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/tools/ -v -run TestListDir`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/agent/tools/fs.go`**

```go
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/remote-assist/tool/internal/agent"
)

// list_dir
type ListDirArgs struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
	Glob      string `json:"glob,omitempty"`
}
type DirEntry struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // "file" | "dir" | "symlink" | "other"
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // unix sec
}
type ListDirResult struct {
	Entries []DirEntry `json:"entries"`
}

type ListDirTool struct{ sb *agent.Sandbox }

func NewListDir(sb *agent.Sandbox) *ListDirTool { return &ListDirTool{sb: sb} }
func (t *ListDirTool) Name() string             { return "list_dir" }

func (t *ListDirTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a ListDirArgs
	json.Unmarshal(raw, &a)
	root, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	var out []DirEntry
	walk := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过权限错的子树
		}
		if p == root {
			return nil
		}
		if a.Glob != "" {
			matched, _ := filepath.Match(a.Glob, d.Name())
			if !matched {
				if d.IsDir() && !a.Recursive {
					return fs.SkipDir
				}
				return nil
			}
		}
		info, _ := d.Info()
		kind := "other"
		switch {
		case d.IsDir():
			kind = "dir"
		case d.Type().IsRegular():
			kind = "file"
		case d.Type()&fs.ModeSymlink != 0:
			kind = "symlink"
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, DirEntry{Name: rel, Kind: kind, Size: info.Size(), Mtime: info.ModTime().Unix()})
		if d.IsDir() && !a.Recursive && p != root {
			return fs.SkipDir
		}
		return nil
	}
	if a.Recursive {
		filepath.WalkDir(root, walk)
	} else {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, d := range entries {
			walk(filepath.Join(root, d.Name()), d, nil)
		}
	}
	return json.Marshal(ListDirResult{Entries: out})
}

// stat
type StatArgs struct{ Path string `json:"path"` }
type StatResult struct {
	Kind  string `json:"kind"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Mode  uint32 `json:"mode"`
}

type StatTool struct{ sb *agent.Sandbox }

func NewStat(sb *agent.Sandbox) *StatTool { return &StatTool{sb: sb} }
func (t *StatTool) Name() string          { return "stat" }

func (t *StatTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a StatArgs
	json.Unmarshal(raw, &a)
	resolved, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file_not_found: %s", a.Path)
		}
		return nil, err
	}
	kind := "other"
	switch {
	case info.IsDir():
		kind = "dir"
	case info.Mode().IsRegular():
		kind = "file"
	case info.Mode()&fs.ModeSymlink != 0:
		kind = "symlink"
	}
	return json.Marshal(StatResult{Kind: kind, Size: info.Size(), Mtime: info.ModTime().Unix(), Mode: uint32(info.Mode())})
}

// glob
type GlobArgs struct {
	Pattern string `json:"pattern"`
	Root    string `json:"root,omitempty"`
}
type GlobResult struct{ Paths []string `json:"paths"` }

type GlobTool struct{ sb *agent.Sandbox }

func NewGlob(sb *agent.Sandbox) *GlobTool { return &GlobTool{sb: sb} }
func (t *GlobTool) Name() string          { return "glob" }

func (t *GlobTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a GlobArgs
	json.Unmarshal(raw, &a)
	root := a.Root
	if root == "" {
		root = "."
	}
	resolved, err := t.sb.ResolvePath(root)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(resolved, a.Pattern))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, _ := filepath.Rel(resolved, m)
		out = append(out, rel)
	}
	return json.Marshal(GlobResult{Paths: out})
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/tools/ -v -run "TestListDir|TestStat|TestGlob"`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/tools/fs.go internal/agent/tools/fs_test.go
git commit -m @'
feat(agent): list_dir / stat / glob 工具（沙箱化）

list_dir 支持 recursive 与 name-glob 过滤；stat 用 Lstat 区分 symlink；
glob 用 filepath.Glob 相对 root 解析。所有路径走 Sandbox 校验。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 9: `grep` 工具

**Files:**
- Create: `internal/agent/tools/grep.go`
- Test: `internal/agent/tools/grep_test.go`

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/remote-assist/tool/internal/agent"
)

func TestGrep(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\nfunc Foo() {}\n"), 0644)
	os.WriteFile(filepath.Join(root, "b.go"), []byte("package main\nfunc Bar() {}\n"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewGrep(sb)
	args, _ := json.Marshal(map[string]any{"pattern": "Foo", "root": root, "glob": "*.go"})
	out, _ := tool.Run(context.Background(), args, nil)
	var r GrepResult
	json.Unmarshal(out, &r)
	if len(r.Matches) != 1 || r.Matches[0].Line != 2 {
		t.Fatalf("matches: %+v", r.Matches)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/tools/ -v -run TestGrep`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/agent/tools/grep.go`**

```go
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/remote-assist/tool/internal/agent"
)

type GrepArgs struct {
	Pattern    string `json:"pattern"`
	Root       string `json:"root,omitempty"`
	Glob       string `json:"glob,omitempty"`        // 文件名 glob 过滤
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	MaxMatches int    `json:"max_matches,omitempty"` // 默认 1000
}

type GrepMatch struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type GrepResult struct{ Matches []GrepMatch `json:"matches"` }

type GrepTool struct{ sb *agent.Sandbox }

func NewGrep(sb *agent.Sandbox) *GrepTool { return &GrepTool{sb: sb} }
func (t *GrepTool) Name() string          { return "grep" }

func (t *GrepTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a GrepArgs
	json.Unmarshal(raw, &a)
	pat := a.Pattern
	if a.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	root := a.Root
	if root == "" {
		root = "."
	}
	resolved, err := t.sb.ResolvePath(root)
	if err != nil {
		return nil, err
	}
	max := a.MaxMatches
	if max == 0 {
		max = 1000
	}
	var out []GrepMatch
	filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if a.Glob != "" {
			matched, _ := filepath.Match(a.Glob, d.Name())
			if !matched {
				return nil
			}
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		rel, _ := filepath.Rel(resolved, p)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			if re.MatchString(sc.Text()) {
				out = append(out, GrepMatch{File: rel, Line: lineNo, Text: sc.Text()})
				if len(out) >= max {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return json.Marshal(GrepResult{Matches: out})
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/tools/ -v -run TestGrep`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/tools/grep.go internal/agent/tools/grep_test.go
git commit -m @'
feat(agent): grep 工具（regexp + name-glob 过滤 + max_matches 上限）

WalkDir + bufio.Scanner 行级匹配；ignore_case=true 自动前缀 (?i)；
默认 1000 个 match 上限避免大仓库爆量。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 10: `process_list` 工具（跨平台）

**Files:**
- Create: `internal/agent/tools/proc.go`
- Test: `internal/agent/tools/proc_test.go`

- [ ] **Step 1: 写失败测试**

```go
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
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/tools/ -v -run TestProcessList`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/agent/tools/proc.go`**

MVP 用 `go-ps` 第三方库（轻量，跨平台），或退而求其次用 `os/exec` 调 `tasklist`/`ps`。这里选最直白方案：调系统命令。

```go
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
	Filter string `json:"filter,omitempty"` // 子串过滤进程名
}

type ProcInfo struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	CmdLine string `json:"cmdline,omitempty"`
	User    string `json:"user,omitempty"`
}

type ProcessListResult struct{ Procs []ProcInfo `json:"procs"` }

type ProcessListTool struct{}

func NewProcessList() *ProcessListTool   { return &ProcessListTool{} }
func (t *ProcessListTool) Name() string  { return "process_list" }

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

// splitCSV 极简 CSV 行解析（处理 "..." 引号）
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
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/tools/ -v -run TestProcessList`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/tools/proc.go internal/agent/tools/proc_test.go
git commit -m @'
feat(agent): process_list 工具（Windows tasklist / Unix ps）

按平台分发到 tasklist /FO CSV 或 ps -eo；可选 filter 子串匹配
进程名或 cmdline。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 11: `tail_log` 工具

**Files:**
- Create: `internal/agent/tools/log.go`
- Test: `internal/agent/tools/log_test.go`
- Modify: `go.mod` (add fsnotify dependency)

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

func TestTailLogStatic(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "app.log")
	os.WriteFile(p, []byte("line1\nline2\nline3\n"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewTailLog(sb)
	args, _ := json.Marshal(map[string]any{"path": p, "lines": 2})
	sink := &captureSink{}
	_, err := tool.Run(context.Background(), args, sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := string(sink.stdout)
	if !strings.Contains(got, "line2") || !strings.Contains(got, "line3") {
		t.Fatalf("got: %q", got)
	}
}

func TestTailLogFollow(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "app.log")
	os.WriteFile(p, []byte("line1\n"), 0644)
	sb := agent.NewSandbox(agent.SandboxConfig{Root: root})
	tool := NewTailLog(sb)
	args, _ := json.Marshal(map[string]any{"path": p, "follow": true})
	sink := &captureSink{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0644)
		f.Write([]byte("appended\n"))
		f.Close()
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	tool.Run(ctx, args, sink)
	if !strings.Contains(string(sink.stdout), "appended") {
		t.Fatalf("did not capture appended line: %q", sink.stdout)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/tools/ -v -run TestTailLog`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/agent/tools/log.go`**

```go
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/remote-assist/tool/internal/agent"
)

type TailLogArgs struct {
	Path   string `json:"path"`
	Lines  int    `json:"lines,omitempty"`  // 初始倒读 N 行；默认 100
	Follow bool   `json:"follow,omitempty"` // true 时阻塞直到 ctx 取消
}

type TailLogTool struct{ sb *agent.Sandbox }

func NewTailLog(sb *agent.Sandbox) *TailLogTool { return &TailLogTool{sb: sb} }
func (t *TailLogTool) Name() string             { return "tail_log" }

func (t *TailLogTool) Run(ctx context.Context, raw json.RawMessage, sink agent.StreamSink) (json.RawMessage, error) {
	var a TailLogArgs
	json.Unmarshal(raw, &a)
	if a.Lines == 0 {
		a.Lines = 100
	}
	resolved, err := t.sb.ResolvePath(a.Path)
	if err != nil {
		return nil, err
	}

	// 初始：读最后 N 行
	last, err := readLastLines(resolved, a.Lines)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, err
	}
	if sink != nil && len(last) > 0 {
		sink.Send("stdout", last)
	}

	if !a.Follow {
		return json.RawMessage(`{"followed":false}`), nil
	}

	// follow: fsnotify watch，每次 Write 事件把新增字节推 sink
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	defer w.Close()
	w.Add(resolved)

	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return json.RawMessage(`{"followed":true,"closed":"context"}`), nil
		case ev, ok := <-w.Events:
			if !ok {
				return json.RawMessage(`{"followed":true,"closed":"watcher"}`), nil
			}
			if ev.Op&fsnotify.Write != 0 {
				for {
					n, _ := f.Read(buf)
					if n == 0 {
						break
					}
					if sink != nil {
						sink.Send("stdout", append([]byte{}, buf[:n]...))
					}
				}
			}
		case <-time.After(500 * time.Millisecond):
			// 兜底：某些平台 fsnotify 漏事件，每 500ms 主动 try-read
			for {
				n, _ := f.Read(buf)
				if n == 0 {
					break
				}
				if sink != nil {
					sink.Send("stdout", append([]byte{}, buf[:n]...))
				}
			}
		}
	}
}

// readLastLines 反向扫描读最后 n 行
func readLastLines(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// 简化实现：全文 scan，环形保留 n 行（小日志够用；大日志 v2 改 mmap 倒扫）
	var ring []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	var out []byte
	for _, l := range ring {
		out = append(out, l...)
		out = append(out, '\n')
	}
	return out, nil
}
```

引入依赖：

```powershell
go get github.com/fsnotify/fsnotify
go mod tidy
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/tools/ -v -run TestTailLog`
Expected: PASS（Windows 上 fsnotify 也能用；500ms 兜底兜住事件丢失）

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/tools/log.go internal/agent/tools/log_test.go go.mod go.sum
git commit -m @'
feat(agent): tail_log 工具（初始 N 行 + fsnotify follow）

初始倒读 N 行（默认 100）；follow=true 用 fsnotify 监听 Write 事件
推送增量字节；500ms 兜底防事件丢失；ctx 取消立即返回。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 12: Agent Daemon（接帧 → 派发 → 回帧）

**Files:**
- Modify: `internal/agent/agent.go` (add Daemon struct)
- Create: `internal/agent/daemon_test.go`

- [ ] **Step 1: 写失败测试**

```go
package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

type fakeConn struct {
	in   chan *proto.Message
	out  chan *proto.Message
	once sync.Once
}

func (c *fakeConn) SendMessage(t proto.MessageType, p interface{}) error {
	msg, _ := proto.NewMessage(t, p)
	c.out <- msg
	return nil
}
func (c *fakeConn) Recv() *proto.Message { return <-c.in }

func TestDaemonRoutesToolReq(t *testing.T) {
	in := make(chan *proto.Message, 4)
	out := make(chan *proto.Message, 4)
	conn := &fakeConn{in: in, out: out}

	r := NewRegistry()
	r.Register(&fakeTool{name: "ping"})
	d := NewDaemon(r, conn, [32]byte{})

	go d.RunLoop(context.Background())

	req := proto.ToolReq{ID: 9, Tool: "ping", ArgsJSON: json.RawMessage(`{}`)}
	msg, _ := proto.NewMessage(proto.MsgToolReq, &req)
	in <- msg

	select {
	case got := <-out:
		if got.Type != proto.MsgToolResp {
			t.Fatalf("expected ToolResp, got %s", got.Type)
		}
		var resp proto.ToolResp
		proto.DecodePayload(got, &resp)
		if resp.ID != 9 || !resp.OK {
			t.Fatalf("got %+v", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("no response in 1s")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/agent/ -v -run TestDaemon`
Expected: FAIL

- [ ] **Step 3: 在 `internal/agent/agent.go` 追加：**

```go
// MsgConn agent 与 share Client 之间的最小依赖契约（便于单测注入）
type MsgConn interface {
	SendMessage(t proto.MessageType, payload interface{}) error
	// agent 不主动 read；share 端 dispatch 把收到的 Tool 消息通过 Inject 投递
}

// Daemon share 端工具消息分发器；持有 Registry + outbound conn
type Daemon struct {
	reg     *Registry
	conn    MsgConn
	key     [32]byte
	inbound chan *proto.Message
	cancels sync.Map // id -> context.CancelFunc
}

func NewDaemon(reg *Registry, conn MsgConn, key [32]byte) *Daemon {
	return &Daemon{reg: reg, conn: conn, key: key, inbound: make(chan *proto.Message, 64)}
}

// Inject share 端 dispatch 收到 MsgToolReq/MsgToolCancel 时调用
func (d *Daemon) Inject(msg *proto.Message) { d.inbound <- msg }

// RunLoop 直到 ctx 完成
func (d *Daemon) RunLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-d.inbound:
			switch msg.Type {
			case proto.MsgToolReq:
				go d.handleReq(ctx, msg)
			case proto.MsgToolCancel:
				var c proto.Cancel
				proto.DecodePayload(msg, &c)
				if v, ok := d.cancels.Load(c.ID); ok {
					v.(context.CancelFunc)()
				}
			}
		}
	}
}

func (d *Daemon) handleReq(parent context.Context, msg *proto.Message) {
	var req proto.ToolReq
	if err := proto.DecodePayload(msg, &req); err != nil {
		return
	}
	// 解密 args（如有 key 非零）
	if d.key != [32]byte{} && len(req.ArgsJSON) > 0 {
		plain, err := proto.AEADOpen(&d.key, req.ArgsJSON)
		if err != nil {
			d.conn.SendMessage(proto.MsgToolResp, &proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "decrypt_failed", ErrorMsg: err.Error()})
			return
		}
		req.ArgsJSON = plain
	}
	ctx, cancel := context.WithCancel(parent)
	d.cancels.Store(req.ID, context.CancelFunc(cancel))
	defer func() {
		d.cancels.Delete(req.ID)
		if r := recover(); r != nil {
			d.conn.SendMessage(proto.MsgToolResp, &proto.ToolResp{ID: req.ID, OK: false, ErrorCode: "remote_panic", ErrorMsg: "tool panic"})
		}
	}()
	sink := &chunkSink{daemon: d, id: req.ID, seq: 0}
	resp := d.reg.Dispatch(ctx, &req, sink)
	// 加密 result
	if d.key != [32]byte{} && len(resp.ResultJSON) > 0 {
		if ct, err := proto.AEADSeal(&d.key, resp.ResultJSON); err == nil {
			resp.ResultJSON = ct
		}
	}
	d.conn.SendMessage(proto.MsgToolResp, &resp)
}

type chunkSink struct {
	daemon *Daemon
	id     uint64
	seq    uint32
	mu     sync.Mutex
}

func (s *chunkSink) Send(stream string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload := data
	if s.daemon.key != [32]byte{} {
		ct, err := proto.AEADSeal(&s.daemon.key, data)
		if err != nil {
			return err
		}
		payload = ct
	}
	c := proto.StreamChunk{ID: s.id, Seq: s.seq, Stream: stream, Data: payload}
	s.seq++
	return s.daemon.conn.SendMessage(proto.MsgToolStream, &c)
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/agent/ -v -run TestDaemon`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/agent.go internal/agent/daemon_test.go
git commit -m @'
feat(agent): Daemon 帧分发器 + cancel + AEAD 包装

Inject(msg) 投递入帧；RunLoop 把 ToolReq goroutine 化、Cancel 取消
对应 context。每条 ToolReq 的 args 解密后给工具，result/chunk
反向加密。panic 捕获返 remote_panic。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 13: Share 端把 Daemon 接入消息分发

**Files:**
- Modify: `internal/client/share.go`
- Create: `internal/client/share_tool_test.go`（测试新 dispatch 分支）

- [ ] **Step 1: 写失败测试（验证 MsgToolReq 走 Daemon.Inject 而不是 SSH tunnel）**

```go
package client

import (
	"encoding/json"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

type injectRecorder struct{ got []*proto.Message }

func (r *injectRecorder) Inject(m *proto.Message) { r.got = append(r.got, m) }

func TestDispatchToolMessageRoutesToDaemon(t *testing.T) {
	rec := &injectRecorder{}
	req := proto.ToolReq{ID: 1, Tool: "ping", ArgsJSON: json.RawMessage(`{}`)}
	msg, _ := proto.NewMessage(proto.MsgToolReq, &req)
	if !dispatchToolMessage(msg, rec) {
		t.Fatal("expected dispatch to handle MsgToolReq")
	}
	if len(rec.got) != 1 || rec.got[0].Type != proto.MsgToolReq {
		t.Fatalf("got: %+v", rec.got)
	}
}

func TestDispatchNonToolReturnsFalse(t *testing.T) {
	rec := &injectRecorder{}
	msg, _ := proto.NewMessage(proto.MsgTunnelData, &proto.TunnelData{Data: []byte("x")})
	if dispatchToolMessage(msg, rec) {
		t.Fatal("expected dispatch to ignore non-tool msg")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/client/ -v -run TestDispatchTool`
Expected: FAIL

- [ ] **Step 3: 修改 `internal/client/share.go`，在文件顶部加：**

```go
// daemonSink share 端把 Tool 消息转给 agent.Daemon 的契约
type daemonSink interface {
	Inject(msg *proto.Message)
}

// dispatchToolMessage 若 msg 属于工具通道则投递并返回 true，否则 false
func dispatchToolMessage(msg *proto.Message, d daemonSink) bool {
	switch msg.Type {
	case proto.MsgToolReq, proto.MsgToolCancel, proto.MsgToolHello:
		if d != nil {
			d.Inject(msg)
		}
		return true
	}
	return false
}
```

并在 `handleTunnel` 的消息分发 `switch msg.Type { ... }` 里，在 `case proto.MsgTunnelData:` 之前加一个分支：

```go
		default:
			if s.daemon != nil && dispatchToolMessage(msg, s.daemon) {
				continue
			}
			log.Printf("Unexpected message: %s", msg.Type)
```

（原有 default 分支挪进 if 之后；如果原代码用 explicit case，把工具几个 case 显式分流到 daemon）

ShareMode 字段加 `daemon daemonSink`，构造 `NewShareMode` 暂不带（Task 17 接上）。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/client/ -v -run TestDispatchTool`
Run: `go test ./internal/client/ -v` （回归全部）
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/client/share.go internal/client/share_tool_test.go
git commit -m @'
feat(client/share): 在消息分发增加工具通道分流

MsgToolReq / MsgToolCancel / MsgToolHello 投递给 daemon.Inject；
其他消息走原 SSH 隧道路径。ShareMode.daemon 字段为后续注入留位。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 14: MCP stdio Server 框架

**Files:**
- Create: `internal/mcp/server.go`
- Create: `internal/mcp/schema.go`
- Test: `internal/mcp/server_test.go`

- [ ] **Step 1: 写失败测试**

```go
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestInitializeHandshake(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}` + "\n")
	var out bytes.Buffer
	srv := NewServer(nil) // 无 bridge 也能走完 initialize / tools/list
	err := srv.Serve(context.Background(), in, &out)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resp struct {
		Result struct {
			ServerInfo struct{ Name string } `json:"serverInfo"`
		} `json:"result"`
	}
	json.Unmarshal(out.Bytes(), &resp)
	if resp.Result.ServerInfo.Name == "" {
		t.Fatalf("missing server info: %s", out.String())
	}
}

func TestToolsListReturnsNineTools(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	srv := NewServer(nil)
	srv.Serve(context.Background(), in, &out)
	if !strings.Contains(out.String(), `"exec"`) || !strings.Contains(out.String(), `"tail_log"`) {
		t.Fatalf("missing tools: %s", out.String())
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/mcp/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/mcp/schema.go`**

```go
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
		{Name: "exec", Description: "Run a command via argv (no shell) on the remote.", InputSchema: json.RawMessage(`{"type":"object","required":["argv"],"properties":{"argv":{"type":"array","items":{"type":"string"}},"cwd":{"type":"string"},"timeout_ms":{"type":"integer"},"stream":{"type":"boolean"}}}`)},
		{Name: "read_file", Description: "Read a remote file.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer"},"length":{"type":"integer"}}}`)},
		{Name: "write_file", Description: "Write/overwrite a remote file.", InputSchema: json.RawMessage(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string","contentEncoding":"base64"},"append":{"type":"boolean"}}}`)},
		{Name: "list_dir", Description: "List a remote directory.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"recursive":{"type":"boolean"},"glob":{"type":"string"}}}`)},
		{Name: "stat", Description: "Stat a remote path.", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)},
		{Name: "glob", Description: "Glob remote files.", InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"root":{"type":"string"}}}`)},
		{Name: "grep", Description: "Regex search remote files.", InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string"},"root":{"type":"string"},"glob":{"type":"string"},"ignore_case":{"type":"boolean"}}}`)},
		{Name: "process_list", Description: "List remote processes.", InputSchema: json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string"}}}`)},
		{Name: "tail_log", Description: "Tail a remote log file (with optional follow).", InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"lines":{"type":"integer"},"follow":{"type":"boolean"}}}`)},
	}
}
```

- [ ] **Step 4: 实现 `internal/mcp/server.go`**

```go
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Bridge MCP server 把 tools/call 转发给 share 端的契约
type Bridge interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

type Server struct{ bridge Bridge }

func NewServer(b Bridge) *Server { return &Server{bridge: b} }

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve 在 in/out 上跑一个 MCP server loop
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal(line, &req); err != nil {
			s.write(out, rpcResp{JSONRPC: "2.0", Error: &rpcErr{Code: -32700, Message: "parse error"}})
			continue
		}
		s.dispatch(ctx, &req, out)
	}
	return sc.Err()
}

func (s *Server) dispatch(ctx context.Context, req *rpcReq, out io.Writer) {
	switch req.Method {
	case "initialize":
		s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "remote-assist", "version": "1"},
		}})
	case "tools/list":
		s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": AllSchemas()}})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		if s.bridge == nil {
			s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32603, Message: "no bridge"}})
			return
		}
		result, err := s.bridge.CallTool(ctx, p.Name, p.Arguments)
		if err != nil {
			s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32000, Message: err.Error()}})
			return
		}
		// MCP tools/call result: { content: [{type:"text", text:"..."}] } 把 JSON 当 text 返
		s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(result)}},
		}})
	case "notifications/initialized":
		// no-op
	case "notifications/cancelled":
		// bridge 处理（Task 15 实现）
	default:
		if len(req.ID) > 0 {
			s.write(out, rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}})
		}
	}
}

func (s *Server) write(out io.Writer, r rpcResp) {
	r.JSONRPC = "2.0"
	b, _ := json.Marshal(r)
	out.Write(b)
	out.Write([]byte("\n"))
}
```

- [ ] **Step 5: 运行测试，确认通过 & 提交**

Run: `go test ./internal/mcp/ -v`
Expected: PASS

```powershell
git add internal/mcp/schema.go internal/mcp/server.go internal/mcp/server_test.go
git commit -m @'
feat(mcp): stdio JSON-RPC MCP server 骨架 + 9 工具静态 schema

实现 initialize / tools/list / tools/call / notifications/* 路由；
schema.go 列出 9 个工具的固定 JSON Schema。tools/call 委托给 Bridge
接口，Task 15 接入真实跨网工具调用。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 15: MCP Bridge（跨网调用 share 端 daemon）

**Files:**
- Create: `internal/mcp/bridge.go`
- Test: `internal/mcp/bridge_test.go`

- [ ] **Step 1: 写失败测试**

```go
package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// stubConn 模拟 help 与 share 之间的隧道：CallTool 发出 ToolReq，
// 我们手工注入对应 ToolResp。
type stubConn struct {
	sent chan *proto.Message
}

func (c *stubConn) SendMessage(t proto.MessageType, p interface{}) error {
	msg, _ := proto.NewMessage(t, p)
	c.sent <- msg
	return nil
}

func TestBridgeCallToolResolvesOnResp(t *testing.T) {
	conn := &stubConn{sent: make(chan *proto.Message, 4)}
	br := NewBridge(conn, [32]byte{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 收 ToolReq，回 ToolResp
		req := <-conn.sent
		var r proto.ToolReq
		proto.DecodePayload(req, &r)
		resp, _ := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: r.ID, OK: true, ResultJSON: json.RawMessage(`{"echo":1}`)})
		br.HandleInbound(resp)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := br.CallTool(ctx, "exec", json.RawMessage(`{"argv":["echo"]}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(out) != `{"echo":1}` {
		t.Fatalf("got %s", out)
	}
	wg.Wait()
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/mcp/ -v -run TestBridge`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/mcp/bridge.go`**

```go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/remote-assist/tool/internal/proto"
)

// MsgConn help 端发给 share 端的对外契约（Client 已经实现 SendMessage）
type MsgConn interface {
	SendMessage(t proto.MessageType, payload interface{}) error
}

// Bridge MCP server <-> 隧道工具消息
type Bridge struct {
	conn    MsgConn
	key     [32]byte
	nextID  uint64
	pending sync.Map // id -> chan proto.ToolResp
}

func NewBridge(c MsgConn, key [32]byte) *Bridge { return &Bridge{conn: c, key: key} }

// CallTool 发 ToolReq，阻塞等 ToolResp（或 ctx 取消）
func (b *Bridge) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	id := atomic.AddUint64(&b.nextID, 1)
	ch := make(chan proto.ToolResp, 1)
	b.pending.Store(id, ch)
	defer b.pending.Delete(id)

	encArgs := args
	if b.key != [32]byte{} && len(args) > 0 {
		ct, err := proto.AEADSeal(&b.key, args)
		if err != nil {
			return nil, err
		}
		encArgs = ct
	}
	if err := b.conn.SendMessage(proto.MsgToolReq, &proto.ToolReq{ID: id, Tool: name, ArgsJSON: encArgs}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		b.conn.SendMessage(proto.MsgToolCancel, &proto.Cancel{ID: id, Reason: "ctx_cancelled"})
		return nil, ctx.Err()
	case resp := <-ch:
		if !resp.OK {
			return nil, fmt.Errorf("%s: %s", resp.ErrorCode, resp.ErrorMsg)
		}
		result := resp.ResultJSON
		if b.key != [32]byte{} && len(result) > 0 {
			plain, err := proto.AEADOpen(&b.key, result)
			if err != nil {
				return nil, err
			}
			result = plain
		}
		return result, nil
	}
}

// HandleInbound help 端 dispatch 收到 ToolResp / ToolStream 时调用
func (b *Bridge) HandleInbound(msg *proto.Message) {
	switch msg.Type {
	case proto.MsgToolResp:
		var r proto.ToolResp
		if err := proto.DecodePayload(msg, &r); err != nil {
			return
		}
		if v, ok := b.pending.Load(r.ID); ok {
			ch := v.(chan proto.ToolResp)
			select {
			case ch <- r:
			default:
			}
		}
	case proto.MsgToolStream:
		// v1：流式工具的 chunk 暂存进 buffer，由调用端通过 MCP progress 拉取（MVP 简化：丢弃）
		// 注：tail_log/exec stream 在 v1 由 share 端最终 ToolResp 收尾；中间 chunk 暂不直透到 Claude。
		// v2 用 MCP progress notification 推送。
	}
}
```

注：MVP 流式 chunk 在 help 端**暂时丢弃**，工具返回的最终 ToolResp 携带汇总（exec 同步模式已经够用）；流式直透到 Claude 留 v2。这点写入 spec 的"开放问题"也是合理。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/mcp/ -v -run TestBridge`
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/mcp/bridge.go internal/mcp/bridge_test.go
git commit -m @'
feat(mcp): Bridge 跨网工具调用 + ToolResp 等待

CallTool 注册 pending channel、发 ToolReq、阻塞等 ToolResp；
ctx 取消会发 ToolCancel。AEAD args/result 透明加解密。
ToolStream chunk 在 v1 暂丢弃（exec/tail_log 走 ToolResp 汇总）。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 16: Help 端把 MCP server 接入 + 工具消息分发

**Files:**
- Modify: `internal/client/help.go`
- Create: `internal/client/help_mcp.go`（新模式入口）
- Test: `internal/client/help_mcp_test.go`

- [ ] **Step 1: 写失败测试**

```go
package client

import (
	"encoding/json"
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

type bridgeRecorder struct{ inbound []*proto.Message }

func (b *bridgeRecorder) HandleInbound(m *proto.Message) { b.inbound = append(b.inbound, m) }

func TestHelpDispatchRoutesToolRespToBridge(t *testing.T) {
	br := &bridgeRecorder{}
	msg, _ := proto.NewMessage(proto.MsgToolResp, &proto.ToolResp{ID: 1, OK: true, ResultJSON: json.RawMessage(`{}`)})
	if !dispatchHelpToolMessage(msg, br) {
		t.Fatal("expected dispatch")
	}
	if len(br.inbound) != 1 {
		t.Fatalf("got %d", len(br.inbound))
	}
}

func TestHelpDispatchIgnoresTunnelData(t *testing.T) {
	br := &bridgeRecorder{}
	msg, _ := proto.NewMessage(proto.MsgTunnelData, &proto.TunnelData{Data: []byte("x")})
	if dispatchHelpToolMessage(msg, br) {
		t.Fatal("did not expect dispatch for TunnelData")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/client/ -v -run TestHelpDispatch`
Expected: FAIL

- [ ] **Step 3: 实现 `internal/client/help_mcp.go`**

```go
package client

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/remote-assist/tool/internal/mcp"
	"github.com/remote-assist/tool/internal/proto"
)

// inboundSink help 端 dispatch 投递 ToolResp/ToolStream 的契约
type inboundSink interface {
	HandleInbound(msg *proto.Message)
}

// dispatchHelpToolMessage 工具消息分流；返回 true 表示已消费
func dispatchHelpToolMessage(msg *proto.Message, b inboundSink) bool {
	switch msg.Type {
	case proto.MsgToolResp, proto.MsgToolStream, proto.MsgToolHelloAck:
		if b != nil {
			b.HandleInbound(msg)
		}
		return true
	}
	return false
}

// RunMCPMode 阻塞跑 MCP stdio server，直到 stdin EOF 或隧道断开
// 调用前应已完成协助码 join + 握手；key 为派生出的 session_key
func (h *HelpMode) RunMCPMode(ctx context.Context, key [32]byte) error {
	bridge := mcp.NewBridge(h.client, key)
	// 启动 ReadMessage 循环，把工具消息投给 bridge，其他打日志
	go func() {
		for {
			h.client.SetReadDeadline(time.Now().Add(2 * time.Minute))
			msg, err := h.client.ReadMessage()
			if err != nil {
				return
			}
			if !dispatchHelpToolMessage(msg, bridge) {
				// 此模式下其他消息忽略（无 SSH 流）
			}
		}
	}()

	srv := mcp.NewServer(bridge)
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		return fmt.Errorf("mcp serve: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: 修改 `internal/client/help.go`，在 `Run()` 中根据新字段分流：**

在 `HelpMode` struct 加 `mcpStdio bool`；`NewHelpMode` 暂保持兼容；新增 `NewHelpModeMCP(cfg, code) *HelpMode` 设 mcpStdio=true。在 `Run()` 成功 join 后判断 `if h.mcpStdio { handshake & RunMCPMode }`，否则走原 SSH 监听逻辑。握手实现：

```go
// handshakeTool 工具通道握手；返回 session_key
func (h *HelpMode) handshakeTool() ([32]byte, error) {
	hello := proto.NewHello()
	if err := h.client.SendMessage(proto.MsgToolHello, &hello); err != nil {
		return [32]byte{}, err
	}
	h.client.SetReadDeadline(time.Now().Add(10 * time.Second))
	msg, err := h.client.ReadMessage()
	h.client.SetReadDeadline(time.Time{})
	if err != nil {
		return [32]byte{}, err
	}
	if msg.Type != proto.MsgToolHelloAck {
		return [32]byte{}, fmt.Errorf("expected hello_ack, got %s", msg.Type)
	}
	var ack proto.HelloAck
	proto.DecodePayload(msg, &ack)
	if !ack.Accept {
		return [32]byte{}, fmt.Errorf("share rejected tool channel: %s", ack.ErrorMsg)
	}
	key := proto.DeriveSessionKey(h.code, ack.NonceB64, hello.NonceB64)
	return key, nil
}
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `go test ./internal/client/ -v`
Expected: 全部 PASS（含原 SSH 模式回归）

- [ ] **Step 6: 提交**

```powershell
git add internal/client/help.go internal/client/help_mcp.go internal/client/help_mcp_test.go
git commit -m @'
feat(client/help): --mcp-stdio 模式 + 工具通道握手

新增 NewHelpModeMCP / RunMCPMode：join 成功后跑 HELLO/HELLO_ACK 握手
派生 session_key，启动 mcp.NewServer(bridge) 监听 stdin/stdout，
后台 ReadMessage 循环把 ToolResp/Stream 投给 bridge；原 SSH 监听
路径保持不变。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 17: Share 端实现握手响应 + 启动 Daemon

**Files:**
- Modify: `internal/client/share.go`
- Modify: `internal/agent/agent.go` (可能补 helper)
- Test: `internal/client/share_handshake_test.go`

- [ ] **Step 1: 写失败测试**

```go
package client

import (
	"testing"

	"github.com/remote-assist/tool/internal/proto"
)

func TestHandleHelloProducesAck(t *testing.T) {
	hello := proto.NewHello()
	ack, key := buildHelloAck(hello, "CODE-1234")
	if !ack.Accept {
		t.Fatalf("expected accept, got %+v", ack)
	}
	derived := proto.DeriveSessionKey("CODE-1234", ack.NonceB64, hello.NonceB64)
	if derived != key {
		t.Fatal("key derivation mismatch")
	}
}

func TestHandleHelloRejectsBadVersion(t *testing.T) {
	hello := proto.NewHello()
	hello.Version = "999"
	ack, _ := buildHelloAck(hello, "CODE-1234")
	if ack.Accept {
		t.Fatal("expected reject for unknown version")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `go test ./internal/client/ -v -run TestHandleHello`
Expected: FAIL

- [ ] **Step 3: 在 `internal/client/share.go` 加：**

```go
// buildHelloAck share 端对 Hello 的应答 + 派生 session_key
func buildHelloAck(hello proto.Hello, code string) (proto.HelloAck, [32]byte) {
	if hello.Version != proto.ToolProtocolVersion {
		return proto.HelloAck{
			Version:  proto.ToolProtocolVersion,
			Accept:   false,
			ErrorMsg: "unsupported tool protocol version: " + hello.Version,
		}, [32]byte{}
	}
	ack := proto.HelloAck{
		Version:      proto.ToolProtocolVersion,
		Capabilities: []string{"exec", "read_file", "write_file", "list_dir", "stat", "glob", "grep", "process_list", "tail_log"},
		NonceB64:     proto.NewHello().NonceB64,
		Accept:       true,
	}
	key := proto.DeriveSessionKey(code, ack.NonceB64, hello.NonceB64)
	return ack, key
}
```

并在 `handleTunnel` / `handleTunnelP2P` 的工具分流处增加 Hello 处理：

```go
		case proto.MsgToolHello:
			var hello proto.Hello
			proto.DecodePayload(msg, &hello)
			ack, key := buildHelloAck(hello, s.code)
			s.client.SendMessage(proto.MsgToolHelloAck, &ack)
			if ack.Accept {
				s.startDaemonOnce(key)
			}
```

`startDaemonOnce` 使用 `sync.Once` 包住，从 sandbox + tool registry 构造 daemon、把 daemon 挂到 ShareMode.daemon 字段：

```go
func (s *ShareMode) startDaemonOnce(key [32]byte) {
	s.daemonOnce.Do(func() {
		reg := agent.NewRegistry()
		sb := agent.NewSandbox(s.sbCfg)
		reg.Register(tools.NewExec(sb))
		reg.Register(tools.NewReadFile(sb))
		reg.Register(tools.NewWriteFile(sb))
		reg.Register(tools.NewListDir(sb))
		reg.Register(tools.NewStat(sb))
		reg.Register(tools.NewGlob(sb))
		reg.Register(tools.NewGrep(sb))
		reg.Register(tools.NewProcessList())
		reg.Register(tools.NewTailLog(sb))
		d := agent.NewDaemon(reg, s.client, key)
		s.daemon = d
		go d.RunLoop(context.Background())
	})
}
```

ShareMode 字段补：`sbCfg agent.SandboxConfig`、`daemon *agent.Daemon`、`daemonOnce sync.Once`、`code string`（已有）。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/client/ -v -run TestHandleHello`
Run: `go test ./internal/client/ -v`（回归）
Expected: PASS

- [ ] **Step 5: 提交**

```powershell
git add internal/client/share.go internal/client/share_handshake_test.go
git commit -m @'
feat(client/share): Hello 应答 + Daemon 启动

share 端收到 MsgToolHello 校验协议版本→生成 ack+派生 session_key→
sync.Once 启动 agent.Daemon（注册 9 个工具，挂 sandbox 配置）。
拒绝版本不匹配 / capability 协商失败的 Hello。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 18: CLI flags（share 端新增 5 个 flag）

**Files:**
- Modify: `cmd/remote/main.go`
- Modify: `internal/client/share.go` (NewShareMode 接 sandbox cfg)

- [ ] **Step 1: 修改 `cmd/remote/main.go`，在 `runShare` 添加：**

```go
	rootDir := fs.String("root", "", "Sandbox root for file operations (required unless --unsafe-full-system)")
	allowExec := fs.String("allow-exec", "", "Comma-separated exec basename allowlist (empty = no restriction beyond deny)")
	denyExec := fs.String("deny-exec", "rm,shutdown,reboot,mkfs,dd", "Comma-separated exec basename denylist")
	elevate := fs.Bool("elevate", false, "Windows: request UAC elevation on startup")
	unsafe := fs.Bool("unsafe-full-system", false, "DANGER: disable sandbox entirely")
```

构造 `agent.SandboxConfig`（split CSV by comma）并传给 `NewShareMode(cfg, sshAddr, sbCfg)`。

`unsafe=true` 时打印 5 秒红色横幅倒计时（用 `\033[31m` ANSI；Windows 终端默认支持 VT 序列）：

```go
	if *unsafe {
		fmt.Fprint(os.Stderr, "\033[1;31m!!! DANGER: --unsafe-full-system disables ALL sandboxing.\nFiles, exec commands have NO restriction.\nAborting in 5 seconds — press Ctrl+C to abort.\033[0m\n")
		for i := 5; i > 0; i-- {
			fmt.Fprintf(os.Stderr, "%d... ", i)
			time.Sleep(time.Second)
		}
		fmt.Fprintln(os.Stderr)
	}
```

`rootDir == "" && !unsafe` → 默认设为 CWD，并打印警告"--root not set, defaulting to current working directory"。

- [ ] **Step 2: 修改 `internal/client/share.go`：**

```go
func NewShareMode(cfg *Config, sshAddr string, sbCfg agent.SandboxConfig) *ShareMode {
	return &ShareMode{client: NewClient(cfg), sshAddr: sshAddr, sbCfg: sbCfg}
}
```

更新 `cmd/remote/main.go` 的调用点。

- [ ] **Step 3: 构建并冒烟**

```powershell
go build -o bin/remote.exe ./cmd/remote
.\bin\remote.exe share --help
```

Expected: 看到新 flag 的说明输出。

- [ ] **Step 4: 提交**

```powershell
git add cmd/remote/main.go internal/client/share.go
git commit -m @'
feat(cli): share 加 --root/--allow-exec/--deny-exec/--elevate/--unsafe-full-system

默认 --deny-exec 含 rm,shutdown,reboot,mkfs,dd 五项危险命令。
--unsafe-full-system 启动时强制 5 秒红色横幅倒计时。
--root 未指定且非 unsafe 时默认 CWD 并打印警告。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 19: CLI flags（help 端新增 --mcp-stdio / --legacy-ssh）

**Files:**
- Modify: `cmd/remote/main.go`

- [ ] **Step 1: 在 `runHelp` 添加：**

```go
	mcpStdio := fs.Bool("mcp-stdio", false, "Run as MCP stdio server for Claude Code")
	legacySSH := fs.Bool("legacy-ssh", false, "Force original SSH tunnel mode (default if --mcp-stdio not set)")
```

后续路由：

```go
	if *mcpStdio && *legacySSH {
		fmt.Fprintln(os.Stderr, "Error: --mcp-stdio and --legacy-ssh are mutually exclusive")
		os.Exit(1)
	}
	if *mcpStdio {
		help := client.NewHelpModeMCP(cfg, *code)
		if err := help.RunMCP(context.Background()); err != nil {
			log.Fatalf("Error: %v", err)
		}
		return
	}
	// 默认走原 SSH 监听（与原行为一致）
```

`NewHelpModeMCP` / `RunMCP` 是 Task 16 已经引入的入口；如签名略有调整，对齐即可。

- [ ] **Step 2: 构建并冒烟**

```powershell
go build -o bin/remote.exe ./cmd/remote
.\bin\remote.exe help --help
```

Expected: 看到新 flag。

- [ ] **Step 3: 提交**

```powershell
git add cmd/remote/main.go
git commit -m @'
feat(cli): help 加 --mcp-stdio / --legacy-ssh

--mcp-stdio：跑 MCP stdio server 供 Claude Code .mcp.json 拉起。
--legacy-ssh：强制原 SSH 监听（与未指定时默认行为一致，但语义显式）。
两个 flag 互斥。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 20: Windows 提权（ShellExecuteW runas）

**Files:**
- Create: `internal/agent/elevate_windows.go` (build tag: windows)
- Create: `internal/agent/elevate_other.go` (build tag: !windows)
- Modify: `cmd/remote/main.go` (当 --elevate 时调用)

- [ ] **Step 1: 创建 `internal/agent/elevate_other.go`**

```go
//go:build !windows

package agent

import "fmt"

// RelaunchElevated 非 Windows 上是 no-op + 错误
func RelaunchElevated() error {
	return fmt.Errorf("--elevate is only supported on Windows")
}
```

- [ ] **Step 2: 创建 `internal/agent/elevate_windows.go`**

```go
//go:build windows

package agent

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	shell32         = syscall.NewLazyDLL("shell32.dll")
	procShellExecW  = shell32.NewProc("ShellExecuteW")
)

// RelaunchElevated 用 runas 重新启动自身，传入 -elevated 旗标避免无限递归
func RelaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// 透传所有原 args，附加 "--elevated-child" 内部标记
	args := append(os.Args[1:], "--elevated-child")
	param := strings.Join(args, " ")

	verb, _ := syscall.UTF16PtrFromString("runas")
	exeP, _ := syscall.UTF16PtrFromString(exe)
	paramP, _ := syscall.UTF16PtrFromString(param)

	ret, _, callErr := procShellExecW.Call(
		0, uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(exeP)),
		uintptr(unsafe.Pointer(paramP)),
		0, 1, // SW_SHOWNORMAL
	)
	if ret <= 32 {
		return fmt.Errorf("ShellExecuteW failed: ret=%d err=%v", ret, callErr)
	}
	os.Exit(0)
	return nil
}
```

- [ ] **Step 3: 在 `cmd/remote/main.go` 的 `runShare` 开头加：**

```go
	// 检测 --elevated-child（内部）；不存在 + --elevate=true → 立即提权 relaunch
	hasElevatedChild := false
	for _, a := range args {
		if a == "--elevated-child" {
			hasElevatedChild = true
			break
		}
	}
	if *elevate && !hasElevatedChild {
		if err := agent.RelaunchElevated(); err != nil {
			fmt.Fprintf(os.Stderr, "Elevation failed: %v\nContinuing without elevation.\n", err)
		}
	}
```

显示当前权限级别：

```go
	if hasElevatedChild || isElevated() {
		fmt.Println("Running as: ELEVATED")
	} else {
		fmt.Printf("Running as: %s (non-elevated)\n", currentUserName())
	}
```

`isElevated()` / `currentUserName()` 是简单 helper，可以放 `internal/agent/elevate_*.go`（Windows 检查 TokenElevation，非 Windows 直接返回 `os.Geteuid() == 0`）。

- [ ] **Step 4: 构建并冒烟（Windows）**

```powershell
go build -o bin/remote.exe ./cmd/remote
.\bin\remote.exe share --elevate --root D:\src\test
```

Expected: UAC 弹窗；批准后新窗口显示 "Running as: ELEVATED"。

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/elevate_windows.go internal/agent/elevate_other.go cmd/remote/main.go
git commit -m @'
feat(agent): Windows --elevate 通过 ShellExecuteW runas 自重启

非 Windows 平台返回 unsupported 错误。--elevated-child 旗标防止
无限递归。share 启动时显示当前权限级别（ELEVATED / 用户名）。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 21: 审计日志接入

**Files:**
- Modify: `internal/agent/agent.go` (Daemon 写审计行)
- Modify: `internal/logger/audit.go` (若需扩展 API)

- [ ] **Step 1: 阅读现有 `internal/logger/audit.go`，沿用其接口**

如果现有 audit 已经接受任意 string，直接调用 `audit.Log("tool|...")` 即可。否则在 Daemon.handleReq 起末双端记录：

```go
func (d *Daemon) handleReq(parent context.Context, msg *proto.Message) {
	start := time.Now()
	var req proto.ToolReq
	proto.DecodePayload(msg, &req)
	// ...（前文逻辑）
	defer func() {
		dur := time.Since(start).Milliseconds()
		status := "ok"
		if !resp.OK {
			status = "err:" + resp.ErrorCode
		}
		argsSummary := summarizeArgs(req.Tool, req.ArgsJSON)
		audit.Logf("tool | %s | %s | %dms | %s", req.Tool, argsSummary, dur, status)
	}()
}
```

`summarizeArgs` 对各工具脱敏：read_file/write_file 只记 path + size；exec 记 argv[0]+argc。

- [ ] **Step 2: 单测**

```go
func TestSummarizeArgsExec(t *testing.T) {
	s := summarizeArgs("exec", json.RawMessage(`{"argv":["go","test","./..."]}`))
	if !strings.Contains(s, "go") || strings.Contains(s, "./...") == false {
		t.Fatalf("got %q", s)
	}
}

func TestSummarizeArgsWriteFileNoContent(t *testing.T) {
	s := summarizeArgs("write_file", json.RawMessage(`{"path":"/a","content":"c2VjcmV0"}`))
	if strings.Contains(s, "c2VjcmV0") {
		t.Fatalf("leaked content: %q", s)
	}
}
```

- [ ] **Step 3: 实现 `summarizeArgs`**

```go
func summarizeArgs(tool string, raw json.RawMessage) string {
	var generic map[string]json.RawMessage
	json.Unmarshal(raw, &generic)
	switch tool {
	case "exec":
		var argv []string
		json.Unmarshal(generic["argv"], &argv)
		if len(argv) == 0 {
			return "exec[]"
		}
		return fmt.Sprintf("exec %s argc=%d", argv[0], len(argv))
	case "read_file", "stat", "tail_log", "list_dir":
		var p string
		json.Unmarshal(generic["path"], &p)
		return tool + " " + p
	case "write_file":
		var p string
		var content []byte
		json.Unmarshal(generic["path"], &p)
		json.Unmarshal(generic["content"], &content)
		return fmt.Sprintf("write_file %s bytes=%d", p, len(content))
	default:
		return tool
	}
}
```

- [ ] **Step 4: 运行测试 + 构建**

```powershell
go test ./internal/agent/ -v
go build ./...
```

- [ ] **Step 5: 提交**

```powershell
git add internal/agent/agent.go internal/agent/agent_test.go
git commit -m @'
feat(agent): 审计日志接入，每条 ToolReq 一行

ts | tool | args_summary | duration_ms | ok|err:<code>
write_file/read_file 只记 path+bytes，不泄漏内容。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 22: Share 端实时活动显示

**Files:**
- Modify: `internal/agent/agent.go` (Daemon 增加 ActivityFunc 钩子)
- Modify: `internal/client/share.go` (注入活动打印)

- [ ] **Step 1: 在 `Daemon` 字段加 `OnActivity func(line string)`，在 audit 同一处调用：**

```go
if d.OnActivity != nil {
	d.OnActivity(fmt.Sprintf("[%s] %s: %s (%dms, %s)",
		time.Now().Format("15:04:05"), req.Tool, argsSummary, dur, status))
}
```

- [ ] **Step 2: 在 `ShareMode.startDaemonOnce` 注入：**

```go
d.OnActivity = func(line string) { fmt.Println(line) }
```

- [ ] **Step 3: 构建并冒烟**

```powershell
go build ./...
```

- [ ] **Step 4: 提交**

```powershell
git add internal/agent/agent.go internal/client/share.go
git commit -m @'
feat(agent): share 端实时打印每条工具调用活动行

[HH:MM:SS] exec: go test argc=3 (152ms, ok)
让 share 端用户能肉眼监督 help 端代为 Claude 做了什么。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 23: E2E 集成测试

**Files:**
- Create: `tests/e2e/mcp_e2e_test.go`

- [ ] **Step 1: 写测试**

```go
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2e: 起 relay + share + help-mcp-stdio，从 help 的 stdin 投 MCP 消息，校验 stdout
func TestMCPEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped in -short")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0644)

	// 1. relay
	relayCert := filepath.Join(dir, "cert")
	exec.Command("bin/relay", "--gen-certs", "--certs-dir", relayCert).Run()
	relay := exec.Command("bin/relay", "--listen", ":18443", "--cert", relayCert+"/server.crt", "--key", relayCert+"/server.key")
	relay.Start()
	defer relay.Process.Kill()
	time.Sleep(300 * time.Millisecond)

	// 2. share
	shareOut := &bytes.Buffer{}
	share := exec.Command("bin/remote", "share", "--server", "localhost:18443", "--insecure", "--root", dir)
	share.Stdout = shareOut
	share.Start()
	defer share.Process.Kill()
	time.Sleep(500 * time.Millisecond)

	// 解析协助码
	code := extractCode(shareOut.String())
	if code == "" {
		t.Fatalf("no code in share output: %s", shareOut.String())
	}

	// 3. help --mcp-stdio
	help := exec.Command("bin/remote", "help", "--server", "localhost:18443", "--insecure", "--code", code, "--mcp-stdio")
	stdin, _ := help.StdinPipe()
	stdout, _ := help.StdoutPipe()
	help.Start()
	defer help.Process.Kill()

	// initialize
	stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"))
	// tools/call read_file
	call := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "read_file", "arguments": map[string]any{"path": filepath.Join(dir, "hello.txt")}}}
	b, _ := json.Marshal(call)
	stdin.Write(b)
	stdin.Write([]byte("\n"))

	// 读 stdout，找包含 "world" 的 line
	buf := make([]byte, 4096)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		stdout.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := stdout.Read(buf)
		if n > 0 && strings.Contains(string(buf[:n]), "world") {
			return // PASS
		}
	}
	t.Fatal("did not see read_file result containing 'world' within 10s")
}

func extractCode(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "协助码") {
			parts := strings.Fields(line)
			if len(parts) > 1 {
				return strings.ReplaceAll(parts[len(parts)-1], "-", "")
			}
		}
	}
	return ""
}

// SetReadDeadline 帮助；exec.Cmd 的 stdout 是 *os.File，可以 .SetReadDeadline
```

注：测试要求 `bin/relay` 和 `bin/remote` 已构建。CI 流程里 `go build` 在测试前执行。

- [ ] **Step 2: 在 `run_tests.bat` / `run_tests.sh` 添加 e2e 入口**

```bash
go build -o bin/relay ./cmd/relay
go build -o bin/remote ./cmd/remote
go test ./tests/e2e/... -v -timeout 60s
```

- [ ] **Step 3: 运行**

```powershell
.\run_tests.bat
```

Expected: e2e PASS

- [ ] **Step 4: 提交**

```powershell
git add tests/e2e/mcp_e2e_test.go run_tests.bat run_tests.sh
git commit -m @'
test(e2e): MCP 端到端覆盖：relay+share+help-mcp-stdio read_file

起本地 relay、share --root 临时目录、help --mcp-stdio，通过 stdin
喂 MCP initialize + tools/call read_file，断言 stdout 含返回内容。
集成进 run_tests 脚本。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 24: README 增补 Claude Code 调试章节

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 在 README 现有 "使用方式" 之后插入新章节**

```markdown
## Claude Code 远程调试（新）

让本地 Claude Code 直接调试远端机器，无需在远端开 openssh-server。

### 远端启动 share（带沙箱）

\`\`\`bash
remote share --server relay.example.com:8443 --root /path/to/project --insecure
\`\`\`

控制台会显示协助码（如 `ABCD-EFGHIJ`）与沙箱配置摘要。

### 本地配置 Claude Code

在项目根目录 `.mcp.json`（或全局 `~/.claude/mcp.json`）：

\`\`\`json
{
  "mcpServers": {
    "remote-debug": {
      "command": "remote",
      "args": ["help", "--server", "relay.example.com:8443",
               "--code", "ABCD-EFGHIJ", "--mcp-stdio", "--insecure"]
    }
  }
}
\`\`\`

启动 Claude Code，`/mcp` 应看到 `remote-debug` 服务下的 9 个工具。

### 工具一览

- `exec` —— 远端运行 argv（不过 shell）
- `read_file` / `write_file` —— 受 `--root` 沙箱
- `list_dir` / `stat` / `glob` / `grep` —— 远端文件系统探索
- `process_list` —— 远端进程
- `tail_log` —— 日志尾随（支持 follow）

### 沙箱与安全

- `--root <dir>` 是文件操作的沙箱根；未设置时默认 CWD。
- `--allow-exec a,b,c` / `--deny-exec rm,shutdown,...` 控制可执行命令。
- `--elevate`（仅 Windows）：启动时通过 UAC 请求管理员权限。
- `--unsafe-full-system`：**关闭所有沙箱**，启动时强制 5 秒红色确认倒计时。

### 旧 SSH 模式

`remote help --legacy-ssh` 或不带 `--mcp-stdio` 即为原 SSH 隧道行为，向后兼容。
```

- [ ] **Step 2: 提交**

```powershell
git add README.md
git commit -m @'
docs(readme): 添加 Claude Code 远程调试章节

记录 share --root 沙箱配置、help --mcp-stdio + .mcp.json 接入、
9 个工具一览、--allow/deny-exec/--elevate/--unsafe-full-system
安全开关；保留 --legacy-ssh 旧模式说明。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

---

## Task 25: CI 矩阵验证

**Files:**
- Modify: `.github/workflows/*.yml` (build/test 矩阵，加 e2e step)

- [ ] **Step 1: 阅读现有 workflow，找到 build/test job**

```powershell
type .github/workflows/*.yml
```

- [ ] **Step 2: 在 test job 加 e2e 步骤**

```yaml
      - name: Build binaries for e2e
        run: |
          go build -o bin/relay ./cmd/relay
          go build -o bin/remote ./cmd/remote
      - name: E2E tests
        run: go test ./tests/e2e/... -v -timeout 120s
```

确保 `bin/` 不被 `.gitignore` 中的某项错误吞掉（如有问题，e2e 测试本地构建即可）。

- [ ] **Step 3: 提交并 push**

```powershell
git add .github/workflows
git commit -m @'
ci: 添加 e2e 测试步骤到现有 build/test 矩阵

在 Linux + Windows runner 上构建 relay/remote 二进制并跑
tests/e2e MCP 端到端验证。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
'@
```

观察 CI 结果，红了就修。

---

## Spec coverage 自查

| Spec 章节 | 任务 |
|---|---|
| §1 目标与约束 | 全部任务整体兑现 |
| §2.1 代码变更点 | Task 1-22 一一覆盖 |
| §2.2 不动的部分 | 不动即不改，验证手段：原 `--legacy-ssh` 模式回归测试（Task 16/19） |
| §3.1 握手 | Task 3, 16, 17 |
| §3.2 工具帧 | Task 1, 12 |
| §3.3 工具集 v1 (9 个) | Task 6-11 |
| §3.4 MCP 适配 | Task 14, 15, 16 |
| §4.1 威胁模型 | Task 2 (AEAD), Task 3 (KDF), Task 4 (沙箱), Task 18 (unsafe 红色横幅) |
| §4.2 沙箱实现 | Task 4 (Resolve + EvalSymlinks + Rel) |
| §4.3 Windows 提权 | Task 20 |
| §4.4 审计 | Task 21 |
| §5.1 Share 生命周期 | Task 13, 17 |
| §5.2 Help 生命周期 | Task 16 |
| §5.3 错误码 | Task 5 (classifyError) |
| §5.4 取消语义 | Task 12 (cancels map), Task 15 (ctx done → MsgToolCancel) |
| §6.1 单元测试 | 每个 Task 都有 |
| §6.2 集成测试 | Task 23 |
| §6.3 手工验收 | Task 24 README 步骤 |
| §6.4 性能基线 | Task 23 e2e 现成可加（v2 加 bench） |
| §7 开放问题 | 编码格式（JSON）已在 Task 1 锁定；MCP 版本 (2024-11-05) 已在 Task 14 锁定；fsnotify Windows 兼容（Task 11 兜底）；audit log 路径（Task 21 沿用现有 audit.go） |

无遗漏。

## 实施前提条件

- Go 1.21+
- 项目能在 `D:\src\remote-assist-tool` 通过 `go build ./...` 通过
- 现有 `internal/logger/audit.go` 提供可调用的 `Log(line string)` 或类似接口；若 API 不同，Task 21 第一步先阅读 audit.go 再确定调用形式

## 执行模式

完成后请按 brainstorming skill 引导选择 superpowers:subagent-driven-development 或 superpowers:executing-plans。
