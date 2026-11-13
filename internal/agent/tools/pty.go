package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remote-assist/tool/internal/agent"
)

// 真终端（PTY）工具集：pty_open（流式长调用）/ pty_write / pty_resize / pty_close。
//
// pty_open 在会话存活期间一直不返回，终端输出以 StreamChunk 原样推送（字节原样、不做编码转换）；
// 会话结束（shell 退出 / pty_close / 调用被取消 / 隧道断开）时返回退出码。
// 会话 id 由调用方指定（pty_open 是长调用，拿不到中途返回值，后续 write/resize/close 靠它定位）。

const (
	ptyMinSize     = 1
	ptyMaxSize     = 1000
	ptyDefaultCols = 80
	ptyDefaultRows = 24
	// ptyChunkSize 单个输出帧上限。与 exec 流式块（32 KiB）同量级，留出 base64/AEAD 开销余量。
	ptyChunkSize = 16 * 1024
	// ptyMaxSessions 单个被控端的并发会话上限，防止资源被无限占用。
	ptyMaxSessions = 16
	// ptyMaxWriteBytes 单次 pty_write 上限。
	ptyMaxWriteBytes = 256 * 1024
	// ptySendFailLimit 连续多少次推送失败就认定对端已不在（隧道断且没有 cancel 到达），关闭会话防孤儿。
	ptySendFailLimit = 20
	// defaultPTYIdleTimeout 会话“无输入且无输出”多久后由被控端回收。环境变量 REMOTE_ASSIST_PTY_IDLE 可覆盖
	// （Go duration，如 30m、6h；0 或 off 关闭空闲回收，只剩 pty_open 的 24h 绝对上限）。
	defaultPTYIdleTimeout = 2 * time.Hour
	// ptyIdleEnv 空闲时限的环境变量名。
	ptyIdleEnv = "REMOTE_ASSIST_PTY_IDLE"
	// ptyWriteChunk 大段输入（粘贴）分块写入的块大小，块间释放输入锁，让 Ctrl+C 等紧急输入插队。
	ptyWriteChunk = 4096
	// ptyDrainTimeout 进程退出后等待残余输出读完的最长时间。
	ptyDrainTimeout = 2 * time.Second
)

// 输入写入的各类等待时限（变量，便于测试缩短）。
var (
	// ptyWriteLockWait 普通输入等输入锁的最长时间；超过说明前面的写入被卡住（对端缓冲满）。
	ptyWriteLockWait = 10 * time.Second
	// ptyWriteTimeout 单块 proc.Write 的最长阻塞；超时只放弃等待，被卡的写入仍占着锁直到完成或会话结束。
	ptyWriteTimeout = 10 * time.Second
	// ptyUrgentLockWait 紧急输入（Ctrl+C 等）等锁/写入的最长时间，超过就走 Interrupt 旁路。
	ptyUrgentLockWait = 300 * time.Millisecond
)

// errPTYWriteBlocked 输入写入被卡住。
var errPTYWriteBlocked = errors.New("pty_write_blocked: 终端输入被阻塞（对端未读取输入），请稍后重试或按 Ctrl+C")

// ptyInterrupter 是 ptyProc 的可选扩展：绕过被卡住的输入管道，直接向前台进程组递送控制信号。
// 目前只有 unix 后端实现（TIOCGPGRP + kill）；Windows ConPTY 没有等价物。
type ptyInterrupter interface {
	// Interrupt 递送由控制字符 b（0x03/0x1a/0x1c）对应的信号，成功返回 true。
	Interrupt(b byte) bool
}

// isUrgentInput 判断输入是否应走优先通道：短且含 Ctrl+C(0x03)/Ctrl+Z(0x1a)/Ctrl+\(0x1c)。
// 长度上限把“恰好含 0x03 字节的大段粘贴”排除在外，它仍按普通输入分块写。
func isUrgentInput(b []byte) bool {
	if len(b) == 0 || len(b) > 64 {
		return false
	}
	for _, c := range b {
		if c == 0x03 || c == 0x1a || c == 0x1c {
			return true
		}
	}
	return false
}

var ptyIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// 错误码前缀会被 agent.classifyError 或调用方按字符串识别。
var (
	errPTYDisabled = errors.New("exec_denied: 被控端未开放真终端（share 设了 --allow-exec 或 --root 时自动关闭，因为 shell 会绕过这些限制）；去掉这些参数后重启 share 即可")
	errPTYNotFound = errors.New("pty_not_found: 终端会话不存在或已结束")
)

// ptyProc 是平台后端给上层的会话句柄。所有方法并发安全且 Kill/Close 幂等。
type ptyProc interface {
	// Read 读取终端输出（字节原样）。进程结束并读尽后返回 io.EOF 或错误。
	Read(p []byte) (int, error)
	// Write 写入终端输入。
	Write(p []byte) (int, error)
	// Resize 调整终端大小。
	Resize(cols, rows uint16) error
	// Wait 阻塞到 shell 进程退出，返回退出码。
	Wait() int
	// Kill 杀掉整棵进程树，不等待。
	Kill()
	// Finish 通知后端“不会再有新输入/进程”，让残余输出得以读尽并给 Read 以 EOF
	// （Windows 需关闭伪控制台管道才会 EOF；unix 为空操作）。不得阻塞。
	Finish()
	// Close 释放全部句柄（会先 Kill）。
	Close()
}

type ptySession struct {
	id      string
	proc    ptyProc
	// writeSem 输入串行保序（容量 1 的信号量，以便带超时地等待）。按小块逐块持有，块间让位给紧急输入。
	writeSem chan struct{}
	urgent   atomic.Int32 // 正在等锁的紧急输入数；大块写入看到它非 0 会在块间让位
	done    chan struct{}
	ready   chan struct{} // proc 就绪后关闭；启动期间到达的 write/resize 在此等待
	once    sync.Once
	reason  string
	mu      sync.Mutex
	// lastActive 最近一次“活动”（输入、resize、输出）的 UnixNano，由 PTYManager.now 提供时间。
	lastActive atomic.Int64
}

// touch 记录一次活动。
func (m *PTYManager) touch(s *ptySession) { s.lastActive.Store(m.now().UnixNano()) }

// idleFor 距上次活动已过去多久。
func (m *PTYManager) idleFor(s *ptySession) time.Duration {
	return m.now().Sub(time.Unix(0, s.lastActive.Load()))
}

// ptyIdleTimeoutFromEnv 解析空闲时限；非法值回落默认，0/off 表示关闭。
func ptyIdleTimeoutFromEnv(getenv func(string) string) time.Duration {
	v := strings.TrimSpace(strings.ToLower(getenv(ptyIdleEnv)))
	switch v {
	case "":
		return defaultPTYIdleTimeout
	case "0", "off":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultPTYIdleTimeout
	}
	return d
}

// closeWith 记录首个关闭原因并杀进程树，幂等。
func (s *ptySession) closeWith(reason string) {
	s.once.Do(func() {
		s.mu.Lock()
		s.reason = reason
		s.mu.Unlock()
		s.proc.Kill()
	})
}

func (s *ptySession) closeReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// PTYManager 会话表。
type PTYManager struct {
	disabled bool
	mu       sync.Mutex
	sessions map[string]*ptySession
	// start 可在测试中替换。
	start func(spec ptySpec) (ptyProc, error)
	// 空闲回收：策略是“输入与输出都算活动”，只有完全无输入且无输出才计空闲——
	// 只看输入会误杀 tail -f、编译等无人值守的长任务；只看输出则会让空提示符的会话永远不回收。
	// now / idleTick 可在测试中注入。
	idleTimeout time.Duration
	idleTick    time.Duration
	now         func() time.Time
}

type ptySpec struct {
	Shell string
	Args  []string
	Cwd   string
	Env   map[string]string
	Cols  uint16
	Rows  uint16
}

// PTYDisabled 判定真终端是否应被禁用：allowPTY 由 share 决定——默认开启，
// 仅当设了 --allow-exec 或 --root 时关闭（PTY 里的 shell 能绕过 exec 白/黑名单和 --root）。
func PTYDisabled(allowPTY bool) bool {
	return !allowPTY
}

