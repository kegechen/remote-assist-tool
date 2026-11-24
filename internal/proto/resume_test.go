package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func resumeTestSession(b byte) Session {
	var s Session
	for i := range s.Key {
		s.Key[i] = b
	}
	s.Version = ToolProtocolVersion
	return s
}

func TestP2PResumeProofRoundTrip(t *testing.T) {
	sess := resumeTestSession(7)
	p, err := NewP2PResumeProof(sess, "ABCD1234")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyP2PResumeProof(p, sess, "ABCD1234") {
		t.Fatal("同一会话密钥与协助码生成的证明应校验通过")
	}
	ack := NewP2PResumeAck(p, sess, "ABCD1234")
	if !VerifyP2PResumeAck(ack, p, sess, "ABCD1234") {
		t.Fatal("share 回的确认应校验通过")
	}
}

// 另一个只知道协助码、没参与那次握手的协助端（会话密钥不同）造不出能通过的证明。
func TestP2PResumeProofRejectsOtherSession(t *testing.T) {
	a, b := resumeTestSession(1), resumeTestSession(2)
	p, err := NewP2PResumeProof(b, "ABCD1234")
	if err != nil {
		t.Fatal(err)
	}
	if VerifyP2PResumeProof(p, a, "ABCD1234") {
		t.Fatal("别的会话密钥生成的证明必须被拒绝")
	}
	if VerifyP2PResumeProof(p, b, "WXYZ9876") {
		t.Fatal("换了协助码，证明必须被拒绝")
	}
	tampered := *p
	tampered.Nonce = strings.Repeat("A", len(p.Nonce))
	if VerifyP2PResumeProof(&tampered, b, "ABCD1234") {
		t.Fatal("篡改 nonce 后证明必须被拒绝")
	}
}

// 方向标签防反射：help 的证明不能当 share 的确认用，确认也必须回显本次 nonce。
func TestP2PResumeAckBindsDirectionAndNonce(t *testing.T) {
	sess := resumeTestSession(3)
	p1, _ := NewP2PResumeProof(sess, "ABCD1234")
	p2, _ := NewP2PResumeProof(sess, "ABCD1234")
	if p1.Nonce == p2.Nonce {
		t.Fatal("每次生成的 nonce 应不同")
	}
	reflected := &P2PResumeAck{Nonce: p1.Nonce, MAC: p1.MAC}
	if VerifyP2PResumeAck(reflected, p1, sess, "ABCD1234") {
		t.Fatal("把证明原样反射回来不能当确认")
	}
	if VerifyP2PResumeAck(NewP2PResumeAck(p2, sess, "ABCD1234"), p1, sess, "ABCD1234") {
		t.Fatal("别的那次续会话的确认不能拿来用")
	}
	if VerifyP2PResumeAck(nil, p1, sess, "ABCD1234") {
		t.Fatal("nil 确认必须被拒绝")
	}
	ack := NewP2PResumeAck(p1, sess, "ABCD1234")
	if VerifyP2PResumeProof(&P2PResumeProof{Nonce: ack.Nonce, MAC: ack.MAC}, sess, "ABCD1234") {
		t.Fatal("share 的确认反射回去不能当证明")
	}
	if VerifyP2PResumeAck(NewP2PResumeAck(p1, resumeTestSession(9), "ABCD1234"), p1, sess, "ABCD1234") {
		t.Fatal("别的会话密钥算出的确认必须被拒绝")
	}
	if VerifyP2PResumeAck(NewP2PResumeAck(p1, sess, "WXYZ9876"), p1, sess, "ABCD1234") {
		t.Fatal("换了协助码算出的确认必须被拒绝")
	}
}

// 会话未激活（零密钥）时既不能生成也不能校验通过：零密钥谁都「知道」。
func TestP2PResumeProofRequiresActiveSession(t *testing.T) {
	if _, err := NewP2PResumeProof(Session{}, "ABCD1234"); err == nil {
		t.Fatal("零密钥会话不应生成证明")
	}
	forged := &P2PResumeProof{Nonce: "bm9uY2U", MAC: computeResumeMAC(Session{}, "ABCD1234", "bm9uY2U", resumeDirProof)}
	if VerifyP2PResumeProof(forged, Session{}, "ABCD1234") {
		t.Fatal("零密钥会话下的证明必须一律拒绝")
	}
	sess := resumeTestSession(4)
	p, _ := NewP2PResumeProof(sess, "ABCD1234")
	if VerifyP2PResumeProof(p, sess, "") {
		t.Fatal("协助码为空时必须拒绝")
	}
}

func TestP2PResumeProofWellFormed(t *testing.T) {
	sess := resumeTestSession(5)
	p, _ := NewP2PResumeProof(sess, "ABCD1234")
	if !p.WellFormed() {
		t.Fatal("正常生成的证明应通过形状检查")
	}
	for name, bad := range map[string]*P2PResumeProof{
		"nil":      nil,
		"空 nonce":  {Nonce: "", MAC: p.MAC},
		"空 MAC":    {Nonce: p.Nonce, MAC: ""},
		"超长 nonce": {Nonce: strings.Repeat("A", maxResumeFieldLen+1), MAC: p.MAC},
		"超长 MAC":   {Nonce: p.Nonce, MAC: strings.Repeat("A", maxResumeFieldLen+1)},
	} {
		if bad.WellFormed() {
			t.Fatalf("%s 应判为畸形", name)
		}
	}
}

// 不带证明的 JoinRequest 序列化结果不能多出字段：普通加入照旧，旧 relay 看到的报文不变。
func TestJoinRequestOmitsEmptyResumeProof(t *testing.T) {
	raw, err := json.Marshal(&JoinRequest{Code: "ABCD1234"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "resume_proof") {
		t.Fatalf("没有证明时不应输出 resume_proof: %s", raw)
	}
	sess := resumeTestSession(6)
	p, _ := NewP2PResumeProof(sess, "ABCD1234")
	raw, _ = json.Marshal(&SessionReady{SessionID: "s1", ResumeProof: p})
	var back SessionReady
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !VerifyP2PResumeProof(back.ResumeProof, sess, "ABCD1234") {
		t.Fatal("SessionReady 往返序列化后证明应仍可校验")
	}
}
