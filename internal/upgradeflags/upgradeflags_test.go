package upgradeflags

import "testing"

func TestExplicitBoolFlag(t *testing.T) {
	tests := []struct {
		arg, name        string
		matched, enabled bool
		wantErr          bool
	}{
		{"--standalone=true", "--standalone", true, true, false},
		{"--standalone=false", "--standalone", true, false, false},
		{"--no-auth=1", "--no-auth", true, true, false},
		{"--no-auth=0", "--no-auth", true, false, false},
		{"--standalone", "--standalone", false, false, false}, // bare: not the =value form
		{"--root=/x", "--standalone", false, false, false},
		{"--standalone=maybe", "--standalone", true, false, true},
	}
	for _, tt := range tests {
		matched, enabled, err := ExplicitBoolFlag(tt.arg, tt.name)
		if matched != tt.matched || enabled != tt.enabled || (err != nil) != tt.wantErr {
			t.Errorf("ExplicitBoolFlag(%q,%q)=(%v,%v,%v), want (%v,%v,err=%v)",
				tt.arg, tt.name, matched, enabled, err, tt.matched, tt.enabled, tt.wantErr)
		}
	}
}

func TestRejectStandaloneNoAuth(t *testing.T) {
	reject := [][]string{
		{"share", "--standalone"},
		{"share", "--standalone=true"},
		{"share", "--standalone=1"},
		{"share", "--no-auth"},
		{"share", "--no-auth=T"},
		{"share", "--standalone=maybe"}, // invalid bool also rejected
	}
	for _, argv := range reject {
		if err := RejectStandaloneNoAuth(argv); err == nil {
			t.Errorf("RejectStandaloneNoAuth(%#v) should error", argv)
		}
	}
	ok := [][]string{
		{"share", "--standalone=false", "--no-auth=0"},
		{"share", "--root", "/srv"},
		{"share"},
		nil,
	}
	for _, argv := range ok {
		if err := RejectStandaloneNoAuth(argv); err != nil {
			t.Errorf("RejectStandaloneNoAuth(%#v) unexpected error: %v", argv, err)
		}
	}
}

func TestFlagValue(t *testing.T) {
	argv := []string{"/opt/remote", "share", "--code-file", "/host/c.json", "--server=relay:8443"}
	if got := FlagValue(argv, "--code-file"); got != "/host/c.json" {
		t.Errorf("--code-file = %q", got)
	}
	if got := FlagValue(argv, "--server"); got != "relay:8443" {
		t.Errorf("--server = %q", got)
	}
	if got := FlagValue(argv, "--root"); got != "" {
		t.Errorf("absent --root = %q, want empty", got)
	}
	// The "=" guard must keep --code-file from matching --code-file-mirror.
	mirrored := []string{"share", "--code-file", "/tmp/up/code.json", "--code-file-mirror", "/host/c.json"}
	if got := FlagValue(mirrored, "--code-file"); got != "/tmp/up/code.json" {
		t.Errorf("--code-file with mirror present = %q", got)
	}
	if got := FlagValue(mirrored, "--code-file-mirror"); got != "/host/c.json" {
		t.Errorf("--code-file-mirror = %q", got)
	}
}

func TestHostCodeFile(t *testing.T) {
	// Plain share: --code-file is the host path.
	if got := HostCodeFile([]string{"remote", "share", "--code-file=/host/c.json"}); got != "/host/c.json" {
		t.Errorf("plain HostCodeFile = %q", got)
	}
	// Chained upgrade: mirror holds the real host path, --code-file is transient.
	chained := []string{"remote", "share", "--code-file", "/tmp/up/code.json", "--code-file-mirror", "/host/c.json"}
	if got := HostCodeFile(chained); got != "/host/c.json" {
		t.Errorf("chained HostCodeFile = %q, want the mirror", got)
	}
	// No code-file at all.
	if got := HostCodeFile([]string{"remote", "share"}); got != "" {
		t.Errorf("no code-file HostCodeFile = %q, want empty", got)
	}
}

func TestStageRootLinux(t *testing.T) {
	tests := []struct {
		argv         []string
		cwd          string
		wantRoot     string
		wantExplicit bool
	}{
		{[]string{"/opt/remote", "share"}, "/srv/app", "/srv/app", false},
		{[]string{"/opt/remote", "share", "--root", "/data"}, "/srv/app", "/data", true},
		{[]string{"/opt/remote", "share", "--root=files"}, "/srv/app", "/srv/app/files", true},
		{[]string{"/opt/remote", "share", "--root=/data/../d2"}, "/srv", "/d2", true},
		{[]string{"/opt/remote", "share", "--root="}, "/srv/app", "/srv/app", false}, // empty root == unrestricted
		{nil, "/srv/app", "/srv/app", false},
	}
	for _, tt := range tests {
		root, explicit, err := StageRoot("linux", tt.argv, tt.cwd)
		if err != nil || root != tt.wantRoot || explicit != tt.wantExplicit {
			t.Errorf("StageRoot(linux,%#v,%q)=(%q,%v,%v), want (%q,%v,nil)",
				tt.argv, tt.cwd, root, explicit, err, tt.wantRoot, tt.wantExplicit)
		}
	}
}

func TestStageRootWindows(t *testing.T) {
	tests := []struct {
		argv         []string
		cwd          string
		wantRoot     string
		wantExplicit bool
	}{
		{[]string{`C:\opt\remote.exe`, "share"}, `C:\srv\app`, `C:\srv\app`, false},
		{[]string{`C:\opt\remote.exe`, "share", "--root", `D:\data`}, `C:\srv\app`, `D:\data`, true},
		{[]string{`C:\opt\remote.exe`, "share", "--root=C:/srv"}, `C:\x`, `C:\srv`, true},
		{[]string{`C:\opt\remote.exe`, "share", "--root=files"}, `C:\srv\app`, `C:\srv\app\files`, true},
		{[]string{`C:\opt\remote.exe`, "share", "--root", `D:\a\..\b`}, `C:\srv`, `D:\b`, true},
		{[]string{`\\host\remote.exe`, "share"}, `\\srv\share\app`, `\\srv\share\app`, false},
	}
	for _, tt := range tests {
		root, explicit, err := StageRoot("windows", tt.argv, tt.cwd)
		if err != nil || root != tt.wantRoot || explicit != tt.wantExplicit {
			t.Errorf("StageRoot(windows,%#v,%q)=(%q,%v,%v), want (%q,%v,nil)",
				tt.argv, tt.cwd, root, explicit, err, tt.wantRoot, tt.wantExplicit)
		}
	}
}

func TestStageRootRejectsStandaloneAndMissingRootValue(t *testing.T) {
	if _, _, err := StageRoot("linux", []string{"remote", "share", "--standalone"}, "/tmp"); err == nil {
		t.Error("standalone should be rejected")
	}
	if _, _, err := StageRoot("linux", []string{"remote", "share", "--no-auth=TRUE"}, "/tmp"); err == nil {
		t.Error("--no-auth=TRUE should be rejected")
	}
	if _, _, err := StageRoot("linux", []string{"remote", "share", "--root"}, "/tmp"); err == nil {
		t.Error("--root without a value should error")
	}
}
