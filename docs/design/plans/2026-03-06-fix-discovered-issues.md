# Remote Assist Tool Bug Fix Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix 11 bugs and design issues discovered during code review: security flaws, architecture conflicts, nil safety, variable shadowing, missing message delimiters, broken tests, and more.

**Architecture:** Targeted fixes to existing files. No new modules. Order: fix broken tests first, then security, correctness, refactoring, enhancements.

**Tech Stack:** Go 1.21+, standard library only

---

### Task 1: Fix broken session_test.go (CreateSession signature mismatch)

`session_test.go` calls `CreateSession` with 3 args but the implementation requires 4 (added `clientID string`). Tests don't compile.

**Files:**
- Modify: `internal/relay/session_test.go`

**Step 1: Fix all CreateSession calls to include the 4th `clientID` argument**

In `session_test.go`, every call to `sm.CreateSession(code, share, ttl)` must become `sm.CreateSession(code, share, ttl, "")`.

Lines to fix:
- Line 39: `sm.CreateSession("TESTCODE", share, 30*time.Minute)` → `sm.CreateSession("TESTCODE", share, 30*time.Minute, "")`
- Line 56: `sm.CreateSession("TESTCODE123", share, 30*time.Minute)` → `sm.CreateSession("TESTCODE123", share, 30*time.Minute, "")`
- Line 73: `sm.CreateSession("TESTJOIN", share, 30*time.Minute)` → `sm.CreateSession("TESTJOIN", share, 30*time.Minute, "")`
- Line 101: `sm.CreateSession("TESTDOUBLE", share, 30*time.Minute)` → `sm.CreateSession("TESTDOUBLE", share, 30*time.Minute, "")`
- Line 116: `sm.CreateSession("TESTCLOSE", share, 30*time.Minute)` → `sm.CreateSession("TESTCLOSE", share, 30*time.Minute, "")`
- Line 134: `sm.CreateSession("EXPIRE1", share, 1*time.Millisecond)` → `sm.CreateSession("EXPIRE1", share, 1*time.Millisecond, "")`
- Line 138: `sm.CreateSession("LONG1", share2, 1*time.Hour)` → `sm.CreateSession("LONG1", share2, 1*time.Hour, "")`
- Line 157: `sm.CreateSession("EXPIRETEST", share, 1*time.Millisecond)` → `sm.CreateSession("EXPIRETEST", share, 1*time.Millisecond, "")`

**Step 2: Verify all tests compile and pass**

Run: `go test ./internal/relay/ -v`
Expected: All 14 tests pass (6 code + 8 session)

**Step 3: Commit**

```
fix: update session tests to match CreateSession 4-arg signature
```

---

### Task 2: Fix insecure `randomString` in session.go

`session.go:232-240` uses `time.Now().UnixNano()` as random source with `time.Sleep(1ns)`. This is predictable and generates correlated characters.

**Files:**
- Modify: `internal/relay/session.go:232-240`

**Step 1: Replace `randomString` with crypto/rand-based implementation**

Add to imports: `"crypto/rand"`, `"math/big"`. Remove `"time"` ONLY if no other code in the file uses it (it does — `time.Now()` is used elsewhere, keep it).

Replace:
```go
func randomString(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[time.Now().UnixNano()%int64(len(charset))]
		time.Sleep(1 * time.Nanosecond)
	}
	return string(b)
}
```

With:
```go
func randomString(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
		b[i] = charset[num.Int64()]
	}
	return string(b)
}
```

**Step 2: Add test for randomness quality**

Add to `session_test.go`:
```go
func TestRandomStringUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s := randomString(8)
		if len(s) != 8 {
			t.Errorf("expected length 8, got %d", len(s))
		}
		if seen[s] {
			t.Errorf("duplicate random string: %s", s)
		}
		seen[s] = true
	}
}
```

**Step 3: Verify**

Run: `go test ./internal/relay/ -run TestRandomString -v`
Expected: PASS

**Step 4: Commit**

```
fix: use crypto/rand for session ID generation instead of time-based
```

---

### Task 3: Fix deterministic `randomString` in p2p/manager.go

`p2p/manager.go:291-297` generates a fixed string `"abcdefghijklmnop"` instead of random bytes.

**Files:**
- Modify: `internal/p2p/manager.go:291-297`

**Step 1: Replace with crypto/rand-based implementation**

Add to imports: `"crypto/rand"`, `"math/big"`.

Replace:
```go
func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return string(b)
}
```

With:
```go
func randomString(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
		b[i] = charset[num.Int64()]
	}
	return string(b)
}
```

**Step 2: Verify compilation**

Run: `go build ./internal/p2p/`
Expected: Success

**Step 3: Commit**

```
fix: use crypto/rand for P2P test packet random string
```

---

### Task 4: Fix `sendMsg` missing newline delimiter

