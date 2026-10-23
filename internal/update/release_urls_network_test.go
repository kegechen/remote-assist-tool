//go:build network

// 真实请求 GitHub，确认升级提示里拼出的链接都能下载。需要联网，默认不跑：
//
//	go test -tags network ./internal/update -run TestReleaseURLsExist -v
//	UPDATE_CHECK_VERSION=v1.2.0 go test -tags network ./internal/update -run TestReleaseURLsExist -v
//
// 发版（Release 产物上传完）后、升级 relay 之前跑一遍：relay 默认把自身版本当作最新版本
// 下发，链接 404 就意味着用户拿到的是死链。
package update

import (
	"io"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"
)

func TestReleaseURLsExist(t *testing.T) {
	ver := os.Getenv("UPDATE_CHECK_VERSION")
	if ver == "" {
		ver = "v1.1.0"
	}
	if !IsRelease(ver) {
		t.Fatalf("UPDATE_CHECK_VERSION=%q 不是正式版本号", ver)
	}

	var urls []string
	for p := range releasedPlatforms {
		goos, goarch := splitPlatform(p)
		u := ExpandURL(DefaultDownloadURL, ver, goos, goarch)
		if u == "" {
			t.Fatalf("%s 未展开", p)
		}
		urls = append(urls, u)
	}
	sort.Strings(urls)
	urls = append(urls, expand(DefaultReleasePage, ver, "", "", ""))

	client := &http.Client{Timeout: 30 * time.Second}
	for _, u := range urls {
		// 用 GET + Range 只取首字节：GitHub 下载经 302 跳到对象存储，部分节点不支持 HEAD。
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Range", "bytes=0-0")
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("%s: %v", u, err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			t.Errorf("%s: HTTP %d", u, resp.StatusCode)
			continue
		}
		t.Logf("OK %d %s", resp.StatusCode, u)
	}
}

func splitPlatform(p string) (string, string) {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i], p[i+1:]
		}
	}
	return p, ""
}
