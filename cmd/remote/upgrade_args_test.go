package main

import (
	"reflect"
	"testing"
)

func TestUpgradedShareArgsPreservesPolicyAndReplacesManagedFlags(t *testing.T) {
	got, err := upgradedShareArgs([]string{
		"/opt/remote", "share", "--server", "old:1", "--root=/srv/app",
		"--allow-exec", "sh,cat", "--new-instance", "--code-file=/tmp/old.json", "--p2p", "disabled", "--unsafe-full-system",
	}, "new:8443", "/tmp/new.json")
	if err != nil {
		t.Fatal(err)
	}
	// old 的 --code-file 作为 --code-file-mirror 保留，让宿主原路径升级后继续刷新。
	want := []string{"share", "--root=/srv/app", "--allow-exec", "sh,cat", "--new-instance", "--p2p", "disabled", "--unsafe-exec", "--server", "new:8443", "--code-file", "/tmp/new.json", "--code-file-mirror", "/tmp/old.json", upgradeSuccessorFlag}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestUpgradedShareArgsOmitsMirrorWhenOldHadNoCodeFile(t *testing.T) {
	got, err := upgradedShareArgs([]string{"/opt/remote", "share", "--root=/srv/app"}, "new:8443", "/tmp/new.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"share", "--root=/srv/app", "--server", "new:8443", "--code-file", "/tmp/new.json", upgradeSuccessorFlag}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// 链式升级：old 本身已是升级产物，真宿主路径在 --code-file-mirror，--code-file 是上次
// 的临时路径。再升级必须把真宿主路径继续带下去，而不是把临时路径当宿主路径。
func TestUpgradedShareArgsCarriesForwardHostMirror(t *testing.T) {
	got, err := upgradedShareArgs([]string{
		"/opt/remote", "share", upgradeSuccessorFlag, "--code-file", "/tmp/up-A/code.json", "--code-file-mirror", "/host/c.json",
	}, "new:8443", "/tmp/up-B/code.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"share", "--server", "new:8443", "--code-file", "/tmp/up-B/code.json", "--code-file-mirror", "/host/c.json", upgradeSuccessorFlag}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestUpgradedShareArgsHandlesNoArgumentShare(t *testing.T) {
	got, err := upgradedShareArgs([]string{"/opt/remote"}, "relay:8443", "/tmp/code.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"share", "--server", "relay:8443", "--code-file", "/tmp/code.json", upgradeSuccessorFlag}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestUpgradedShareArgsRejectsStandalone(t *testing.T) {
	for _, flag := range []string{"--standalone", "--standalone=true", "--standalone=1", "--no-auth", "--no-auth=T"} {
		if _, err := upgradedShareArgs([]string{"remote", "share", flag}, "relay:8443", "/tmp/code.json"); err == nil {
			t.Fatalf("expected %s to be rejected", flag)
		}
	}
}

func TestUpgradedShareArgsPreservesExplicitFalseStandaloneFlags(t *testing.T) {
	got, err := upgradedShareArgs([]string{
		"remote", "share", "--standalone=false", "--no-auth=0", "--root", "/srv/app",
	}, "relay:8443", "/tmp/code.json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"share", "--standalone=false", "--no-auth=0", "--root", "/srv/app", "--server", "relay:8443", "--code-file", "/tmp/code.json", upgradeSuccessorFlag}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestUpgradedShareArgsRejectsInvalidExplicitBool(t *testing.T) {
	if _, err := upgradedShareArgs([]string{"remote", "share", "--standalone=maybe"}, "relay:8443", "/tmp/code.json"); err == nil {
		t.Fatal("expected invalid boolean value to be rejected")
	}
}

// TestUpgradedShareArgsPreservesMinProto 热升级必须原样带上 --min-proto。
//
// 丢了它的后果正是本次兼容改动要避免的那个场景：用户为了迁就旧 help 才开了兼容模式，
// share 一次热升级之后悄悄回到默认的"拒绝 v1"，于是"升级完就连不上了"。
// 目前它是靠 upgradedShareArgs 的默认分支透传的——没有任何一行代码显式提到它，
// 所以用测试钉住，免得将来有人收紧透传规则时顺手把它滤掉。
func TestUpgradedShareArgsPreservesMinProto(t *testing.T) {
	for _, form := range [][]string{
		{"--min-proto", "1"},
		{"--min-proto=1"},
	} {
		args := append([]string{"remote", "share"}, form...)
		got, err := upgradedShareArgs(args, "relay:8443", "/tmp/code.json")
		if err != nil {
			t.Fatal(err)
		}
		want := append(append([]string{"share"}, form...),
			"--server", "relay:8443", "--code-file", "/tmp/code.json", upgradeSuccessorFlag)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("form %v: got %#v, want %#v", form, got, want)
		}
	}
}
