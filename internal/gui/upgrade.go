package gui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	pathpkg "path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/remote-assist/tool/internal/upgradeflags"
)

const maxUpgradeBinarySize = 256 << 20

type connectMetadata struct {
	Connected   bool   `json:"connected"`
	SessionID   string `json:"session_id"`
	Server      string `json:"server"`
	PeerVersion string `json:"peer_version"`
	PeerHost    string `json:"peer_host"`
	HelpVersion string `json:"help_version"`
	P2P         bool   `json:"p2p"`
}

type remoteExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error"`
}

type remoteProcessProbe struct {
	PID          int
	Exe          string
	CWD          string
	OS           string
	Arch         string
	Version      string
	Argv         []string
	StageRoot    string
	RootExplicit bool   // old share 显式设了非空 --root（决定升级目录能否放系统临时目录）
	TempDir      string // 远端系统临时目录，用作未受限 share 的升级目录父级
}

type upgradeCodeFile struct {
	Code      string `json:"code"`
	Server    string `json:"server"`
	ExpiresAt int64  `json:"expiresAt"`
}

func decodeToolResult(raw json.RawMessage, out any) error {
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Content) > 0 {
		return json.Unmarshal([]byte(envelope.Content[0].Text), out)
	}
	return json.Unmarshal(raw, out)
}

func decodeConnectMetadata(raw json.RawMessage) (connectMetadata, error) {
	var meta connectMetadata
	err := decodeToolResult(raw, &meta)
	return meta, err
}

func (s *Server) recordConnectMetadata(raw json.RawMessage, code, server string, noAuth bool) {
	s.lastCode = code
	s.lastServer = server
	s.lastNoAuth = noAuth
	meta, err := decodeConnectMetadata(raw)
	if err != nil {
		return
	}
	s.peerVersion = meta.PeerVersion
	s.helpVersion = meta.HelpVersion
	s.peerHost = meta.PeerHost
	s.effectiveSrv = meta.Server
	s.sessionID = meta.SessionID
	s.p2p = meta.P2P
	s.connectedAt = time.Now()
}

func callRemoteJSON(ctx context.Context, client *MCPClient, tool string, args map[string]any, out any) error {
	raw, err := client.CallTool(ctx, tool, args)
	if err != nil {
		return err
	}
	return decodeToolResult(raw, out)
}

func execRemote(ctx context.Context, client *MCPClient, argv []string) (remoteExecResult, error) {
	var result remoteExecResult
	err := callRemoteJSON(ctx, client, "exec", map[string]any{
		"argv":       argv,
		"timeout_ms": 30000,
	}, &result)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Error)
		if detail == "" {
			detail = strings.TrimSpace(result.Stderr)
		}
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return result, errors.New(detail)
	}
	return result, nil
}

func probeRemoteShare(ctx context.Context, client *MCPClient, fallbackVersion string) (remoteProcessProbe, error) {
	linuxProbe, linuxErr := probeLinuxShare(ctx, client, fallbackVersion)
	if linuxErr == nil {
		return linuxProbe, nil
	}
	windowsProbe, windowsErr := probeWindowsShare(ctx, client, fallbackVersion)
	if windowsErr == nil {
		return windowsProbe, nil
	}
	return remoteProcessProbe{}, fmt.Errorf("无法探测远端 share（Linux: %v；Windows: %v）", linuxErr, windowsErr)
}

func probeLinuxShare(ctx context.Context, client *MCPClient, fallbackVersion string) (remoteProcessProbe, error) {
	// 固定顺序：pid, exe, cwd, uname-s, uname-m, tmpdir, [base64 cmdline]。tmpdir 放在
	// 条件性 cmdline 之前以保证位置稳定；cmdline 若缺（无 base64）末行不出现。
	const script = `printf '%s\n' "$PPID"; readlink "/proc/$PPID/exe"; readlink "/proc/$PPID/cwd"; uname -s; uname -m; printf '%s\n' "${TMPDIR:-/tmp}"; if command -v base64 >/dev/null 2>&1; then base64 "/proc/$PPID/cmdline" | tr -d '\n'; fi; printf '\n'`
	res, err := execRemote(ctx, client, []string{"sh", "-c", script})
	if err != nil {
		return remoteProcessProbe{}, fmt.Errorf("Linux probe failed (sh may be denied): %w", err)
	}
	probe, err := parseLinuxProbeOutput(res.Stdout, fallbackVersion)
	if err != nil {
		return remoteProcessProbe{}, err
	}
	if probe.Version == "" {
		if vres, verr := execRemote(ctx, client, []string{probe.Exe, "--version"}); verr == nil {
			probe.Version = extractVersion(vres.Stdout)
		}
	}
	return probe, nil
}