// NewPTYManager disabled=true 时所有 pty 工具返回 exec_denied（share 设了 --allow-exec/--root 时自动关闭）。
func NewPTYManager(disabled bool) *PTYManager {
	return &PTYManager{
		disabled: disabled, sessions: map[string]*ptySession{}, start: startPTY,
		idleTimeout: ptyIdleTimeoutFromEnv(os.Getenv), now: time.Now,
	}
}

// NewPTYTools 返回要注册的四个工具。
func NewPTYTools(disabled bool) []agent.Tool {
	m := NewPTYManager(disabled)
	return []agent.Tool{&ptyOpenTool{m}, &ptyWriteTool{m}, &ptyResizeTool{m}, &ptyCloseTool{m}}
}

// Count 当前会话数（测试用）。
func (m *PTYManager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *PTYManager) get(id string) (*ptySession, error) {
	if m.disabled {
		return nil, errPTYDisabled
	}
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return nil, errPTYNotFound
	}
	if !s.waitReady() {
		return nil, errPTYNotFound
	}
	return s, nil
}

// waitReady 等待后端进程就绪；启动失败/超时返回 false。
func (s *ptySession) waitReady() bool {
	select {
	case <-s.ready:
		return true
	case <-s.done:
		return false
	case <-time.After(5 * time.Second):
		return false
	}
}

// validateSize 校验并补默认值。0 表示未指定。
func validateSize(cols, rows int) (uint16, uint16, error) {
	if cols == 0 {
		cols = ptyDefaultCols
	}
	if rows == 0 {
		rows = ptyDefaultRows
	}
	if cols < ptyMinSize || cols > ptyMaxSize || rows < ptyMinSize || rows > ptyMaxSize {
		return 0, 0, fmt.Errorf("bad args: cols/rows 必须在 %d~%d 之间", ptyMinSize, ptyMaxSize)
	}
	return uint16(cols), uint16(rows), nil
}

// ---------- pty_open ----------

type ptyOpenArgs struct {
	Session string            `json:"session"`
	Shell   string            `json:"shell,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Cols    int               `json:"cols,omitempty"`
	Rows    int               `json:"rows,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Stream  bool              `json:"stream,omitempty"`
}

type ptyOpenResult struct {
	ExitCode int    `json:"exit_code"`
	Reason   string `json:"reason"` // exited | closed | cancelled | idle（长时间无输入无输出被回收）
}

type ptyOpenTool struct{ m *PTYManager }

func (t *ptyOpenTool) Name() string { return "pty_open" }