`server.go:339-345` writes JSON without a trailing `\n`. The `json.Decoder` on the client side uses newlines to delimit JSON objects. Without them, messages may stick together.

**Files:**
- Modify: `internal/relay/server.go:339-345`

**Step 1: Add newline after JSON marshal, handle marshal errors**

Replace:
```go
func sendMsg(client *ClientConn, msg *proto.Message) {
	if client == nil || client.Conn == nil {
		return
	}
	data, _ := json.Marshal(msg)
	client.Conn.Write(data)
}
```

With:
```go
func sendMsg(client *ClientConn, msg *proto.Message) {
	if client == nil || client.Conn == nil {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal message: %v", err)
		return
	}
	data = append(data, '\n')
	client.Conn.Write(data)
}
```

**Step 2: Verify compilation**

Run: `go build ./internal/relay/`
Expected: Success

**Step 3: Commit**

```
fix: add newline delimiter to server sendMsg for proper JSON framing
```

---

### Task 5: Remove conflicting raw byte tunnel

`server.go:249-250` starts a raw byte `Tunnel` that competes with the JSON message-based `handleTunnelData` router. Both read from the same connection simultaneously — a data race. The JSON routing is what clients actually use.

**Files:**
- Modify: `internal/relay/server.go:249-250`
- Delete: `internal/relay/tunnel.go`

**Step 1: Remove tunnel.Start() from handleJoin**

In `server.go` `handleJoin`, remove these two lines at the end of the function:
```go
	tunnel := NewTunnel(session.Share, client)
	tunnel.Start()
```

**Step 2: Delete tunnel.go**

Delete the file `internal/relay/tunnel.go`.

**Step 3: Verify compilation and tests**

Run: `go build ./internal/relay/ && go test ./internal/relay/ -v`
Expected: Build success, all tests pass

**Step 4: Commit**

```
fix: remove conflicting raw byte tunnel, use JSON message routing only
```

---

### Task 6: Fix variable shadowing in client.Connect()

`client.go:51-58`: `tlsConfig, err :=` declares a new local `err` that shadows the outer one. If `NewTLSClientConfig` fails, `tls.Dial` is still called with nil config.

**Files:**
- Modify: `internal/client/client.go:44-69`

**Step 1: Fix the shadowed error variable**

Replace the Connect method body:
```go
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var conn net.Conn
	var err error

	if c.config.UseTLS {
		var tlsConfig *tls.Config
		tlsConfig, err = crypto.NewTLSClientConfig(c.config.InsecureSkip, c.config.CAFile)
		if err != nil {
			return fmt.Errorf("failed to create TLS config: %w", err)
		}
		conn, err = tls.Dial("tcp", c.config.ServerAddr, tlsConfig)
	} else {
		conn, err = net.Dial("tcp", c.config.ServerAddr)
	}

	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	c.conn = conn
	c.enc = json.NewEncoder(conn)
	c.dec = json.NewDecoder(conn)
	return nil
}
```

**Step 2: Verify compilation**

Run: `go build ./internal/client/`
Expected: Success

**Step 3: Commit**

```
fix: resolve variable shadowing in client.Connect() TLS path
```

---

### Task 7: Fix nil safety in CleanupExpired and Close() panic handling

Two related safety issues:
- `session.go:211-212`: `CleanupExpired` checks `session.Share != nil` but not `session.Share.Conn != nil`
- `client.go:88-90`: `Close()` silently swallows all panics via `recover()`

**Files:**
- Modify: `internal/relay/session.go:211-213`
- Modify: `internal/client/client.go:88-90`

**Step 1: Fix CleanupExpired nil check**

In `session.go` `CleanupExpired`, change:
```go
			if session.Share != nil {
				session.Share.Conn.Close()
			}
			if session.Help != nil {
				session.Help.Conn.Close()
			}
```
to:
```go
			if session.Share != nil && session.Share.Conn != nil {
				session.Share.Conn.Close()
			}
			if session.Help != nil && session.Help.Conn != nil {
				session.Help.Conn.Close()
			}
```

**Step 2: Fix Close() panic handling**

In `client.go` `Close`, change:
```go
		defer func() {
			recover()
		}()
```
to:
```go
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Recovered panic during connection close: %v", r)
			}
		}()
```

**Step 3: Verify compilation and tests**

Run: `go build ./... && go test ./internal/relay/ -v`
Expected: Success

**Step 4: Commit**

```
fix: add nil safety to CleanupExpired, log recovered panics in Close
```

---

### Task 8: Fix self-signed cert missing IPAddresses

`tls.go:82`: `IPAddresses: nil` means TLS verification fails when connecting via IP address `127.0.0.1`. The `DNSNames` field contains `"127.0.0.1"` but IP addresses need to be in `IPAddresses`.

**Files:**
- Modify: `internal/crypto/tls.go:80-82`

