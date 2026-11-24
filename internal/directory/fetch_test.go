package directory_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

func TestCheckURL(t *testing.T) {
	for _, u := range []string{
		directory.DefaultURL,
		"https://relay-list.example.test/v1/directory",
		"https://192.0.2.10:8443/v1/directory",
	} {
		if err := directory.CheckURL(u); err != nil {
			t.Errorf("CheckURL(%q) = %v, want nil", u, err)
		}
	}
	for _, u := range []string{
		"",
		"http://relay-list.example.test/v1/directory",
		"relay-list.example.test/v1/directory",
		"https:///v1/directory",
		"https://:443/v1/directory", // 只有端口，没有主机名
		"https://user:x@relay-list.example.test/v1/directory",
		"https://user@relay-list.example.test/v1/directory",
		"://relay-list.example.test",
		"ftp://relay-list.example.test/v1/directory",
	} {
		if err := directory.CheckURL(u); err == nil {
			t.Errorf("CheckURL(%q) = nil, want error", u)
		}
	}
}

// fetchServer 起一个 TLS 测试服务：/v1/directory 返回 body；/hop/N 跳转 N 次后到 /v1/directory；
// /to-http 跳到另一个明文 http 服务（它对任何请求都正常返回 body）；/slow 一直不回，直到请求被取消；
// /endless 返回没有尽头的响应体；/status/N 返回状态码 N 和 body。
func fetchServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(plain.Close)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/directory":
			_, _ = w.Write(body)
		case strings.HasPrefix(r.URL.Path, "/hop/"):
			n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
			if err != nil || n < 0 {
				http.NotFound(w, r)
				return
			}
			next := "/v1/directory"
			if n > 1 {
				next = fmt.Sprintf("/hop/%d", n-1)
			}
			http.Redirect(w, r, next, http.StatusFound)
		case r.URL.Path == "/to-http":
			http.Redirect(w, r, plain.URL+"/v1/directory", http.StatusFound)
		case r.URL.Path == "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		case r.URL.Path == "/endless":
			chunk := bytes.Repeat([]byte(" "), 64<<10)
			for r.Context().Err() == nil {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		case strings.HasPrefix(r.URL.Path, "/status/"):
			code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
			if err != nil || code < 100 || code > 599 {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(code)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// testClient 是 NewHTTPClient 的客户端，换上信任测试证书的 Transport（跳转策略与超时不变）。
func testClient(ts *httptest.Server, timeout time.Duration) *http.Client {
	c := directory.NewHTTPClient(timeout)
	c.Transport = ts.Client().Transport
	return c
}

func TestFetchOK(t *testing.T) {
	body := []byte(`{"kind":"directory"}`)
	ts := fetchServer(t, body)
	got, err := directory.Fetch(context.Background(), testClient(ts, 5*time.Second), ts.URL+"/v1/directory")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("Fetch = %q, want %q", got, body)
	}
}

func TestFetchSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		size int
		ok   bool
	}{
		{directory.MaxRawSize, true},
		{directory.MaxRawSize + 1, false},
	} {
		ts := fetchServer(t, bytes.Repeat([]byte(" "), tc.size))
		got, err := directory.Fetch(context.Background(), testClient(ts, 5*time.Second), ts.URL+"/v1/directory")
		if tc.ok && (err != nil || len(got) != tc.size) {
			t.Errorf("size %d: got %d bytes, err %v; want the whole body", tc.size, len(got), err)
		}
		if !tc.ok && err == nil {
			t.Errorf("size %d: got %d bytes, want an error", tc.size, len(got))
		}
	}
}

// 没有尽头的响应：读到上限就停，不能一直读到超时（也不能把它全读进内存）。
func TestFetchEndlessBody(t *testing.T) {
	ts := fetchServer(t, nil)
	start := time.Now()
	_, err := directory.Fetch(context.Background(), testClient(ts, 20*time.Second), ts.URL+"/endless")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("Fetch of an endless body = %v, want a size error", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Fetch took %s, want it to stop at the size limit", elapsed)
	}
}

// 只接受 200：其它 2xx、3xx（没有 Location 的）、4xx 都不是列表。
func TestFetchRejectsNon200(t *testing.T) {
	body := []byte(`{"kind":"directory"}`)
	ts := fetchServer(t, body)
	for _, code := range []int{http.StatusCreated, http.StatusNotModified, http.StatusNotFound, http.StatusInternalServerError} {
		got, err := directory.Fetch(context.Background(), testClient(ts, 5*time.Second), fmt.Sprintf("%s/status/%d", ts.URL, code))
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) {
			t.Errorf("HTTP %d: got %q, err %v; want an error naming the status", code, got, err)
		}
	}
}

