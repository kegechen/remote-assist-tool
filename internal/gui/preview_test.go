package gui

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreviewImageContentType(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n"), "image/png"},
		{"jpeg", []byte("\xff\xd8\xff\xe0"), "image/jpeg"},
		{"gif", []byte("GIF89a"), "image/gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"bmp", []byte("BM\x00\x00\x00\x00"), "image/bmp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := previewImageContentType(tt.data)
			if !ok || got != tt.want {
				t.Fatalf("got (%q, %v), want (%q, true)", got, ok, tt.want)
			}
		})
	}
}

func TestParseMD5Output(t *testing.T) {
	want := "d41d8cd98f00b204e9800998ecf8427e"
	for _, output := range []string{
		want + "  /tmp/a.png\n",
		"MD5 (/tmp/a.png) = " + want + "\n",
		"MD5 hash of a.png:\r\n" + want + "\r\nCertUtil: command completed successfully.\r\n",
		"MD5 hash:\r\nd4 1d 8c d9 8f 00 b2 04 e9 80 09 98 ec f8 42 7e\r\n",
	} {
		if got := parseMD5Output(output); got != want {
			t.Errorf("parseMD5Output(%q) = %q, want %q", output, got, want)
		}
	}
}

func TestCachePreviewDownloadHitsAndValidatesMD5(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "previews")
	content := []byte("cached image bytes")
	sum := md5.Sum(content)
	digest := hex.EncodeToString(sum[:])
	calls := 0
	download := func(path string) error {
		calls++
		return os.WriteFile(path, content, 0600)
	}
	first, err := cachePreviewDownload(cacheDir, digest, download)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cachePreviewDownload(cacheDir, digest, func(string) error {
		return errors.New("cache hit must not download")
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || calls != 1 {
		t.Fatalf("cache paths/calls = %q %q / %d", first, second, calls)
	}
	if got, err := os.ReadFile(first); err != nil || string(got) != string(content) {
		t.Fatalf("cached content = %q, %v", got, err)
	}
}

func TestCachePreviewDownloadRejectsChangedContent(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "previews")
	digest := "d41d8cd98f00b204e9800998ecf8427e"
	if _, err := cachePreviewDownload(cacheDir, digest, func(path string) error {
		return os.WriteFile(path, []byte("changed"), 0600)
	}); err == nil {
		t.Fatal("expected MD5 mismatch")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, digest)); !os.IsNotExist(err) {
		t.Fatalf("mismatched content was cached: %v", err)
	}
}

func TestPreviewImageContentTypeRejectsActiveOrUnknownContent(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		[]byte("<!doctype html><script>alert(1)</script>"),
		[]byte("not an image"),
	} {
		if got, ok := previewImageContentType(data); ok {
			t.Fatalf("unexpectedly allowed %q as %q", data, got)
		}
	}
}