func (t *ptyOpenTool) Run(ctx context.Context, raw json.RawMessage, sink agent.StreamSink) (json.RawMessage, error) {
	m := t.m
	if m.disabled {
		return nil, errPTYDisabled
	}
	var a ptyOpenArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if !ptyIDRe.MatchString(a.Session) {
		return nil, fmt.Errorf("bad args: session 必须是 1~64 位字母数字/下划线/连字符")
	}
	cols, rows, err := validateSize(a.Cols, a.Rows)
	if err != nil {
		return nil, err
	}
	if sink == nil {
		return nil, fmt.Errorf("bad args: pty_open 需要流式通道")
	}
	shell, args, err := resolveShell(a.Shell)
	if err != nil {
		return nil, err
	}

	// 先占位再启动：避免并发同 id 或超上限。
	m.mu.Lock()
	if _, dup := m.sessions[a.Session]; dup {
		m.mu.Unlock()
		return nil, fmt.Errorf("bad args: session %q 已存在", a.Session)
	}
	if len(m.sessions) >= ptyMaxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("server_busy: 终端会话数已达上限 %d", ptyMaxSessions)
	}
	sess := &ptySession{id: a.Session, done: make(chan struct{}), ready: make(chan struct{}), writeSem: make(chan struct{}, 1)}
	m.sessions[a.Session] = sess
	m.mu.Unlock()
	remove := func() {
		m.mu.Lock()
		if m.sessions[a.Session] == sess {
			delete(m.sessions, a.Session)
		}
		m.mu.Unlock()
	}

	proc, err := m.start(ptySpec{Shell: shell, Args: args, Cwd: a.Cwd, Env: a.Env, Cols: cols, Rows: rows})
	if err != nil {
		remove()
		close(sess.done)
		return nil, err
	}
	sess.proc = proc
	m.touch(sess)
	close(sess.ready)
	// 无论如何退出都清理：杀树 + 释放句柄 + 摘表。
	defer func() {
		sess.closeWith("closed")
		proc.Close()
		remove()
		close(sess.done)
	}()

	// 空闲回收：定期检查，超时后以 "idle" 为原因关闭（先于 defer 里的 closeWith("closed") 记录原因）。
	if m.idleTimeout > 0 {
		stopReap := make(chan struct{})
		defer close(stopReap)
		go m.reapIdle(sess, stopReap)
	}

	// 输出泵：reader 只管读，sender 把积压的块合并后推送（天然背压，不增加空闲时延迟）。
	outCh := make(chan []byte, 64)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(outCh)
		buf := make([]byte, ptyChunkSize)
		for {
			n, err := proc.Read(buf)
			if n > 0 {
				m.touch(sess)
				b := make([]byte, n)
				copy(b, buf[:n])
				outCh <- b
			}
			if err != nil {
				return
			}
		}
	}()
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		fails := 0
		for first := range outCh {
			merged := first
		coalesce:
			for len(merged) < ptyChunkSize {
				select {
				case more, ok := <-outCh:
					if !ok {
						break coalesce
					}
					merged = append(merged, more...)
				default:
					break coalesce
				}
			}
			if err := sink.Send("stdout", merged); err != nil {
				fails++
				if fails >= ptySendFailLimit {
					log.Printf("tool | pty_open | 推送连续失败 %d 次，关闭会话", fails)
					sess.closeWith("closed")
				}
			} else {
				fails = 0
			}
		}
	}()

	exitCh := make(chan int, 1)
	go func() { exitCh <- proc.Wait() }()

	var code int
	select {
	case code = <-exitCh:
		sess.closeWith("exited") // 杀残余子孙进程（shell 已退出，后台进程不应继续存活）
	case <-ctx.Done():
		sess.closeWith("cancelled")
		select {
		case code = <-exitCh:
		case <-time.After(3 * time.Second):
			code = -1
		}
	}
	// 等残余输出读尽。先 Finish 让后端给出 EOF，再有限等待；超时则由 defer 里的 Close 强制收尾。
	proc.Finish()
	select {
	case <-readDone:
	case <-time.After(ptyDrainTimeout):
	}
	select {
	case <-sendDone:
	case <-time.After(ptyDrainTimeout):
	}
	reason := sess.closeReason()
	out, _ := json.Marshal(ptyOpenResult{ExitCode: code, Reason: reason})
	return out, nil
}

// reapIdle 周期检查会话是否已空闲超时；stop 关闭或会话结束即退出。
func (m *PTYManager) reapIdle(s *ptySession, stop <-chan struct{}) {
	tick := m.idleTick
	if tick <= 0 {
		tick = m.idleTimeout / 10
		if tick > time.Minute {
			tick = time.Minute
		}
		if tick < time.Second {
			tick = time.Second
		}
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.done:
			return
		case <-t.C:
			if m.idleFor(s) > m.idleTimeout {
				log.Printf("tool | pty_open | 会话 %s 空闲超过 %v，已回收", s.id, m.idleTimeout)
				s.closeWith("idle")
				return
			}
		}
	}
}

// ---------- pty_write ----------

type ptyWriteArgs struct {
	Session string `json:"session"`
	Data    []byte `json:"data"`
}

type ptyWriteTool struct{ m *PTYManager }

func (t *ptyWriteTool) Name() string { return "pty_write" }

func (t *ptyWriteTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a ptyWriteArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if len(a.Data) > ptyMaxWriteBytes {
		return nil, fmt.Errorf("bad args: data 超过 %d 字节", ptyMaxWriteBytes)
	}
	s, err := t.m.get(a.Session)
	if err != nil {
		return nil, err
	}
	t.m.touch(s)
	n, err := s.writeInput(a.Data)
	if err != nil {
		if errors.Is(err, errPTYWriteBlocked) {
			return nil, err
		}
		return nil, fmt.Errorf("pty_not_found: 写入失败（会话已结束）: %w", err)
	}
	out, _ := json.Marshal(map[string]int{"written": n})
	return out, nil
}