// 非 200 时报状态码，不照抄服务器给的状态行原因短语：它由服务器决定，可能带着终端控制字符，
// 客户端会把这个错误打到终端上。
func TestFetchNon200HidesReasonPhrase(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 503 \x1b[2Jowned\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = buf.Flush()
	}))
	t.Cleanup(ts.Close)
	_, err := directory.Fetch(context.Background(), testClient(ts, 5*time.Second), ts.URL+"/v1/directory")
	if err == nil {
		t.Fatal("Fetch of a 503 response succeeded")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q does not mention the status code", err)
	}
	if strings.Contains(err.Error(), "\x1b") || strings.Contains(err.Error(), "owned") {
		t.Errorf("error %q echoes the server's reason phrase", err)
	}
}

// 传输层的错误里也有服务器决定的文本（坏掉的响应头、证书里的名字……），同样会打到终端：控制字符要转义。
// 错误链要保留，调用方还能用 errors.Is 判断超时等原因。
func TestFetchErrorsArePrintable(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nX-A: \x1b[2Jowned\xe2\x80\xae\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = buf.Flush()
	}))
	t.Cleanup(ts.Close)
	_, err := directory.Fetch(context.Background(), testClient(ts, 5*time.Second), ts.URL+"/v1/directory")
	if err == nil {
		t.Fatal("Fetch of a response with a malformed header succeeded")
	}
	if msg := err.Error(); strings.ContainsAny(msg, "\x1b\xe2\x80\xae") {
		t.Errorf("error %q carries the server's control characters", msg)
	}
	// 服务器的文本确实走到了错误里（只是转义了）：请求若因为别的原因失败，上面那条断言也会通过。
	if msg, want := err.Error(), `\x1b[2Jowned\u202e`; !strings.Contains(msg, want) {
		t.Errorf("error %q does not show the escaped header %s", msg, want)
	}

	slow := fetchServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := directory.Fetch(ctx, testClient(slow, 5*time.Second), slow.URL+"/slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fetch past its deadline = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}

// 错误文本有上限：坏掉的响应头会整行拼进传输层的错误，一行可以有好几 MB（转义后还要翻几倍）。
// relay 每次刷新失败都打一行日志，客户端第一次失败也会整段打到终端、写进日志文件。
func TestFetchErrorTextIsBounded(t *testing.T) {
	long := "\x1b" + strings.Repeat("\x01", 256<<10)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nX-A: " + long + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = buf.Flush()
	}))
	t.Cleanup(ts.Close)
	_, err := directory.Fetch(context.Background(), testClient(ts, 5*time.Second), ts.URL+"/v1/directory")
	if err == nil {
		t.Fatal("Fetch of a response with a malformed header succeeded")
	}
	msg := err.Error()
	if len(msg) > 4096 {
		t.Errorf("error text is %d bytes, want it clipped", len(msg))
	}
	if !strings.Contains(msg, "truncated") || strings.ContainsAny(msg, "\x1b\x01") {
		t.Errorf("error %.200q... should be escaped and say it was truncated", msg)
	}
}