// parseLinuxProbeOutput 解析 probeLinuxShare 脚本的 stdout（纯函数，便于单测）：
// 固定行 pid/exe/cwd/os/arch/tmpdir，之后可选一行 base64 编码的 /proc/PID/cmdline。
func parseLinuxProbeOutput(stdout, fallbackVersion string) (remoteProcessProbe, error) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(stdout), "\r", ""), "\n")
	if len(lines) < 6 {
		return remoteProcessProbe{}, fmt.Errorf("Linux probe returned %d lines, want 6", len(lines))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 1 {
		return remoteProcessProbe{}, fmt.Errorf("invalid old share PID %q", lines[0])
	}
	probe := remoteProcessProbe{
		PID:     pid,
		Exe:     strings.TrimSpace(lines[1]),
		CWD:     strings.TrimSpace(lines[2]),
		OS:      strings.ToLower(strings.TrimSpace(lines[3])),
		Arch:    normalizeArch(strings.TrimSpace(lines[4])),
		TempDir: strings.TrimSpace(lines[5]),
		Version: fallbackVersion,
	}
	if probe.OS != "linux" || probe.Exe == "" || probe.CWD == "" || probe.Arch == "" {
		return remoteProcessProbe{}, fmt.Errorf("unsupported target: os=%q arch=%q", probe.OS, lines[4])
	}
	if len(lines) >= 7 && strings.TrimSpace(lines[6]) != "" {
		if cmdline, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[6])); derr == nil {
			for _, arg := range strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00") {
				probe.Argv = append(probe.Argv, arg)
			}
		}
	}
	// argv 是 --root/--code-file/standalone 三项策略的唯一依据（temp-dir 选择、宿主 code-file
	// 快照、拒绝 standalone 都靠它）。远端 stage 读 /proc 拿的是真 argv；若这里因远端缺 base64
	// 或解码失败拿到空 argv 还继续，就会与远端不对称：误判 no-root→选 /tmp 致握手 read_file
	// 够不到、且宿主 --code-file 被 new share 覆写却无快照可还原。故与 Windows 一样：拿不到
	// argv 直接中止，绝不带空 argv 继续。
	if len(probe.Argv) == 0 {
		return remoteProcessProbe{}, errors.New("无法获取 old share 命令行（远端缺 base64？）——升级依赖它判定 --root/--code-file，已中止")
	}
	probe.StageRoot, probe.RootExplicit, err = upgradeflags.StageRoot(probe.OS, probe.Argv, probe.CWD)
	if err != nil {
		return remoteProcessProbe{}, err
	}
	return probe, nil
}

