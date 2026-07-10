package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// transferLoop：隧道断两次后自动重连+续传成功，且续传参数正确（upload 置 Offset>0）。
func TestTransferLoopRetriesAndResumesOnTunnelLost(t *testing.T) {
	old := transferBackoffUnit
	transferBackoffUnit = time.Millisecond
	defer func() { transferBackoffUnit = old }()

	var runCalls, reconnectCalls int
	var offsets []int64
	run := func(a json.RawMessage) (json.RawMessage, error) {
		runCalls++
		var fa fileTransferArgs
		_ = json.Unmarshal(a, &fa)
		offsets = append(offsets, fa.Offset)
		if runCalls <= 2 {
			return nil, errors.New("tunnel_lost: no response within deadline")
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	reconnect := func() error { reconnectCalls++; return nil }

	args, _ := json.Marshal(fileTransferArgs{LocalPath: "l", RemotePath: "r", Offset: 0})
	res, err := transferLoop(context.Background(), "upload_file", args, run, reconnect)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("res=%s", res)
	}
	if runCalls != 3 || reconnectCalls != 2 {
		t.Fatalf("runCalls=%d reconnectCalls=%d want 3/2", runCalls, reconnectCalls)
	}
	// 首次 offset=0；两次重连后 upload 续传置 Offset=1（doUploadFile 会再按远端 stat 为准）
	if len(offsets) != 3 || offsets[0] != 0 || offsets[1] != 1 || offsets[2] != 1 {
		t.Fatalf("offsets=%v want [0 1 1]", offsets)
	}
}

// 非隧道错（如 file_not_found）立即返回，不重连。
func TestTransferLoopNonTunnelErrorReturnsImmediately(t *testing.T) {
	var runCalls, reconnectCalls int
	run := func(a json.RawMessage) (json.RawMessage, error) {
		runCalls++
		return nil, errors.New("file_not_found: x")
	}
	reconnect := func() error { reconnectCalls++; return nil }
	args, _ := json.Marshal(fileTransferArgs{LocalPath: "l", RemotePath: "r"})
	if _, err := transferLoop(context.Background(), "download_file", args, run, reconnect); err == nil {
		t.Fatal("want error")
	}
	if runCalls != 1 || reconnectCalls != 0 {
		t.Fatalf("runCalls=%d reconnectCalls=%d want 1/0", runCalls, reconnectCalls)
	}
}

// 持续 tunnel_lost：重连次数耗尽后返回最后错误。
func TestTransferLoopExhaustsReconnects(t *testing.T) {
	old := transferBackoffUnit
	transferBackoffUnit = time.Millisecond
	defer func() { transferBackoffUnit = old }()

	var runCalls, reconnectCalls int
	run := func(a json.RawMessage) (json.RawMessage, error) {
		runCalls++
		return nil, errors.New("tunnel_lost: dead")
	}
	reconnect := func() error { reconnectCalls++; return nil }
	args, _ := json.Marshal(fileTransferArgs{LocalPath: "l", RemotePath: "r"})
	if _, err := transferLoop(context.Background(), "upload_file", args, run, reconnect); err == nil {
		t.Fatal("want error after exhausting reconnects")
	}
	if runCalls != maxTransferReconnects+1 {
		t.Fatalf("runCalls=%d want %d", runCalls, maxTransferReconnects+1)
	}
	if reconnectCalls != maxTransferReconnects {
		t.Fatalf("reconnectCalls=%d want %d", reconnectCalls, maxTransferReconnects)
	}
}

// 重连失败：错误应同时带出重连失败原因与原传输错误。
func TestTransferLoopReconnectFailureWrapsError(t *testing.T) {
	old := transferBackoffUnit
	transferBackoffUnit = time.Millisecond
	defer func() { transferBackoffUnit = old }()

	run := func(a json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("tunnel_lost: dead")
	}
	reconnect := func() error { return errors.New("invalid code") }
	args, _ := json.Marshal(fileTransferArgs{LocalPath: "l", RemotePath: "r"})
	_, err := transferLoop(context.Background(), "upload_file", args, run, reconnect)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "invalid code") || !strings.Contains(err.Error(), "tunnel_lost") {
		t.Fatalf("err should mention both reconnect + transfer failure: %v", err)
	}
}
