package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/remote-assist/tool/internal/p2p"
)

// Limits 定义 relay 的运营容量阈值。零值 Config 仍使用 DefaultLimits。
type Limits struct {
	MaxConnectionsTotal       int        `json:"max_connections_total"`
	MaxConnectionsPerIP       int        `json:"max_connections_per_ip"`
	MaxJoinFailures           int        `json:"max_join_failures"`
	JoinRatePerIP             float64    `json:"join_rate_per_ip"`
	JoinBurstPerIP            float64    `json:"join_burst_per_ip"`
	JoinRateGlobal            float64    `json:"join_rate_global"`
	JoinBurstGlobal           float64    `json:"join_burst_global"`
	RejectAuditSampleEvery    uint64     `json:"reject_audit_sample_every"`
	CreateRatePerIP           float64    `json:"create_rate_per_ip"`
	CreateBurstPerIP          float64    `json:"create_burst_per_ip"`
	CreateRateGlobal          float64    `json:"create_rate_global"`
	CreateBurstGlobal         float64    `json:"create_burst_global"`
	MaxActiveSessionsPerIP    int        `json:"max_active_sessions_per_ip"`
	MaxActiveSessionsTotal    int        `json:"max_active_sessions_total"`
	HeartbeatRatePerIP        float64    `json:"heartbeat_rate_per_ip"`
	HeartbeatBurstPerIP       float64    `json:"heartbeat_burst_per_ip"`
	HeartbeatRateGlobal       float64    `json:"heartbeat_rate_global"`
	HeartbeatBurstGlobal      float64    `json:"heartbeat_burst_global"`
	DataRatePerConnection     float64    `json:"data_rate_per_connection"`
	DataBurstPerConnection    float64    `json:"data_burst_per_connection"`
	DataRateGlobal            float64    `json:"data_rate_global"`
	DataBurstGlobal           float64    `json:"data_burst_global"`
	ControlRatePerConnection  float64    `json:"control_rate_per_connection"`
	ControlBurstPerConnection float64    `json:"control_burst_per_connection"`
	ControlRateGlobal         float64    `json:"control_rate_global"`
	ControlBurstGlobal        float64    `json:"control_burst_global"`
	LimiterMaxKeys            int        `json:"limiter_max_keys"`
	LimiterIdleSeconds        int        `json:"limiter_idle_seconds"`
	UDP                       p2p.Limits `json:"udp"`
}

// DefaultLimits 返回当前生产安全基线。
func DefaultLimits() Limits {
	return Limits{
		MaxConnectionsTotal:       maxConnsTotal,
		MaxConnectionsPerIP:       maxConnsPerIP,
		MaxJoinFailures:           maxJoinFailures,
		JoinRatePerIP:             joinRatePerIP,
		JoinBurstPerIP:            joinBurstPerIP,
		JoinRateGlobal:            joinRateGlobal,
		JoinBurstGlobal:           joinBurstGlobal,
		RejectAuditSampleEvery:    rejectAuditSampleN,
		CreateRatePerIP:           createRatePerIP,
		CreateBurstPerIP:          createBurstPerIP,
		CreateRateGlobal:          createRateGlobal,
		CreateBurstGlobal:         createBurstGlobal,
		MaxActiveSessionsPerIP:    maxActiveSessionsPerIP,
		MaxActiveSessionsTotal:    maxActiveSessionsTotal,
		HeartbeatRatePerIP:        heartbeatRatePerIP,
		HeartbeatBurstPerIP:       heartbeatBurstPerIP,
		HeartbeatRateGlobal:       heartbeatRateGlobal,
		HeartbeatBurstGlobal:      heartbeatBurstGlobal,
		DataRatePerConnection:     tunnelDataRatePerConn,
		DataBurstPerConnection:    tunnelDataBurstPerConn,
		DataRateGlobal:            tunnelDataRateGlobal,
		DataBurstGlobal:           tunnelDataBurstGlobal,
		ControlRatePerConnection:  controlRatePerConn,
		ControlBurstPerConnection: controlBurstPerConn,
		ControlRateGlobal:         controlRateGlobal,
		ControlBurstGlobal:        controlBurstGlobal,
		LimiterMaxKeys:            limiterMaxKeys,
		LimiterIdleSeconds:        int(limiterIdle.Seconds()),
		UDP:                       p2p.DefaultLimits(),
	}
}