const windowsProbeScript = `$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object Text.UTF8Encoding($false)
$OutputEncoding = [Console]::OutputEncoding
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class RemoteAssistCommandLine {
    [DllImport("shell32.dll", SetLastError=true)]
    static extern IntPtr CommandLineToArgvW([MarshalAs(UnmanagedType.LPWStr)] string commandLine, out int argc);
    [DllImport("kernel32.dll")]
    static extern IntPtr LocalFree(IntPtr value);
    public static string[] Split(string commandLine) {
        int argc;
        IntPtr argv = CommandLineToArgvW(commandLine, out argc);
        if (argv == IntPtr.Zero) throw new System.ComponentModel.Win32Exception();
        try {
            string[] result = new string[argc];
            for (int i = 0; i < argc; i++) result[i] = Marshal.PtrToStringUni(Marshal.ReadIntPtr(argv, i * IntPtr.Size));
            return result;
        } finally { LocalFree(argv); }
    }
}
'@
$self = Get-CimInstance Win32_Process -Filter ("ProcessId=" + $PID)
$old = Get-CimInstance Win32_Process -Filter ("ProcessId=" + $self.ParentProcessId)
if (-not $old -or -not $old.ExecutablePath -or -not $old.CommandLine) { throw 'cannot inspect parent share process' }
$argv = [RemoteAssistCommandLine]::Split($old.CommandLine)
$cwd = (Get-Location).ProviderPath
$stream = [IO.File]::Open($old.ExecutablePath, [IO.FileMode]::Open, [IO.FileAccess]::Read, ([IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete))
try {
    $reader = New-Object IO.BinaryReader($stream)
    if ($reader.ReadUInt16() -ne 0x5a4d) { throw 'old share is not a PE executable' }
    $stream.Position = 0x3c; $peOffset = $reader.ReadUInt32(); $stream.Position = $peOffset
    if ($reader.ReadUInt32() -ne 0x00004550) { throw 'old share has an invalid PE header' }
    $machine = $reader.ReadUInt16()
} finally { $stream.Dispose() }
$arch = switch ($machine) { 0x8664 { 'amd64' } 0xaa64 { 'arm64' } default { throw ("unsupported old share PE machine 0x{0:x4}" -f $machine) } }
[ordered]@{
    pid = [int]$old.ProcessId
    exe = [string]$old.ExecutablePath
    cwd = [string]$cwd
    os = 'windows'
    arch = $arch
    argv = @($argv)
    temp_dir = [string][IO.Path]::GetTempPath()
} | ConvertTo-Json -Compress`

