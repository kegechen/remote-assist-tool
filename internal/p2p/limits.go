package p2p

import "fmt"

// Limits 定义 STUN/UDP relay 的运营容量阈值。所有字段必须为正数。
type Limits struct {
	WorkerCount            int     `json:"worker_count"`
	TaskQueueDepth         int     `json:"task_queue_depth"`
	PacketsPerIPRate       float64 `json:"packets_per_ip_rate"`
	PacketsPerIPBurst      float64 `json:"packets_per_ip_burst"`
	PacketsGlobalRate      float64 `json:"packets_global_rate"`
	PacketsGlobalBurst     float64 `json:"packets_global_burst"`
	LimiterMaxKeys         int     `json:"limiter_max_keys"`
	LimiterIdleSeconds     int     `json:"limiter_idle_seconds"`
	MaxRelaySessionsTotal  int     `json:"max_relay_sessions_total"`
	MaxRelaySessionsPerIP  int     `json:"max_relay_sessions_per_ip"`
	RelayBytesPerSession   int     `json:"relay_bytes_per_session"`
	RelayBytesBurstSession int     `json:"relay_bytes_burst_session"`
	RelayBytesGlobal       int     `json:"relay_bytes_global"`
	RelayBytesBurstGlobal  int     `json:"relay_bytes_burst_global"`
	InvalidLogSampleEvery  uint64  `json:"invalid_log_sample_every"`
}

// DefaultLimits 返回当前生产安全基线。
func DefaultLimits() Limits {
	return Limits{
		WorkerCount:            stunWorkerCount,
		TaskQueueDepth:         stunTaskQueueDepth,
		PacketsPerIPRate:       udpRatePerIP,
		PacketsPerIPBurst:      udpBurstPerIP,
		PacketsGlobalRate:      udpRateGlobal,
		PacketsGlobalBurst:     udpBurstGlobal,
		LimiterMaxKeys:         udpLimiterMaxKeys,
		LimiterIdleSeconds:     int(udpLimiterIdle.Seconds()),
		MaxRelaySessionsTotal:  maxRelaySessionsTotal,
		MaxRelaySessionsPerIP:  maxRelaySessionsPerIP,
		RelayBytesPerSession:   relayBytesPerSession,
		RelayBytesBurstSession: relayBytesBurstSession,
		RelayBytesGlobal:       relayBytesGlobal,
		RelayBytesBurstGlobal:  relayBytesBurstGlobal,
		InvalidLogSampleEvery:  invalidLogSampleEvery,
	}
}

func normalizeLimits(l Limits) Limits {
	d := DefaultLimits()
	if l.WorkerCount == 0 {
		l.WorkerCount = d.WorkerCount
	}
	if l.TaskQueueDepth == 0 {
		l.TaskQueueDepth = d.TaskQueueDepth
	}
	if l.PacketsPerIPRate == 0 {
		l.PacketsPerIPRate = d.PacketsPerIPRate
	}
	if l.PacketsPerIPBurst == 0 {
		l.PacketsPerIPBurst = d.PacketsPerIPBurst
	}
	if l.PacketsGlobalRate == 0 {
		l.PacketsGlobalRate = d.PacketsGlobalRate
	}
	if l.PacketsGlobalBurst == 0 {
		l.PacketsGlobalBurst = d.PacketsGlobalBurst
	}
	if l.LimiterMaxKeys == 0 {
		l.LimiterMaxKeys = d.LimiterMaxKeys
	}
	if l.LimiterIdleSeconds == 0 {
		l.LimiterIdleSeconds = d.LimiterIdleSeconds
	}
	if l.MaxRelaySessionsTotal == 0 {
		l.MaxRelaySessionsTotal = d.MaxRelaySessionsTotal
	}
	if l.MaxRelaySessionsPerIP == 0 {
		l.MaxRelaySessionsPerIP = d.MaxRelaySessionsPerIP
	}
	if l.RelayBytesPerSession == 0 {
		l.RelayBytesPerSession = d.RelayBytesPerSession
	}
	if l.RelayBytesBurstSession == 0 {
		l.RelayBytesBurstSession = d.RelayBytesBurstSession
	}
	if l.RelayBytesGlobal == 0 {
		l.RelayBytesGlobal = d.RelayBytesGlobal
	}
	if l.RelayBytesBurstGlobal == 0 {
		l.RelayBytesBurstGlobal = d.RelayBytesBurstGlobal
	}
	if l.InvalidLogSampleEvery == 0 {
		l.InvalidLogSampleEvery = d.InvalidLogSampleEvery
	}
	return l
}

// ValidateLimits 拒绝会关闭安全保护或造成无效容量的配置。
func ValidateLimits(l Limits) error {
	checks := []struct {
		name  string
		value float64
	}{
		{"worker_count", float64(l.WorkerCount)},
		{"task_queue_depth", float64(l.TaskQueueDepth)},
		{"packets_per_ip_rate", l.PacketsPerIPRate},
		{"packets_per_ip_burst", l.PacketsPerIPBurst},
		{"packets_global_rate", l.PacketsGlobalRate},
		{"packets_global_burst", l.PacketsGlobalBurst},
		{"limiter_max_keys", float64(l.LimiterMaxKeys)},
		{"limiter_idle_seconds", float64(l.LimiterIdleSeconds)},
		{"max_relay_sessions_total", float64(l.MaxRelaySessionsTotal)},
		{"max_relay_sessions_per_ip", float64(l.MaxRelaySessionsPerIP)},
		{"relay_bytes_per_session", float64(l.RelayBytesPerSession)},
		{"relay_bytes_burst_session", float64(l.RelayBytesBurstSession)},
		{"relay_bytes_global", float64(l.RelayBytesGlobal)},
		{"relay_bytes_burst_global", float64(l.RelayBytesBurstGlobal)},
		{"invalid_log_sample_every", float64(l.InvalidLogSampleEvery)},
	}
	for _, check := range checks {
		if check.value <= 0 {
			return fmt.Errorf("udp.%s must be greater than zero", check.name)
		}
	}
	return nil
}