**Step 1: Add IPAddresses and fix DNSNames**

Add `"net"` to imports.

Change:
```go
		DNSNames:              []string{"localhost", "127.0.0.1"},
		IPAddresses:           nil,
```
to:
```go
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
```

**Step 2: Verify compilation**

Run: `go build ./internal/crypto/`
Expected: Success

**Step 3: Commit**

```
fix: add IPAddresses to self-signed cert for proper IP-based TLS verification
```

---

### Task 9: Add SessionManager method for peer address handling (encapsulation)

`server.go:263` directly accesses `s.sessions.mu` and iterates `s.sessions.sessions`, breaking encapsulation.

**Files:**
- Modify: `internal/relay/session.go` (add new method)
- Modify: `internal/relay/server.go:253-286` (use new method)

**Step 1: Add UpdatePeerAddr to SessionManager**

Add to `session.go`:
```go
// PeerAddrUpdate contains the result of a peer address update
type PeerAddrUpdate struct {
	Peer        *ClientConn
	IsShareSide bool
}

// UpdatePeerAddr updates a client's peer addresses and returns the paired client info
func (sm *SessionManager) UpdatePeerAddr(clientID string, publicAddr, privateAddr string) *PeerAddrUpdate {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, session := range sm.sessions {
		if session.Share != nil && session.Share.ID == clientID {
			session.SharePublicAddr = publicAddr
			session.SharePrivateAddr = privateAddr
			if session.Help != nil {
				return &PeerAddrUpdate{Peer: session.Help, IsShareSide: true}
			}
			return nil
		}
		if session.Help != nil && session.Help.ID == clientID {
			session.HelpPublicAddr = publicAddr
			session.HelpPrivateAddr = privateAddr
			if session.Share != nil {
				return &PeerAddrUpdate{Peer: session.Share, IsShareSide: false}
			}
			return nil
		}
	}
	return nil
}
```

**Step 2: Refactor handlePeerAddrAdvertise to use new method**

In `server.go`, replace `handlePeerAddrAdvertise`:
```go
func (s *Server) handlePeerAddrAdvertise(client *ClientConn, msg *proto.Message) {
	var advert proto.PeerAddrAdvertise
	if err := proto.DecodePayload(msg, &advert); err != nil {
		return
	}

	log.Printf("Received peer address from %s: public=%s, private=%s", client.ID, advert.PublicAddr, advert.PrivateAddr)

	update := s.sessions.UpdatePeerAddr(client.ID, advert.PublicAddr, advert.PrivateAddr)
	if update != nil {
		s.sendPeerAddrReady(update.Peer, advert.PublicAddr, advert.PrivateAddr, update.IsShareSide)
	}
}
```

**Step 3: Verify compilation and tests**

Run: `go build ./internal/relay/ && go test ./internal/relay/ -v`
Expected: Success

**Step 4: Commit**

```
refactor: encapsulate peer address updates behind SessionManager API
```

---

### Task 10: Simplify CodeManager to stateless generator

`CodeManager` maintains its own code→CodeInfo map with TTL and cleanup, but `SessionManager` already tracks codes via `byCode` map. `CodeManager.Validate()` and `Invalidate()` are never called. This creates redundant state and two cleanup loops.

**Files:**
- Modify: `internal/relay/code.go`
- Modify: `internal/relay/code_test.go`
- Modify: `internal/relay/server.go` (update NewCodeManager call)

**Step 1: Simplify CodeManager**

Replace `internal/relay/code.go` with:
```go
package relay

import (
	"crypto/rand"
	"math/big"
	"strings"
)

const (
	charset           = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	defaultCodeLength = 10
)

// CodeManager generates assistance codes
type CodeManager struct {
	codeLength int
}

// NewCodeManager creates a code manager
func NewCodeManager(codeLength int) *CodeManager {
	if codeLength <= 0 {
		codeLength = defaultCodeLength
	}
	return &CodeManager{
		codeLength: codeLength,
	}
}

// Generate generates a new assistance code
func (cm *CodeManager) Generate() (string, error) {
	return generateCode(cm.codeLength)
}

func generateCode(length int) (string, error) {
	result := make([]byte, length)
	for i := range result {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		result[i] = charset[num.Int64()]
	}
	return string(result), nil
}

func normalizeCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '_' {
			return -1
		}
		return r
	}, code)
}

// FormatCode formats a code for display
func FormatCode(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[:4] + "-" + code[4:]
}
```

**Step 2: Update NewServer in server.go**

Change:
```go
codes: NewCodeManager(cfg.CodeTTL, cfg.CodeLength),
```
to:
```go
codes: NewCodeManager(cfg.CodeLength),
```

**Step 3: Update code_test.go**