func normalizeLimits(l Limits) Limits {
	d := DefaultLimits()
	if l.MaxConnectionsTotal == 0 {
		l.MaxConnectionsTotal = d.MaxConnectionsTotal
	}
	if l.MaxConnectionsPerIP == 0 {
		l.MaxConnectionsPerIP = d.MaxConnectionsPerIP
	}
	if l.MaxJoinFailures == 0 {
		l.MaxJoinFailures = d.MaxJoinFailures
	}
	if l.JoinRatePerIP == 0 {
		l.JoinRatePerIP = d.JoinRatePerIP
	}
	if l.JoinBurstPerIP == 0 {
		l.JoinBurstPerIP = d.JoinBurstPerIP
	}
	if l.JoinRateGlobal == 0 {
		l.JoinRateGlobal = d.JoinRateGlobal
	}
	if l.JoinBurstGlobal == 0 {
		l.JoinBurstGlobal = d.JoinBurstGlobal
	}
	if l.RejectAuditSampleEvery == 0 {
		l.RejectAuditSampleEvery = d.RejectAuditSampleEvery
	}
	if l.CreateRatePerIP == 0 {
		l.CreateRatePerIP = d.CreateRatePerIP
	}
	if l.CreateBurstPerIP == 0 {
		l.CreateBurstPerIP = d.CreateBurstPerIP
	}
	if l.CreateRateGlobal == 0 {
		l.CreateRateGlobal = d.CreateRateGlobal
	}
	if l.CreateBurstGlobal == 0 {
		l.CreateBurstGlobal = d.CreateBurstGlobal
	}
	if l.MaxActiveSessionsPerIP == 0 {
		l.MaxActiveSessionsPerIP = d.MaxActiveSessionsPerIP
	}
	if l.MaxActiveSessionsTotal == 0 {
		l.MaxActiveSessionsTotal = d.MaxActiveSessionsTotal
	}
	if l.HeartbeatRatePerIP == 0 {
		l.HeartbeatRatePerIP = d.HeartbeatRatePerIP
	}
	if l.HeartbeatBurstPerIP == 0 {
		l.HeartbeatBurstPerIP = d.HeartbeatBurstPerIP
	}
	if l.HeartbeatRateGlobal == 0 {
		l.HeartbeatRateGlobal = d.HeartbeatRateGlobal
	}
	if l.HeartbeatBurstGlobal == 0 {
		l.HeartbeatBurstGlobal = d.HeartbeatBurstGlobal
	}
	if l.DataRatePerConnection == 0 {
		l.DataRatePerConnection = d.DataRatePerConnection
	}
	if l.DataBurstPerConnection == 0 {
		l.DataBurstPerConnection = d.DataBurstPerConnection
	}
	if l.DataRateGlobal == 0 {
		l.DataRateGlobal = d.DataRateGlobal
	}
	if l.DataBurstGlobal == 0 {
		l.DataBurstGlobal = d.DataBurstGlobal
	}
	if l.ControlRatePerConnection == 0 {
		l.ControlRatePerConnection = d.ControlRatePerConnection
	}
	if l.ControlBurstPerConnection == 0 {
		l.ControlBurstPerConnection = d.ControlBurstPerConnection
	}
	if l.ControlRateGlobal == 0 {
		l.ControlRateGlobal = d.ControlRateGlobal
	}
	if l.ControlBurstGlobal == 0 {
		l.ControlBurstGlobal = d.ControlBurstGlobal
	}
	if l.LimiterMaxKeys == 0 {
		l.LimiterMaxKeys = d.LimiterMaxKeys
	}
	if l.LimiterIdleSeconds == 0 {
		l.LimiterIdleSeconds = d.LimiterIdleSeconds
	}
	if l.UDP == (p2p.Limits{}) {
		l.UDP = d.UDP
	}
	return l
}

// ValidateLimits 拒绝会关闭安全保护或造成无效容量的配置。
func ValidateLimits(l Limits) error {
	checks := []struct {
		name  string
		value float64
	}{
		{"max_connections_total", float64(l.MaxConnectionsTotal)},
		{"max_connections_per_ip", float64(l.MaxConnectionsPerIP)},
		{"max_join_failures", float64(l.MaxJoinFailures)},
		{"join_rate_per_ip", l.JoinRatePerIP},
		{"join_burst_per_ip", l.JoinBurstPerIP},
		{"join_rate_global", l.JoinRateGlobal},
		{"join_burst_global", l.JoinBurstGlobal},
		{"reject_audit_sample_every", float64(l.RejectAuditSampleEvery)},
		{"create_rate_per_ip", l.CreateRatePerIP},
		{"create_burst_per_ip", l.CreateBurstPerIP},
		{"create_rate_global", l.CreateRateGlobal},
		{"create_burst_global", l.CreateBurstGlobal},
		{"max_active_sessions_per_ip", float64(l.MaxActiveSessionsPerIP)},
		{"max_active_sessions_total", float64(l.MaxActiveSessionsTotal)},
		{"heartbeat_rate_per_ip", l.HeartbeatRatePerIP},
		{"heartbeat_burst_per_ip", l.HeartbeatBurstPerIP},
		{"heartbeat_rate_global", l.HeartbeatRateGlobal},
		{"heartbeat_burst_global", l.HeartbeatBurstGlobal},
		{"data_rate_per_connection", l.DataRatePerConnection},
		{"data_burst_per_connection", l.DataBurstPerConnection},
		{"data_rate_global", l.DataRateGlobal},
		{"data_burst_global", l.DataBurstGlobal},
		{"control_rate_per_connection", l.ControlRatePerConnection},
		{"control_burst_per_connection", l.ControlBurstPerConnection},
		{"control_rate_global", l.ControlRateGlobal},
		{"control_burst_global", l.ControlBurstGlobal},
		{"limiter_max_keys", float64(l.LimiterMaxKeys)},
		{"limiter_idle_seconds", float64(l.LimiterIdleSeconds)},
	}
	for _, check := range checks {
		if check.value <= 0 {
			return fmt.Errorf("%s must be greater than zero", check.name)
		}
	}
	return p2p.ValidateLimits(l.UDP)
}

// ParseLimitsJSON 以默认值为基线加载局部覆盖，并拒绝未知字段和尾随 JSON。
func ParseLimitsJSON(data []byte) (Limits, error) {
	limits := DefaultLimits()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&limits); err != nil {
		return Limits{}, fmt.Errorf("decode limits: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Limits{}, fmt.Errorf("decode limits: multiple JSON values")
		}
		return Limits{}, fmt.Errorf("decode limits trailing data: %w", err)
	}
	if err := ValidateLimits(limits); err != nil {
		return Limits{}, err
	}
	return limits, nil
}

// LoadLimitsFile 从 JSON 文件读取限流覆盖。
func LoadLimitsFile(path string) (Limits, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Limits{}, fmt.Errorf("read limits file: %w", err)
	}
	return ParseLimitsJSON(data)
}

// JSON 返回适合启动日志记录的完整生效配置。
func (l Limits) JSON() string {
	data, err := json.Marshal(l)
	if err != nil {
		return "{}"
	}
	return string(data)
}