// 地址很长（带了很长的查询串）时，错误里的状态码和失败原因不能被截掉：它们排在地址后面，是排错要看的部分。
func TestFetchErrorKeepsReasonForLongURL(t *testing.T) {
	ts := fetchServer(t, nil)
	// 700 字节的地址：错误文本在 512 到 1024 字节之间，该截而不能放过。
	long := "?" + strings.Repeat("p", 700)
	_, err := directory.Fetch(context.Background(), testClient(ts, 5*time.Second), ts.URL+"/status/404"+long)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || !strings.HasPrefix(err.Error(), "GET https://") {
		t.Errorf("Fetch of a 404 with a long URL = %v, want the error to start with the request and end with HTTP 404", err)
	}
	if err != nil && len(err.Error()) > 600 {
		t.Errorf("error text is %d bytes, want it clipped to about 512", len(err.Error()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = directory.Fetch(ctx, testClient(ts, 5*time.Second), ts.URL+"/slow"+long)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("Fetch past its deadline with a long URL = %v, want the error to say context deadline exceeded", err)
	}
	if err != nil && len(err.Error()) > 600 {
		t.Errorf("error text is %d bytes, want it clipped to about 512", len(err.Error()))
	}
}

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"plain text: 中文（全角）→ ok":  "plain text: 中文（全角）→ ok",
		"a\x1b[2Jb":               `a\x1b[2Jb`,
		"line\nbreak\r":           `line\nbreak\r`,
		"rtl\xe2\x80\xaeoverride": "rtl" + `\` + "u202eoverride", // U+202E 从右到左覆盖
		"del\x7f":                 `del\x7f`,
		"bad\x9b[2J":              `bad\x9b[2J`,     // 不合法的 UTF-8 字节（单独的 0x9b 在有的终端里是 CSI）
		"ok\xef\xbf\xbd":          "ok\xef\xbf\xbd", // 本来就是 U+FFFD 的照样保留
		"":                        "",
	} {
		if got := directory.Printable(in); got != want {
			t.Errorf("Printable(%q) = %q, want %q", in, got, want)
		}
	}
}

// CheckURL 的错误会打到终端和日志：不能带出地址里的用户名密码，哪怕地址写得不成样子
// （漏了 https://，整串被当成 scheme；密码里有不合法的百分号转义，解析错误会带出转义后的两个字符）。
func TestCheckURLHidesCredentials(t *testing.T) {
	for _, u := range []string{
		"https://user:fakepass@relay-list.example.test/v1/directory",
		"http://user:fakepass@relay-list.example.test/v1/directory",
		"https://user:fakepass@relay-list.example.test:bad/v1/directory", // 解析失败
		"https://fakepass@relay-list.example.test/v1/directory",          // 只写了用户名的令牌
		"http://fakepass@relay-list.example.test/v1/directory",
		"user:fakepass@relay-list.example.test/v1/directory", // 漏了 https://
		"fakepass@relay-list.example.test/v1/directory",
		"https://user%zzfakepass@relay-list.example.test/v1/directory", // 解析错误 invalid URL escape "%zz" 会带出百分号后的两个字符
	} {
		err := directory.CheckURL(u)
		if err == nil {
			t.Errorf("CheckURL(%q) = nil, want error", u)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, "fakepass") || strings.Contains(msg, "invalid URL escape") {
			t.Errorf("CheckURL(%q) error %q shows part of the credentials", u, err)
		}
	}
}

func TestNewHTTPClientRedirects(t *testing.T) {
	body := []byte(`{"kind":"directory"}`)
	ts := fetchServer(t, body)
	c := testClient(ts, 5*time.Second)
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"/hop/1", true},
		{"/hop/4", true},
		{"/hop/5", false}, // 最多跟随 4 次跳转（第 5 次请求之后不再跟），与 net/http 默认策略的计数方式一致
		{"/to-http", false},
	} {
		got, err := directory.Fetch(context.Background(), c, ts.URL+tc.path)
		if tc.ok && (err != nil || !bytes.Equal(got, body)) {
			t.Errorf("%s: got %q, err %v; want the body", tc.path, got, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: got %q, want an error", tc.path, got)
		}
	}
}

func TestNewHTTPClientTimeout(t *testing.T) {
	ts := fetchServer(t, nil)
	start := time.Now()
	_, err := directory.Fetch(context.Background(), testClient(ts, 200*time.Millisecond), ts.URL+"/slow")
	if err == nil {
		t.Fatal("Fetch of a server that never answers succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Fetch took %s, the client timeout is 200ms", elapsed)
	}
}

func TestErrorClass(t *testing.T) {
	for _, sentinel := range []error{directory.ErrExpired, directory.ErrNotYetValid, directory.ErrRevoked, directory.ErrRollback,
		directory.ErrBadSignature, directory.ErrMalformed, directory.ErrNoRoots} {
		// 包在 VerifyError 里，原文还带着时间（每次都不同），类别必须稳定。
		err := &directory.VerifyError{Layer: directory.LayerDirectory, Err: sentinel, Msg: time.Now().String()}
		if got := directory.ErrorClass(err); got != sentinel.Error() {
			t.Errorf("ErrorClass(%v) = %q, want %q", err, got, sentinel.Error())
		}
	}
	if got := directory.ErrorClass(errors.New("something else")); got != "invalid" {
		t.Errorf("ErrorClass(other) = %q, want %q", got, "invalid")
	}
	if got := directory.ErrorClass(nil); got != "" {
		t.Errorf("ErrorClass(nil) = %q, want empty", got)
	}
}
