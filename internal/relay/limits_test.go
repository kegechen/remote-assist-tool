package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLimitsJSONPartialOverride(t *testing.T) {
	limits, err := ParseLimitsJSON([]byte(`{
		"max_connections_total": 321,
		"reject_audit_sample_every": 25,
		"udp": {"packets_global_rate": 1234}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultLimits()
	if limits.MaxConnectionsTotal != 321 || limits.RejectAuditSampleEvery != 25 {
		t.Fatalf("relay 覆盖未生效: %+v", limits)
	}
	if limits.JoinRatePerIP != defaults.JoinRatePerIP {
		t.Fatal("未指定的 relay 字段应保留默认值")
	}
	if limits.UDP.PacketsGlobalRate != 1234 {
		t.Fatal("UDP 局部覆盖未生效")
	}
	if limits.UDP.WorkerCount != defaults.UDP.WorkerCount {
		t.Fatal("未指定的 UDP 字段应保留默认值")
	}
}

func TestParseLimitsJSONRejectsUnknownAndNonPositive(t *testing.T) {
	if _, err := ParseLimitsJSON([]byte(`{"join_rate_glboal": 1}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("未知字段应被拒绝，实际=%v", err)
	}
	if _, err := ParseLimitsJSON([]byte(`{"join_rate_global": 0}`)); err == nil || !strings.Contains(err.Error(), "join_rate_global") {
		t.Fatalf("显式零值应被拒绝，实际=%v", err)
	}
	if _, err := ParseLimitsJSON([]byte(`{"udp": {"worker_count": -1}}`)); err == nil || !strings.Contains(err.Error(), "udp.worker_count") {
		t.Fatalf("UDP 负值应被拒绝，实际=%v", err)
	}
}

func TestLoadLimitsFileAndRuntimeEnforcement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.json")
	if err := os.WriteFile(path, []byte(`{"max_connections_total": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	limits, err := LoadLimitsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(&Config{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	if !srv.acquireConnSlot("10.0.0.1") || !srv.acquireConnSlot("10.0.0.2") {
		t.Fatal("全局上限以内应放行")
	}
	if srv.acquireConnSlot("10.0.0.3") {
		t.Fatal("自定义 max_connections_total 应在运行时生效")
	}
}

func TestSampleHitEveryOneAndInterval(t *testing.T) {
	for i := uint64(1); i <= 5; i++ {
		if !sampleHit(i, 1) {
			t.Fatalf("sample_every=1 时第 %d 条应记录", i)
		}
	}
	if !sampleHit(1, 3) || sampleHit(2, 3) || sampleHit(3, 3) || !sampleHit(4, 3) {
		t.Fatal("sample_every=3 应记录第 1、4、7... 条")
	}
}

func TestP2PSamplingDoesNotPolluteRejectCounter(t *testing.T) {
	srv := &Server{limits: DefaultLimits()}

	srv.logP2PSampled("p2p test")
	if srv.p2pSampleCtr != 1 || srv.logSampleCtr != 0 {
		t.Fatalf("P2P 采样计数不独立: p2p=%d reject=%d", srv.p2pSampleCtr, srv.logSampleCtr)
	}

	srv.logSampled("reject test")
	if srv.p2pSampleCtr != 1 || srv.logSampleCtr != 1 {
		t.Fatalf("拒绝采样污染了 P2P 计数: p2p=%d reject=%d", srv.p2pSampleCtr, srv.logSampleCtr)
	}
}
