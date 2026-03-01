package proto

import (
	"testing"
)

func TestNewMessage(t *testing.T) {
	payload := &RegisterRequest{ClientID: "test-client"}
	msg, err := NewMessage(MsgRegisterRequest, payload)
	if err != nil {
		t.Fatalf("NewMessage failed: %v", err)
	}
	if msg.Type != MsgRegisterRequest {
		t.Error("Message type mismatch")
	}
	if len(msg.Payload) == 0 {
		t.Error("Payload should not be empty")
	}
}

func TestMessageWithoutPayload(t *testing.T) {
	msg, err := NewMessage(MsgHeartbeat, nil)
	if err != nil {
		t.Fatalf("NewMessage without payload failed: %v", err)
	}
	if msg.Type != MsgHeartbeat {
		t.Error("Message type mismatch")
	}
}

func TestParseMessage(t *testing.T) {
	original := &Message{
		Type:    MsgJoinRequest,
		Payload: MustMarshal(&JoinRequest{Code: "TESTCODE"}),
	}
	data := MustMarshal(original)

	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("ParseMessage failed: %v", err)
	}
	if parsed.Type != original.Type {
		t.Error("Parsed type mismatch")
	}
}

func TestDecodePayload(t *testing.T) {
	expected := &JoinResponse{
		Success:   true,
		SessionID: "ses-test-123",
	}
	msg, _ := NewMessage(MsgJoinResponse, expected)

	var actual JoinResponse
	err := DecodePayload(msg, &actual)
	if err != nil {
		t.Fatalf("DecodePayload failed: %v", err)
	}
	if actual.Success != expected.Success {
		t.Error("Success field mismatch")
	}
	if actual.SessionID != expected.SessionID {
		t.Error("SessionID field mismatch")
	}
}

func TestTunnelData(t *testing.T) {
	testData := []byte("this is test tunnel data")
	dataMsg := &TunnelData{Data: testData}

	msg, err := NewMessage(MsgTunnelData, dataMsg)
	if err != nil {
		t.Fatalf("NewMessage for tunnel data failed: %v", err)
	}

	var decoded TunnelData
	if err := DecodePayload(msg, &decoded); err != nil {
		t.Fatalf("Decode tunnel data failed: %v", err)
	}

	if string(decoded.Data) != string(testData) {
		t.Error("Tunnel data mismatch")
	}
}

func TestErrorMessage(t *testing.T) {
	errMsg := &ErrorMessage{
		Code:    "TEST_ERROR",
		Message: "This is a test error",
	}

	msg, _ := NewMessage(MsgError, errMsg)

	var decoded ErrorMessage
	DecodePayload(msg, &decoded)

	if decoded.Code != errMsg.Code {
		t.Error("Error code mismatch")
	}
	if decoded.Message != errMsg.Message {
		t.Error("Error message mismatch")
	}
}

func TestHeartbeat(t *testing.T) {
	hb := &Heartbeat{Timestamp: 1234567890}
	msg, _ := NewMessage(MsgHeartbeat, hb)

	var decoded Heartbeat
	DecodePayload(msg, &decoded)

	if decoded.Timestamp != hb.Timestamp {
		t.Error("Heartbeat timestamp mismatch")
	}
}

func TestMessageRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		msgType MessageType
		payload interface{}
	}{
		{
			name:    "RegisterRequest",
			msgType: MsgRegisterRequest,
			payload: &RegisterRequest{ClientID: "cli-123"},
		},
		{
			name:    "RegisterResponse",
			msgType: MsgRegisterResponse,
			payload: &RegisterResponse{Code: "ABCDEFGHIJ", ExpiresAt: 1234567890},
		},
		{
			name:    "JoinRequest",
			msgType: MsgJoinRequest,
			payload: &JoinRequest{Code: "ABCDEFGHIJ"},
		},
		{
			name:    "JoinResponseSuccess",
			msgType: MsgJoinResponse,
			payload: &JoinResponse{Success: true, SessionID: "ses-123"},
		},
		{
			name:    "JoinResponseError",
			msgType: MsgJoinResponse,
			payload: &JoinResponse{Success: false, Error: "code expired"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := NewMessage(tt.msgType, tt.payload)
			if err != nil {
				t.Fatalf("NewMessage failed: %v", err)
			}

			data := MustMarshal(msg)

			parsed, err := ParseMessage(data)
			if err != nil {
				t.Fatalf("ParseMessage failed: %v", err)
			}

			if parsed.Type != tt.msgType {
				t.Errorf("Type mismatch: got %s, want %s", parsed.Type, tt.msgType)
			}
		})
	}
}