func probeWindowsShare(ctx context.Context, client *MCPClient, fallbackVersion string) (remoteProcessProbe, error) {
	res, err := execRemote(ctx, client, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", windowsProbeScript})
	if err != nil {
		return remoteProcessProbe{}, fmt.Errorf("PowerShell probe failed: %w", err)
	}
	var raw struct {
		PID     int      `json:"pid"`
		Exe     string   `json:"exe"`
		CWD     string   `json:"cwd"`
		OS      string   `json:"os"`
		Arch    string   `json:"arch"`
		Argv    []string `json:"argv"`
		TempDir string   `json:"temp_dir"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &raw); err != nil {
		return remoteProcessProbe{}, fmt.Errorf("decode PowerShell probe: %w", err)
	}
	probe := remoteProcessProbe{
		PID: raw.PID, Exe: raw.Exe, CWD: raw.CWD, OS: raw.OS, Arch: normalizeArch(raw.Arch),
		Version: fallbackVersion, Argv: raw.Argv, TempDir: raw.TempDir,
	}
	if probe.PID <= 1 || probe.OS != "windows" || probe.Exe == "" || probe.CWD == "" || probe.Arch == "" || len(probe.Argv) == 0 {
		return remoteProcessProbe{}, fmt.Errorf("invalid Windows probe result: pid=%d os=%q arch=%q", probe.PID, probe.OS, raw.Arch)
	}
	// StageRoot（含 standalone/no-auth 拒绝、--root 解析）统一在 Go 侧从 argv+cwd 计算，
	// 与 Linux 走同一 upgradeflags.StageRoot，PowerShell 只负责采集原始事实。
	probe.StageRoot, probe.RootExplicit, err = upgradeflags.StageRoot(probe.OS, probe.Argv, probe.CWD)
	if err != nil {
		return remoteProcessProbe{}, err
	}
	if probe.Version == "" {
		if vres, verr := execRemote(ctx, client, []string{probe.Exe, "--version"}); verr == nil {
			probe.Version = extractVersion(vres.Stdout)
		}
	}
	return probe, nil
}

func normalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return ""
	}
}

func inspectLinuxELF(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	header := make([]byte, 20)
	if _, err := io.ReadFull(f, header); err != nil {
		return "", fmt.Errorf("read ELF header: %w", err)
	}
	if string(header[:4]) != "\x7fELF" || header[5] != 1 {
		return "", errors.New("selected file is not a little-endian Linux ELF binary")
	}
	machine := binary.LittleEndian.Uint16(header[18:20])
	switch machine {
	case 62:
		return "amd64", nil
	case 183:
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported ELF machine %d", machine)
	}
}

func inspectWindowsPE(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	header := make([]byte, 64)
	if _, err := io.ReadFull(f, header); err != nil {
		return "", fmt.Errorf("read PE header: %w", err)
	}
	if string(header[:2]) != "MZ" {
		return "", errors.New("selected file is not a Windows PE executable")
	}
	peOffset := int64(binary.LittleEndian.Uint32(header[0x3c:]))
	if peOffset < 0x40 || peOffset > 64<<20 {
		return "", errors.New("selected file has an invalid PE header offset")
	}
	peHeader := make([]byte, 6)
	if _, err := f.ReadAt(peHeader, peOffset); err != nil {
		return "", fmt.Errorf("read PE machine: %w", err)
	}
	if string(peHeader[:4]) != "PE\x00\x00" {
		return "", errors.New("selected file has an invalid PE signature")
	}
	switch binary.LittleEndian.Uint16(peHeader[4:]) {
	case 0x8664:
		return "amd64", nil
	case 0xaa64:
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported PE machine 0x%04x", binary.LittleEndian.Uint16(peHeader[4:]))
	}
}

func inspectUpgradeBinary(path, remoteOS string) (string, error) {
	switch remoteOS {
	case "linux":
		return inspectLinuxELF(path)
	case "windows":
		return inspectWindowsPE(path)
	default:
		return "", fmt.Errorf("unsupported remote OS %q", remoteOS)
	}
}

func remoteJoin(remoteOS, base string, names ...string) string {
	if remoteOS == "windows" {
		out := strings.TrimRight(base, `\/`)
		for _, name := range names {
			out += `\` + strings.Trim(name, `\/`)
		}
		return out
	}
	parts := append([]string{base}, names...)
	return pathpkg.Join(parts...)
}

func windowsPowerShellArgScript(value, body string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	return fmt.Sprintf("$ErrorActionPreference='Stop';$value=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'));%s", encoded, body)
}

func prepareRemoteUpgradeDir(ctx context.Context, client *MCPClient, remoteOS, dir string) error {
	if remoteOS == "windows" {
		_, err := execRemote(ctx, client, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", windowsPowerShellArgScript(dir, "[IO.Directory]::CreateDirectory($value) | Out-Null")})
		return err
	}
	_, err := execRemote(ctx, client, []string{"mkdir", "-p", dir})
	return err
}

var buildVersionRE = regexp.MustCompile(`(?i)(?:^|\s)v?(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g[0-9a-f]+)?(?:-dirty)?(?:\s|$)`)

type buildVersion struct {
	Major, Minor, Patch, Distance int
}

func parseBuildVersion(s string) (buildVersion, bool) {
	m := buildVersionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return buildVersion{}, false
	}
	values := make([]int, 4)
	for i := range values {
		if m[i+1] != "" {
			values[i], _ = strconv.Atoi(m[i+1])
		}
	}
	return buildVersion{values[0], values[1], values[2], values[3]}, true
}

func extractVersion(s string) string {
	m := buildVersionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "remote-assist "))
	}
	return strings.TrimSpace(m[0])
}

func newerBuild(candidate, current string) (newer, known bool) {
	c, cok := parseBuildVersion(candidate)
	o, ook := parseBuildVersion(current)
	if !cok || !ook {
		return false, false
	}
	ca := []int{c.Major, c.Minor, c.Patch, c.Distance}
	oa := []int{o.Major, o.Minor, o.Patch, o.Distance}
	for i := range ca {
		if ca[i] != oa[i] {
			return ca[i] > oa[i], true
		}
	}
	return false, true
}

// upgradeStageParent 选升级目录的父级：未显式 --root（文件工具默认不受限）时放系统临时
// 目录，交给 OS 临时清理机制回收（new share 退出后老化），read_file 在无限制下仍可达；
// 显式 --root 时必须留在 StageRoot 内，否则握手期 read_file 够不到升级目录。
func upgradeStageParent(probe remoteProcessProbe) string {
	if !probe.RootExplicit && probe.TempDir != "" {
		return probe.TempDir
	}
	return probe.StageRoot
}

func randomUpgradeID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func readRemoteText(ctx context.Context, client *MCPClient, remotePath string) (string, error) {
	var result struct {
		Text string `json:"text"`
	}
	if err := callRemoteJSON(ctx, client, "read_file", map[string]any{"path": remotePath, "length": 65536}, &result); err != nil {
		return "", err
	}
	if result.Text == "" {
		return "", errors.New("remote file is empty or not text")
	}
	return result.Text, nil
}

// writeRemoteFile 通过 write_file 工具把 data 写到远端 path（truncate + create）。
// content 以 []byte 传入，经 JSON 编成 base64，远端解码后原样落盘。用于回滚还原
// 宿主 code-file。
func writeRemoteFile(ctx context.Context, client *MCPClient, path string, data []byte) error {
	var result struct {
		BytesWritten int `json:"bytes_written"`
	}
	return callRemoteJSON(ctx, client, "write_file", map[string]any{
		"path": path, "content": data, "create": true,
	}, &result)
}

func waitForUpgradeFiles(ctx context.Context, client *MCPClient, codePath, pidPath string) (upgradeCodeFile, int, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	var observedPID int
	for {
		codeRaw, codeErr := readRemoteText(ctx, client, codePath)
		pidRaw, pidErr := readRemoteText(ctx, client, pidPath)
		if pidErr == nil {
			if pid, err := parseUpgradePID(pidRaw); err == nil {
				observedPID = pid
			}
		}
		if codeErr == nil && pidErr == nil {
			var code upgradeCodeFile
			if json.Unmarshal([]byte(codeRaw), &code) == nil && code.Code != "" && code.Server != "" && observedPID > 1 {
				return code, observedPID, nil
			}
			lastErr = errors.New("upgrade files are not complete yet")
		} else if codeErr != nil {
			lastErr = codeErr
		} else {
			lastErr = pidErr
		}
		select {
		case <-ctx.Done():
			return upgradeCodeFile{}, observedPID, fmt.Errorf("new share did not publish its code: %w (last error: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func parseUpgradePID(raw string) (int, error) {
	pid, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("invalid upgrade PID %q", strings.TrimSpace(raw))
	}
	return pid, nil
}

func (s *Server) markConnectionLost(client *MCPClient) bool {
	s.mu.Lock()
	if s.client != client {
		s.mu.Unlock()
		return false
	}
	s.connected = false
	s.peerVersion = ""
	s.helpVersion = ""
	s.peerHost = ""
	s.effectiveSrv = ""
	s.sessionID = ""
	s.p2p = false
	s.connectedAt = time.Time{}
	s.mu.Unlock()
	s.broadcast("event: lost\n")
	return true
}

func connectToolArgs(code, server string, noAuth bool) map[string]any {
	args := map[string]any{}
	if noAuth {
		args["no_auth"] = true
	} else {
		args["code"] = code
	}
	if server != "" {
		args["server"] = server
	}
	return args
}

func reconnectPeer(ctx context.Context, client *MCPClient, code, server string, noAuth bool) (connectMetadata, error) {
	raw, err := client.CallTool(ctx, "connect", connectToolArgs(code, server, noAuth))
	if err != nil {
		return connectMetadata{}, err
	}
	return decodeConnectMetadata(raw)
}

func (s *Server) upgradeProgress(message string) {
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " ")
	s.broadcast("event: upgrade-progress\ndata: " + message + "\n")
}

func (s *Server) rejectWhileUpgrading(w http.ResponseWriter) bool {
	if !s.upgrading.Load() {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "share 正在升级，请等待当前操作完成"})
	return true
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.upgradeMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "另一个升级任务正在运行"})
		return
	}
	defer s.upgradeMu.Unlock()
	if !s.upgrading.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "另一个升级任务正在运行"})
		return
	}
	defer s.upgrading.Store(false)

	s.mu.Lock()
	client := s.client
	connected := s.connected
	oldCode, oldServer, oldNoAuth := s.lastCode, s.lastServer, s.lastNoAuth
	peerVersion, effectiveServer := s.peerVersion, s.effectiveSrv
	if effectiveServer == "" {
		if oldServer != "" {
			effectiveServer = oldServer
		} else {
			effectiveServer = s.defaultServer
		}
	}
	s.mu.Unlock()
	if !connected || client == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "当前未连接 share"})
		return
	}
	if oldNoAuth {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "standalone/no-auth 暂不支持通道内升级"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUpgradeBinarySize)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取升级包失败: " + err.Error()})
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	upload, header, err := r.FormFile("binary")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请选择升级二进制"})
		return
	}
	defer upload.Close()
	local, err := os.CreateTemp("", "remote-assist-upgrade-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	localPath := local.Name()
	defer os.Remove(localPath)
	if _, err := io.Copy(local, upload); err != nil {
		local.Close()
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "保存升级包失败: " + err.Error()})
		return
	}
	if err := local.Close(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 用户已明确确认升级后让事务独立完成；浏览器刷新/关闭不能把流程截断在已切 new、
	// 尚未替换或回滚的中间状态。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	s.upgradeProgress("正在探测 old share")
	probe, err := probeRemoteShare(ctx, client, peerVersion)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	packageArch, err := inspectUpgradeBinary(localPath, probe.OS)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if packageArch != probe.Arch {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("升级包架构为 %s/%s，远端为 %s/%s", probe.OS, packageArch, probe.OS, probe.Arch)})
		return
	}

	// 兼容 old 的 --code-file：升级后 new share 会继续写它（upgradedShareArgs 透传
	// --code-file-mirror），但升级失败回滚时 new share 已把原路径写成了它那份即将失效的
	// 协助码。这里升级前 best-effort 快照原文件；仅当读得到（== 也能写回还原）才启用还原。
	// 快照/还原走 sandbox 化的 read_file/write_file，故只在原路径处于 old share 可达范围
	// （默认无 --root 时恒可达）时生效。链式升级时 HostCodeFile 取 mirror（真宿主路径）。
	originalCodeFile := upgradeflags.HostCodeFile(probe.Argv)
	var codeFileSnapshot []byte
	snapshotOK := false
	if originalCodeFile != "" {
		if snap, err := readRemoteText(ctx, client, originalCodeFile); err == nil {
			codeFileSnapshot = []byte(snap)
			snapshotOK = true
		} else {
			s.upgradeProgress("原 --code-file 不可读，回滚将不还原该路径: " + err.Error())
		}
	}
	staged := false
	committed := false
	defer func() {
		if committed || !staged || !snapshotOK {
			return
		}
		// 回滚：把宿主 code-file 还原为升级前内容，避免残留失效的新码。请求 ctx 可能已随
		// 响应结束，用独立后台 ctx；回连 old 由各回滚分支负责，这里只 best-effort 写回。
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer restoreCancel()
		if err := writeRemoteFile(restoreCtx, client, originalCodeFile, codeFileSnapshot); err != nil {
			s.upgradeProgress("回滚后还原原 --code-file 失败: " + err.Error())
		}
	}()

	id := randomUpgradeID()
	upgradeDir := remoteJoin(probe.OS, upgradeStageParent(probe), ".remote-assist-upgrade-"+id)
	binaryName := "remote-assist-new"
	if probe.OS == "windows" {
		binaryName += ".exe"
	}
	remoteBinary := remoteJoin(probe.OS, upgradeDir, binaryName)
	codePath := remoteJoin(probe.OS, upgradeDir, "code.json")
	pidPath := remoteJoin(probe.OS, upgradeDir, "new.pid")
	logPath := remoteJoin(probe.OS, upgradeDir, "new.log")
	homePath := remoteJoin(probe.OS, upgradeDir, "home")
	backupPath := probe.Exe + ".old-" + id
	failedPath := probe.Exe + ".failed-" + id
	if err := prepareRemoteUpgradeDir(ctx, client, probe.OS, upgradeDir); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "创建远端升级目录失败: " + err.Error()})
		return
	}
	s.upgradeProgress("正在分块上传 " + header.Filename)
	var transfer struct {
		Bytes int64 `json:"bytes"`
	}
	if err := callRemoteJSON(ctx, client, "upload_file", map[string]any{"local_path": localPath, "remote_path": remoteBinary}, &transfer); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "上传升级包失败，old 通道仍保留: " + err.Error()})
		return
	}
	if probe.OS == "linux" {
		if _, err := execRemote(ctx, client, []string{"chmod", "0755", remoteBinary}); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "设置升级包执行权限失败: " + err.Error()})
			return
		}
	}
	versionResult, err := execRemote(ctx, client, []string{remoteBinary, "--version"})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "候选二进制无法在远端运行: " + err.Error()})
		return
	}
	candidateVersion := extractVersion(versionResult.Stdout)
	if candidateVersion == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "无法读取候选二进制版本"})
		return
	}
	if newer, known := newerBuild(candidateVersion, probe.Version); known && !newer {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "not_needed": true,
			"error":           fmt.Sprintf("无需升级：远端 %s，候选包 %s", probe.Version, candidateVersion),
			"current_version": probe.Version, "candidate_version": candidateVersion,
		})
		return
	}

	s.upgradeProgress("正在启动隔离的新 share")
	stageArgs := []string{remoteBinary, "upgrade-stage",
		"--old-pid", strconv.Itoa(probe.PID), "--home", homePath, "--code-file", codePath,
		"--pid-file", pidPath, "--log-file", logPath, "--server", effectiveServer}
	if probe.OS == "windows" {
		stageArgs = append(stageArgs, "--target", probe.Exe, "--backup", backupPath, "--cwd", probe.CWD, "--")
		stageArgs = append(stageArgs, probe.Argv...)
	}
	if _, err := execRemote(ctx, client, stageArgs); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "启动新 share 失败，old 通道仍保留: " + err.Error()})
		return
	}
	staged = true // new share 已起，可能已写过 mirror；此后任何非 commit 返回都触发 defer 还原
	pollCtx, pollCancel := context.WithTimeout(ctx, 45*time.Second)
	newCode, newPID, err := waitForUpgradeFiles(pollCtx, client, codePath, pidPath)
	pollCancel()
	if err != nil {
		msg := err.Error() + "；old 通道仍保留，可查看 " + logPath
		if probe.OS == "windows" {
			if rollbackErr := rollbackWindowsUpgrade(ctx, client, remoteBinary, probe, newPID, pidPath, backupPath, failedPath); rollbackErr != nil {
				msg += "；恢复旧文件失败: " + rollbackErr.Error()
			}
		} else if newPID > 1 {
			if _, stopErr := execRemote(ctx, client, []string{"kill", "-TERM", strconv.Itoa(newPID)}); stopErr != nil {
				msg += "；终止候选 share 失败: " + stopErr.Error()
			}
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": msg})
		return
	}

	s.upgradeProgress("新 share 已就绪，正在切换通道")
	newMeta, err := reconnectPeer(ctx, client, newCode.Code, newCode.Server, false)
	if err != nil {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 45*time.Second)
		_, reconnectErr := reconnectPeer(rollbackCtx, client, oldCode, oldServer, oldNoAuth)
		var cleanupErr error
		if reconnectErr == nil {
			if probe.OS == "windows" {
				cleanupErr = rollbackWindowsUpgrade(rollbackCtx, client, remoteBinary, probe, newPID, pidPath, backupPath, failedPath)
			} else {
				_, cleanupErr = execRemote(rollbackCtx, client, []string{"kill", "-TERM", strconv.Itoa(newPID)})
			}
		}
		rollbackCancel()
		msg := "连接新 share 失败，已保留 old share: " + err.Error()
		if reconnectErr != nil {
			msg += "；自动回连 old 也失败: " + reconnectErr.Error()
			s.markConnectionLost(client)
		} else if cleanupErr != nil {
			msg += "；清理候选 share 失败: " + cleanupErr.Error()
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": msg})
		return
	}
	if newMeta.PeerVersion != "" && extractVersion(newMeta.PeerVersion) != extractVersion(candidateVersion) {
		var reconnectErr, cleanupErr error
		if probe.OS == "windows" {
			_, reconnectErr = reconnectPeer(ctx, client, oldCode, oldServer, oldNoAuth)
			if reconnectErr == nil {
				cleanupErr = rollbackWindowsUpgrade(ctx, client, remoteBinary, probe, newPID, pidPath, backupPath, failedPath)
			}
		} else {
			_, _ = execRemote(ctx, client, []string{"kill", "-TERM", strconv.Itoa(newPID)})
			_, reconnectErr = reconnectPeer(ctx, client, oldCode, oldServer, oldNoAuth)
		}
		msg := fmt.Sprintf("新通道版本校验失败：期望 %s，实际 %s", candidateVersion, newMeta.PeerVersion)
		if reconnectErr != nil {
			msg += "；自动回连 old 失败: " + reconnectErr.Error()
			s.markConnectionLost(client)
		} else if cleanupErr != nil {
			msg += "；恢复旧文件失败: " + cleanupErr.Error()
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": msg})
		return
	}

	var killErr error
	if probe.OS == "windows" {
		s.upgradeProgress("新通道验证成功，正在终止 old 并清理备份")
		_, killErr = execRemote(ctx, client, []string{remoteBinary, "upgrade-finalize", "--action", "commit", "--old-pid", strconv.Itoa(probe.PID), "--backup", backupPath})
	} else {
		s.upgradeProgress("新通道验证成功，正在原子替换原文件")
		if _, err := execRemote(ctx, client, []string{remoteBinary, "upgrade-finalize", "--source", remoteBinary, "--target", probe.Exe}); err != nil {
			_, _ = execRemote(ctx, client, []string{"kill", "-TERM", strconv.Itoa(newPID)})
			_, reconnectErr := reconnectPeer(ctx, client, oldCode, oldServer, oldNoAuth)
			msg := "替换原文件失败，old 尚未终止: " + err.Error()
			if reconnectErr != nil {
				msg += "；自动回连 old 失败: " + reconnectErr.Error()
				s.markConnectionLost(client)
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": msg})
			return
		}
		_, killErr = execRemote(ctx, client, []string{"kill", "-TERM", strconv.Itoa(probe.PID)})
	}
	committed = true // new share 已接管、原文件已替换/old 已终止，不再回滚，跳过 defer 还原
	s.mu.Lock()
	if s.client == client {
		s.connected = true
		s.recordConnectMetadata(mustJSON(newMeta), newCode.Code, newCode.Server, false)
	}
	s.mu.Unlock()
	s.upgrading.Store(false) // 先开放普通 API，随后 connected 事件触发的 loadDrives 才能成功。
	s.broadcast("event: connected\n")
	s.upgradeProgress("升级完成")
	result := map[string]any{
		"ok": true, "current_version": probe.Version, "new_version": candidateVersion,
		"peer_version": newMeta.PeerVersion, "replaced_path": probe.Exe,
	}
	if killErr != nil {
		result["warning"] = "新通道已接管且文件已替换，但终止 old PID 失败: " + killErr.Error()
	}
	writeJSON(w, http.StatusOK, result)
}

func rollbackWindowsUpgrade(ctx context.Context, client *MCPClient, helper string, probe remoteProcessProbe, newPID int, pidFile, backup, failed string) error {
	args := []string{helper, "upgrade-finalize", "--action", "rollback", "--old-pid", strconv.Itoa(probe.PID),
		"--new-pid", strconv.Itoa(newPID), "--pid-file", pidFile, "--target", probe.Exe, "--backup", backup, "--failed", failed}
	_, err := execRemote(ctx, client, args)
	return err
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
