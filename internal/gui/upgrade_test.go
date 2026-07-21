package gui

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/upgradeflags"
)

func TestDecodeConnectMetadataFromMCPEnvelope(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"{\"connected\":true,\"peer_version\":\"0.0.5\",\"help_version\":\"0.0.6-7-gabc\",\"server\":\"relay:8443\",\"p2p\":true}"}]}`)
	got, err := decodeConnectMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Connected || got.PeerVersion != "0.0.5" || got.HelpVersion != "0.0.6-7-gabc" || got.Server != "relay:8443" || !got.P2P {
		t.Fatalf("unexpected metadata: %+v", got)
	}
}

func TestNewerBuild(t *testing.T) {
	tests := []struct {
		candidate string
		current   string
		newer     bool
		known     bool
	}{
		{"0.0.6", "0.0.5", true, true},
		{"remote-assist 0.0.6-7-gabcdef-dirty", "0.0.6", true, true},
		{"0.0.6", "0.0.6-7-gabcdef", false, true},
		{"0.0.5", "0.0.6", false, true},
		{"dev", "0.0.6", false, false},
	}
	for _, tt := range tests {
		newer, known := newerBuild(tt.candidate, tt.current)
		if newer != tt.newer || known != tt.known {
			t.Errorf("newerBuild(%q, %q)=(%v,%v), want (%v,%v)", tt.candidate, tt.current, newer, known, tt.newer, tt.known)
		}
	}
}

func TestInspectLinuxELFArchitecture(t *testing.T) {
	for _, tt := range []struct {
		name    string
		machine uint16
		want    string
	}{{"amd64", 62, "amd64"}, {"arm64", 183, "arm64"}} {
		t.Run(tt.name, func(t *testing.T) {
			header := make([]byte, 20)
			copy(header, []byte("\x7fELF"))
			header[4] = 2
			header[5] = 1
			binary.LittleEndian.PutUint16(header[18:], tt.machine)
			path := filepath.Join(t.TempDir(), "remote")
			if err := os.WriteFile(path, header, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := inspectLinuxELF(path)
			if err != nil || got != tt.want {
				t.Fatalf("got (%q, %v), want (%q, nil)", got, err, tt.want)
			}
		})
	}
}

func TestInspectLinuxELFRejectsNonELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.exe")
	if err := os.WriteFile(path, []byte("MZ-not-an-elf-binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectLinuxELF(path); err == nil {
		t.Fatal("expected a non-ELF file to be rejected")
	}
}

func TestInspectWindowsPEArchitecture(t *testing.T) {
	for _, tt := range []struct {
		name    string
		machine uint16
		want    string
	}{{"amd64", 0x8664, "amd64"}, {"arm64", 0xaa64, "arm64"}} {
		t.Run(tt.name, func(t *testing.T) {
			image := make([]byte, 0x86)
			copy(image, []byte("MZ"))
			binary.LittleEndian.PutUint32(image[0x3c:], 0x80)
			copy(image[0x80:], []byte("PE\x00\x00"))
			binary.LittleEndian.PutUint16(image[0x84:], tt.machine)
			path := filepath.Join(t.TempDir(), "remote.exe")
			if err := os.WriteFile(path, image, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := inspectWindowsPE(path)
			if err != nil || got != tt.want {
				t.Fatalf("got (%q, %v), want (%q, nil)", got, err, tt.want)
			}
		})
	}
}

func TestInspectWindowsPERejectsELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote")
	if err := os.WriteFile(path, []byte("\x7fELF-not-a-pe-binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectWindowsPE(path); err == nil {
		t.Fatal("expected a non-PE file to be rejected")
	}
}

func TestRemoteJoinUsesTargetSeparators(t *testing.T) {
	if got := remoteJoin("windows", `C:\data`, "upgrade", "new.exe"); got != `C:\data\upgrade\new.exe` {
		t.Fatalf("Windows path = %q", got)
	}
	if got := remoteJoin("linux", "/srv/data", "upgrade", "new"); got != "/srv/data/upgrade/new" {
		t.Fatalf("Linux path = %q", got)
	}
}

func TestWindowsProbeScriptAgainstParentProcess(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell probe")
	}
	probeDir := filepath.Join(t.TempDir(), "升级测试")
	if err := os.Mkdir(probeDir, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", windowsProbeScript)
	cmd.Dir = probeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	var got struct {
		PID     int      `json:"pid"`
		Exe     string   `json:"exe"`
		CWD     string   `json:"cwd"`
		OS      string   `json:"os"`
		Arch    string   `json:"arch"`
		Argv    []string `json:"argv"`
		TempDir string   `json:"temp_dir"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("decode probe output: %v\n%s", err, out)
	}
	if got.PID != os.Getpid() {
		t.Errorf("parent PID = %d, want %d", got.PID, os.Getpid())
	}
	if got.OS != "windows" || got.Arch != runtime.GOARCH || got.Exe == "" || len(got.Argv) == 0 || got.TempDir == "" {
		t.Errorf("unexpected probe metadata: %+v", got)
	}
	if filepath.Base(got.CWD) != "升级测试" {
		t.Errorf("cwd lost its UTF-8 path: %q", got.CWD)
	}
	// StageRoot 现在在 Go 侧从 argv+cwd 计算；无 --root 时应回落到 cwd（保留 UTF-8）。
	stageRoot, _, err := upgradeflags.StageRoot(got.OS, got.Argv, got.CWD)
	if err != nil || filepath.Base(stageRoot) != "升级测试" {
		t.Errorf("StageRoot from probe = (%q, %v), want base 升级测试", stageRoot, err)
	}
}

