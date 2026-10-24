package relay

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

func writeUpdateInfoTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
}

func TestUpdateInfoFileLoadsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"latest_version":"1.1.0"}`)
	srv, err := NewServer(&Config{Update: proto.UpdateInfo{LatestVersion: "v1.0.0", DownloadURL: "https://mirror.example/{version}/{os}-{arch}{ext}", ReleasePage: "https://mirror.example/releases/{version}"}, UpdateInfoFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.updateInfo(); got.LatestVersion != "1.1.0" {
		t.Fatalf("initial update info = %+v", got)
	}
	if got := srv.updateInfo(); got.DownloadURL != "https://mirror.example/{version}/{os}-{arch}{ext}" || got.ReleasePage != "https://mirror.example/releases/{version}" {
		t.Fatalf("file reload discarded base URLs: %+v", got)
	}

	writeUpdateInfoTestFile(t, path, `{"latest_version":"v1.2.0","release_page":"https://example.test/releases/{version}"}`)
	if err := srv.reloadUpdateInfoFile(); err != nil {
		t.Fatal(err)
	}
	if got := srv.updateInfo(); got.LatestVersion != "v1.2.0" || got.ReleasePage == "" {
		t.Fatalf("reloaded update info = %+v", got)
	}
}

func TestUpdateInfoFileNormalizesLiteralTemplateVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"latest_version":"1.1.0","download_url":"https://mirror.example/v1.1.0/remote-assist-cli-{os}-{arch}{ext}","release_page":"https://mirror.example/releases/v1.1.0"}`)
	srv, err := NewServer(&Config{UpdateInfoFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.updateInfo(); got.LatestVersion != "v1.1.0" || got.DownloadURL != "https://mirror.example/v1.1.0/remote-assist-cli-{os}-{arch}{ext}" {
		t.Fatalf("literal template was not preserved with canonical version: %+v", got)
	}
}

func TestUpdateInfoFileEmptyVersionInheritsBaseAndKeepsURLOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"download_url":"https://mirror.example/v1.1.0/remote-assist-cli-{os}-{arch}{ext}"}`)
	srv, err := NewServer(&Config{
		Update: proto.UpdateInfo{
			LatestVersion: "v1.1.0",
			DownloadURL:   "https://old.example/{version}/{os}-{arch}{ext}",
			ReleasePage:   "https://old.example/releases/{version}",
		},
		UpdateInfoFile: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := srv.updateInfo()
	if got.LatestVersion != "v1.1.0" || got.DownloadURL != "https://mirror.example/v1.1.0/remote-assist-cli-{os}-{arch}{ext}" || got.ReleasePage != "https://old.example/releases/{version}" {
		t.Fatalf("empty file version did not inherit base or preserve URL overrides: %+v", got)
	}
}

func TestUpdateInfoFileEmptyVersionRejectsUnsafeURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"latest_version":"v1.1.0"}`)
	srv, err := NewServer(&Config{Update: proto.UpdateInfo{LatestVersion: "v1.1.0"}, UpdateInfoFile: path})
	if err != nil {
		t.Fatal(err)
	}
	writeUpdateInfoTestFile(t, path, `{"download_url":"http://intranet/{os}-{arch}{ext}"}`)
	if err := srv.reloadUpdateInfoFile(); err == nil {
		t.Fatal("unsafe URL with empty version was accepted")
	}
	if got := srv.updateInfo(); got.LatestVersion != "v1.1.0" || got.DownloadURL != "" {
		t.Fatalf("unsafe reload replaced previous announcement: %+v", got)
	}
}

func TestUpdateInfoFileInvalidChangeKeepsPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"latest_version":"v1.1.0"}`)
	srv, err := NewServer(&Config{UpdateInfoFile: path})
	if err != nil {
		t.Fatal(err)
	}
	writeUpdateInfoTestFile(t, path, `{"latest_version":"dev"}`)
	if err := srv.reloadUpdateInfoFile(); err == nil {
		t.Fatal("invalid announcement accepted")
	}
	if got := srv.updateInfo(); got.LatestVersion != "v1.1.0" {
		t.Fatalf("invalid reload replaced previous info: %+v", got)
	}
}

func TestUpdateInfoFileRetriesInvalidContentAfterItBecomesValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"latest_version":"v1.1.0"}`)
	srv, err := NewServer(&Config{UpdateInfoFile: path})
	if err != nil {
		t.Fatal(err)
	}
	writeUpdateInfoTestFile(t, path, `{"latest_version":`)
	if err := srv.reloadUpdateInfoFile(); err == nil {
		t.Fatal("invalid announcement accepted")
	}
	writeUpdateInfoTestFile(t, path, `{"latest_version":"v1.2.0"}`)
	if err := srv.reloadUpdateInfoFile(); err != nil {
		t.Fatal(err)
	}
	if got := srv.updateInfo(); got.LatestVersion != "v1.2.0" {
		t.Fatalf("valid retry did not replace previous info: %+v", got)
	}
}

func TestUpdateInfoFileDoesNotRetryUnchangedInvalidContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"latest_version":`)
	srv, err := NewServer(&Config{UpdateInfoFile: path})
	if err != nil {
		t.Fatal(err)
	}
	state := srv.updateInfoFileState
	if !state.initialized || !state.exists {
		t.Fatalf("invalid file metadata was not remembered: %+v", state)
	}
	if _, changed, err := srv.readUpdateInfoFile(); err != nil || changed {
		t.Fatalf("unchanged invalid file retried: changed=%v err=%v", changed, err)
	}
}

func TestUpdateInfoFileNoneDisablesNotice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"latest_version":"none"}`)
	srv, err := NewServer(&Config{Update: proto.UpdateInfo{LatestVersion: "v1.1.0"}, UpdateInfoFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.updateInfo(); got != (proto.UpdateInfo{}) {
		t.Fatalf("none did not disable announcement: %+v", got)
	}
}

func TestUpdateInfoFileDeletionRestoresBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-info.json")
	writeUpdateInfoTestFile(t, path, `{"latest_version":"v1.1.0"}`)
	srv, err := NewServer(&Config{Update: proto.UpdateInfo{LatestVersion: "v1.0.0"}, UpdateInfoFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := srv.reloadUpdateInfoFile(); err != nil {
		t.Fatal(err)
	}
	if got := srv.updateInfo(); got.LatestVersion != "v1.0.0" {
		t.Fatalf("file deletion did not restore base announcement: %+v", got)
	}
}

func BenchmarkUpdateInfoUnchangedStat(b *testing.B) {
	path := filepath.Join(b.TempDir(), "update-info.json")
	if err := os.WriteFile(path, []byte(`{"latest_version":"v1.1.0"}`), 0600); err != nil {
		b.Fatal(err)
	}
	srv, err := NewServer(&Config{UpdateInfoFile: path})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, changed, err := srv.readUpdateInfoFile(); err != nil || changed {
			b.Fatalf("unchanged read: changed=%v err=%v", changed, err)
		}
	}
}
