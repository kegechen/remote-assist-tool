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
	msg, err := NewMessage(MsgToolStream, &c)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	var got StreamChunk
	if err := DecodePayload(parsed, &got); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !got.Fin || got.Seq != 7 || string(got.Data) != "hello" {
		t.Fatalf("got %+v", got)
	}
}