// writeChunk 在持有输入锁的前提下写一小块。等锁超过 lockWait 或写入阻塞超过 writeWait 返回 errPTYWriteBlocked；
// 写入协程自己负责释放锁，所以被卡住的写入完成前，后续输入仍保持顺序、不会交错。
func (s *ptySession) writeChunk(b []byte, lockWait, writeWait time.Duration) (int, error) {
	lt := time.NewTimer(lockWait)
	defer lt.Stop()
	select {
	case s.writeSem <- struct{}{}:
	case <-lt.C:
		return 0, errPTYWriteBlocked
	case <-s.done:
		return 0, errPTYNotFound
	}
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := s.proc.Write(b)
		<-s.writeSem
		ch <- result{n, err}
	}()
	wt := time.NewTimer(writeWait)
	defer wt.Stop()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-wt.C:
		return 0, errPTYWriteBlocked
	}
}

// writeInput 写入终端输入。
//   - 紧急输入（Ctrl+C 等）：短等锁；若输入被卡住且后端支持，直接向前台进程组递送信号。
//   - 普通输入：按 ptyWriteChunk 分块逐块拿锁，块间让位给等待中的紧急输入。
func (s *ptySession) writeInput(data []byte) (int, error) {
	if isUrgentInput(data) {
		return s.writeUrgent(data)
	}
	total := 0
	for total < len(data) {
		for i := 0; i < 50 && s.urgent.Load() > 0; i++ {
			time.Sleep(time.Millisecond) // 让紧急输入先过
		}
		end := total + ptyWriteChunk
		if end > len(data) {
			end = len(data)
		}
		n, err := s.writeChunk(data[total:end], ptyWriteLockWait, ptyWriteTimeout)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *ptySession) writeUrgent(data []byte) (int, error) {
	s.urgent.Add(1)
	defer s.urgent.Add(-1)
	n, err := s.writeChunk(data, ptyUrgentLockWait, ptyUrgentLockWait)
	if !errors.Is(err, errPTYWriteBlocked) {
		return n, err
	}
	// 输入管道被卡住：绕过它，直接给前台进程组发信号。
	if in, ok := s.proc.(ptyInterrupter); ok {
		sent := false
		for _, c := range data {
			if (c == 0x03 || c == 0x1a || c == 0x1c) && in.Interrupt(c) {
				sent = true
			}
		}
		if sent {
			return len(data), nil
		}
	}
	return 0, err
}

// ---------- pty_resize ----------

type ptyResizeArgs struct {
	Session string `json:"session"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

type ptyResizeTool struct{ m *PTYManager }

func (t *ptyResizeTool) Name() string { return "pty_resize" }

func (t *ptyResizeTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	var a ptyResizeArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	if a.Cols == 0 || a.Rows == 0 {
		return nil, fmt.Errorf("bad args: cols/rows 必须在 %d~%d 之间", ptyMinSize, ptyMaxSize)
	}
	cols, rows, err := validateSize(a.Cols, a.Rows)
	if err != nil {
		return nil, err
	}
	s, err := t.m.get(a.Session)
	if err != nil {
		return nil, err
	}
	t.m.touch(s)
	if err := s.proc.Resize(cols, rows); err != nil {
		return nil, fmt.Errorf("internal_error: resize 失败: %w", err)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

// ---------- pty_close ----------

type ptyCloseArgs struct {
	Session string `json:"session"`
}

type ptyCloseTool struct{ m *PTYManager }

func (t *ptyCloseTool) Name() string { return "pty_close" }

// Run 幂等：会话不存在也返回成功（closed=false）。
func (t *ptyCloseTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	if t.m.disabled {
		return nil, errPTYDisabled
	}
	var a ptyCloseArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("bad args: %w", err)
	}
	t.m.mu.Lock()
	s := t.m.sessions[a.Session]
	t.m.mu.Unlock()
	if s == nil {
		return json.RawMessage(`{"closed":false}`), nil
	}
	if !s.waitReady() {
		return json.RawMessage(`{"closed":false}`), nil
	}
	s.closeWith("closed")
	// 等 pty_open 收尾（最多 3 秒），让返回时进程树已被清理。
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
	}
	return json.RawMessage(`{"closed":true}`), nil
}
