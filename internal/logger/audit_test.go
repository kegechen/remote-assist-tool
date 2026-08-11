package logger

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type blockingLogWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (writer *blockingLogWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.started) })
	<-writer.release
	return len(data), nil
}

func TestAuditFileDoesNotHoldLockWhileLogOutputBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewAuditLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()

	writer := &blockingLogWriter{started: make(chan struct{}), release: make(chan struct{})}
	previousOutput := log.Writer()
	log.SetOutput(writer)
	defer log.SetOutput(previousOutput)

	done := make(chan struct{}, 2)
	go func() {
		audit.Log(AuditEvent{Level: AuditLevelInfo, Event: "first", Message: "first"})
		done <- struct{}{}
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("first log write did not block")
	}
	go func() {
		audit.Log(AuditEvent{Level: AuditLevelInfo, Event: "second", Message: "second"})
		done <- struct{}{}
	}()

	deadline := time.Now().Add(time.Second)
	for {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Count(data, []byte("\n")) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second audit record did not reach disk while output blocked: %q", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(writer.release)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("audit Log did not return after output release")
		}
	}
}

// TestCodeFingerprintImplicitInit 未显式初始化审计密钥时，
// CodeFingerprint 仍应非空、稳定、恒为 24 个十六进制字符（96 bit）。
func TestCodeFingerprintImplicitInit(t *testing.T) {
	const code = "abc123def0"

	first := CodeFingerprint(code)
	if first == "" {
		t.Fatal("CodeFingerprint 返回空串")
	}
	if len(first) != 24 {
		t.Fatalf("CodeFingerprint 长度=%d，期望 24", len(first))
	}
	for _, c := range first {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("CodeFingerprint 含非十六进制字符: %q", first)
		}
	}

	second := CodeFingerprint(code)
	if second != first {
		t.Fatalf("CodeFingerprint 不稳定: %q != %q", first, second)
	}
}

// TestCodeFingerprintDistinct 不同协助码应得到不同指纹。
func TestCodeFingerprintDistinct(t *testing.T) {
	a := CodeFingerprint("code-aaaaaa")
	b := CodeFingerprint("code-bbbbbb")
	if a == b {
		t.Fatalf("不同码得到相同指纹: %q", a)
	}
}

// TestCodeFingerprintConcurrent 并发调用应安全且结果一致。
func TestCodeFingerprintConcurrent(t *testing.T) {
	const code = "concurrent-code"
	const n = 64

	var wg sync.WaitGroup
	results := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = CodeFingerprint(code)
		}(i)
	}
	wg.Wait()

	want := results[0]
	if len(want) != 24 {
		t.Fatalf("并发指纹长度=%d，期望 24", len(want))
	}
	for i, got := range results {
		if got != want {
			t.Fatalf("并发指纹不一致: results[%d]=%q != %q", i, got, want)
		}
	}
}

// TestHmacSumStableAndKeyed hmacSum 同 key 稳定、异 key 异值。
// 直接按值传入固定密钥，绕开 auditKeyOnce。
func TestHmacSumStableAndKeyed(t *testing.T) {
	var key1, key2 [32]byte
	for i := range key1 {
		key1[i] = byte(i)
		key2[i] = byte(255 - i)
	}
	const msg = "fixed-message"

	a1 := hmacSum(key1, msg)
	a2 := hmacSum(key1, msg)
	if a1 != a2 {
		t.Fatal("hmacSum 同 key 同输入结果不稳定")
	}

	b := hmacSum(key2, msg)
	if a1 == b {
		t.Fatal("hmacSum 不同 key 得到相同摘要")
	}
}

// TestMaskCode MaskCode 返回固定掩码，不泄露原码。
func TestMaskCode(t *testing.T) {
	got := MaskCode("secret1234")
	if got == "" {
		t.Fatal("MaskCode 返回空串")
	}
	if got == "secret1234" {
		t.Fatal("MaskCode 泄露了原始协助码")
	}
}