func TestParseLinuxProbeOutput(t *testing.T) {
	enc := func(argv ...string) string {
		return base64.StdEncoding.EncodeToString([]byte(strings.Join(argv, "\x00")))
	}
	// 正常：带 --root，argv 完整 → StageRoot=/data、explicit、TempDir=/tmp。
	out := strings.Join([]string{"1234", "/opt/remote", "/srv/app", "Linux", "x86_64", "/tmp",
		enc("/opt/remote", "share", "--root", "/data")}, "\n")
	probe, err := parseLinuxProbeOutput(out, "0.0.6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if probe.PID != 1234 || probe.OS != "linux" || probe.Arch != "amd64" || probe.TempDir != "/tmp" {
		t.Fatalf("probe basics wrong: %+v", probe)
	}
	if probe.StageRoot != "/data" || !probe.RootExplicit {
		t.Errorf("StageRoot=%q explicit=%v, want /data,true", probe.StageRoot, probe.RootExplicit)
	}
	if len(probe.Argv) != 4 || probe.Argv[0] != "/opt/remote" {
		t.Errorf("argv=%v", probe.Argv)
	}

	// 无 --root：StageRoot 回落 cwd、not explicit；arm64 归一化。
	out = strings.Join([]string{"1234", "/opt/remote", "/srv/app", "Linux", "aarch64", "/var/tmp",
		enc("/opt/remote", "share")}, "\n")
	if probe, err = parseLinuxProbeOutput(out, ""); err != nil ||
		probe.StageRoot != "/srv/app" || probe.RootExplicit || probe.Arch != "arm64" || probe.TempDir != "/var/tmp" {
		t.Errorf("no-root probe = %+v, err=%v", probe, err)
	}

	// 空 argv（远端无 base64，只有 6 行）→ 中止，绝不带空 argv 继续。
	out = strings.Join([]string{"1234", "/opt/remote", "/srv/app", "Linux", "x86_64", "/tmp"}, "\n")
	if _, err := parseLinuxProbeOutput(out, ""); err == nil {
		t.Error("empty argv should abort the probe")
	}

	// 行数不足 → 报错。
	if _, err := parseLinuxProbeOutput("1234\n/opt/remote\n", ""); err == nil {
		t.Error("short probe output should error")
	}

	// standalone → StageRoot 经 RejectStandaloneNoAuth 拒绝。
	out = strings.Join([]string{"1234", "/opt/remote", "/srv/app", "Linux", "x86_64", "/tmp",
		enc("/opt/remote", "share", "--standalone")}, "\n")
	if _, err := parseLinuxProbeOutput(out, ""); err == nil {
		t.Error("standalone should be rejected")
	}
}