Replace with tests that match the new simplified API:
```go
package relay

import (
	"strings"
	"testing"
)

func TestCodeGeneration(t *testing.T) {
	cm := NewCodeManager(10)

	code, err := cm.Generate()
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if len(code) != 10 {
		t.Errorf("Expected code length 10, got %d", len(code))
	}

	// Verify character set
	validChars := "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	for _, c := range code {
		if !strings.ContainsRune(validChars, c) {
			t.Errorf("Invalid character in code: %c", c)
		}
	}

	// Verify no confusing characters
	confusing := "IiLlOo01"
	for _, c := range code {
		if strings.ContainsRune(confusing, c) {
			t.Errorf("Found confusing character: %c", c)
		}
	}
}

func TestCodeUniqueness(t *testing.T) {
	cm := NewCodeManager(10)
	codes := make(map[string]bool)

	for i := 0; i < 100; i++ {
		code, err := cm.Generate()
		if err != nil {
			t.Fatalf("Generate failed: %v", err)
		}
		if codes[code] {
			t.Errorf("Duplicate code: %s", code)
		}
		codes[code] = true
	}
}

func TestNormalizeCode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"ABCD-EFGHIJ", "ABCDEFGHIJ"},
		{"ABCD EFGHIJ", "ABCDEFGHIJ"},
		{"ABCD_EFGHIJ", "ABCDEFGHIJ"},
		{"ABCDEFGHIJ", "ABCDEFGHIJ"},
	}

	for _, tt := range tests {
		result := normalizeCode(tt.input)
		if result != tt.expected {
			t.Errorf("normalizeCode(%s) = %s, want %s", tt.input, result, tt.expected)
		}
	}
}

func TestFormatCode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"ABCDEFGHIJ", "ABCD-EFGHIJ"},
		{"ABCD", "ABCD"},
		{"", ""},
	}

	for _, tt := range tests {
		result := FormatCode(tt.input)
		if result != tt.expected {
			t.Errorf("FormatCode(%s) = %s, want %s", tt.input, result, tt.expected)
		}
	}
}
```

**Step 4: Verify all tests pass**

Run: `go build ./... && go test ./... -v`
Expected: All tests pass

**Step 5: Commit**

```
refactor: simplify CodeManager to stateless generator, remove redundant storage
```

---

### Task 11: Add graceful shutdown to server

Server's `Start()` uses an infinite `for` loop with no way to stop. No signal handling, no context support.

**Files:**
- Modify: `internal/relay/server.go` (add context-based shutdown)
- Modify: `cmd/relay/main.go` (add signal handling)

**Step 1: Add context support to Server.Start**

In `server.go`, add `"context"` to imports.

Replace `Start()` method:
```go
func (s *Server) Start() error {
	return s.StartWithContext(context.Background())
}

func (s *Server) StartWithContext(ctx context.Context) error {
	// Start STUN server if configured
	if s.config.STUNListenAddr != "" {
		var err error
		s.stunServer, err = p2p.NewSTUNServer(s.config.STUNListenAddr)
		if err != nil {
			log.Printf("Warning: failed to start STUN server: %v", err)
		} else {
			log.Printf("STUN server listening on %s", s.stunServer.LocalAddr())
			defer s.stunServer.Close()
		}
	}

	var listener net.Listener
	var err error

	if s.config.UseTLS && s.config.TLSCertFile != "" && s.config.TLSKeyFile != "" {
		tlsConfig, tlsErr := crypto.NewTLSConfig(s.config.TLSCertFile, s.config.TLSKeyFile)
		if tlsErr != nil {
			return fmt.Errorf("failed to create TLS config: %w", tlsErr)
		}
		listener, err = tls.Listen("tcp", s.config.ListenAddr, tlsConfig)
	} else {
		listener, err = net.Listen("tcp", s.config.ListenAddr)
	}

	if err != nil {
		return err
	}

	log.Printf("Server starting on %s", s.config.ListenAddr)
	go s.cleanupLoop()

	// Close listener when context is cancelled
	go func() {
		<-ctx.Done()
		log.Printf("Shutting down server...")
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				log.Printf("Server stopped")
				return nil
			default:
				log.Printf("Accept error: %v", err)
				continue
			}
		}
		go s.handleConn(conn)
	}
}
```

**Step 2: Add signal handling to cmd/relay/main.go**

Add `"context"`, `"os/signal"`, `"syscall"` to imports.

Replace the server start section in `main()`:
```go
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	err = server.StartWithContext(ctx)
	if err != nil {
		log.Fatalf("Server error: %v", err)
	}
```

**Step 3: Fix the variable shadowing in the original Start (TLS err)**

Note: The original Start had the same `err` shadowing as client.Connect. The rewrite above fixes it using `tlsErr`.

**Step 4: Verify compilation**

Run: `go build ./...`
Expected: Success

**Step 5: Commit**

```
feat: add graceful shutdown with signal handling (SIGINT/SIGTERM)
```