func TestUpgradeStageParent(t *testing.T) {
	// 未显式 --root：放系统临时目录。
	if got := upgradeStageParent(remoteProcessProbe{StageRoot: "/srv/app", TempDir: "/tmp", RootExplicit: false}); got != "/tmp" {
		t.Errorf("unrestricted parent = %q, want /tmp", got)
	}
	// 显式 --root：留在 StageRoot 内，即便有 TempDir。
	if got := upgradeStageParent(remoteProcessProbe{StageRoot: "/srv/app", TempDir: "/tmp", RootExplicit: true}); got != "/srv/app" {
		t.Errorf("restricted parent = %q, want /srv/app", got)
	}
	// TempDir 探测缺失时回落到 StageRoot。
	if got := upgradeStageParent(remoteProcessProbe{StageRoot: "/srv/app", TempDir: "", RootExplicit: false}); got != "/srv/app" {
		t.Errorf("missing temp dir parent = %q, want /srv/app", got)
	}
}

func TestParseUpgradePID(t *testing.T) {
	if got, err := parseUpgradePID(" 1234\n"); err != nil || got != 1234 {
		t.Fatalf("parseUpgradePID returned (%d, %v)", got, err)
	}
	for _, raw := range []string{"", "1", "-2", "not-a-pid"} {
		if _, err := parseUpgradePID(raw); err == nil {
			t.Errorf("parseUpgradePID(%q) should fail", raw)
		}
	}
}

func TestTryReconnectAbandonsStaleGeneration(t *testing.T) {
	s := NewServer("remote", "relay:8443")
	// 守护线程带着代次 3 发起重连，但用户已在期间接管连接把 connGen 推到了 5。
	s.connGen = 5
	if got := s.tryReconnect(3, "code", "", false); got != reconnectSuperseded {
		t.Fatalf("tryReconnect(过期代次)=%v, want reconnectSuperseded", got)
	}
	// 过期重连必须原地放弃：既没建子进程，也没覆盖用户当前的连接状态。
	if s.client != nil || s.connected {
		t.Fatalf("过期重连改动了连接状态: client=%v connected=%v", s.client, s.connected)
	}
}

func TestMarkConnectionLostOnlyClearsCurrentClient(t *testing.T) {
	current := &MCPClient{}
	stale := &MCPClient{}
	s := NewServer("remote", "relay:8443")
	s.client = current
	s.connected = true
	s.peerVersion = "0.0.5"
	s.helpVersion = "0.0.6"
	s.peerHost = "server"
	s.effectiveSrv = "relay:8443"
	s.sessionID = "session"
	s.p2p = true
	s.connectedAt = time.Now()
	events := make(chan string, 1)
	s.sseSubs[events] = struct{}{}

	if s.markConnectionLost(stale) {
		t.Fatal("stale client must not clear the active connection")
	}
	if !s.connected {
		t.Fatal("stale client changed connection state")
	}
	if !s.markConnectionLost(current) {
		t.Fatal("current client should be marked lost")
	}
	if s.connected || s.peerVersion != "" || s.helpVersion != "" || s.peerHost != "" || s.effectiveSrv != "" || s.sessionID != "" || s.p2p || !s.connectedAt.IsZero() {
		t.Fatalf("stale connection metadata was retained: %+v", s)
	}
	select {
	case event := <-events:
		if event != "event: lost\n" {
			t.Fatalf("event = %q", event)
		}
	default:
		t.Fatal("lost event was not broadcast")
	}
}
