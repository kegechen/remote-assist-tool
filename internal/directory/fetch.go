package directory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 取列表（客户端与 relay 共用）。列表的真实性全靠根签名，取列表的地址与传输都不被信任：
// 拿到的原文一律交给 Verify / Cache.Offer。这里只管把请求限死，免得被一个坏地址拖住或撑爆。

// DefaultURL 是官方节点列表的公开读取地址（spec 6.2、第 8 节）。客户端默认从这里取列表，
// 可用环境变量 REMOTE_RELAY_DIRECTORY 覆盖。
const DefaultURL = "https://relay-list.ifly.qzz.io/v1/directory"

// maxRedirects 是取列表时请求的总数上限（含第一次）：最多跟随 maxRedirects-1 次跳转，计数方式同 net/http 的默认策略。
const maxRedirects = 5

// CheckURL 校验列表地址：必须是带主机名、不带用户名密码的 https 地址。
// 错误会打到终端和日志，所以一个字都不回显地址：写错了的地址里可能有令牌（漏写 https:// 时整串会被当成 scheme，
// 只写了用户名的往往就是令牌），url.Parse 的错误也会带出转义里的字符。调用方的提示里已经说明是哪个设置项。
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("directory URL is not a valid URL")
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return errors.New("directory URL must be an https URL with a host and no credentials")
	}
	return nil
}

// NewHTTPClient 返回取列表用的 HTTP 客户端：总超时 timeout（含读完响应），只跟随 https 跳转，次数有上限。
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: checkRedirect}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-https URL %s", req.URL.Redacted())
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d requests", maxRedirects)
	}
	return nil
}

// Fetch 用 GET 取一份列表原文，不验签。只接受 200，原文不超过 MaxRawSize。
//
// 错误会打到终端上，而里面有服务器决定的文本：传输层的错误带着坏掉的响应头、证书里的名字，
// 可能有终端控制字符，也可能有好几 MB。所以错误文本一律截短并经过 Printable（错误链不变，errors.Is 照常可用）；
// 非 200 时只报状态码，状态行的原因短语根本不放进来。
func Fetch(ctx context.Context, client *http.Client, url string) (data []byte, err error) {
	defer func() {
		if err != nil {
			err = &printableError{msg: clipPrintable(err.Error()), err: err}
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, MaxRawSize+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if len(data) > MaxRawSize {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", url, MaxRawSize)
	}
	return data, nil
}

// Printable 把 s 里不可打印的字符（控制字符、从右到左覆盖这类格式字符、不合法的 UTF-8）写成 Go 的转义形式，
// 其余原样保留。要打到终端、写进日志的文本里有别人决定的部分时用它：一个 ESC 就能清屏、改颜色、伪造后面的提示。
func Printable(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i]) // 不合法的字节：有的终端把单独的 0x9b 当作控制序列的开头
		case unicode.IsPrint(r):
			b.WriteString(s[i : i+size])
		default:
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String()
}

// maxErrorText 是错误文本最多保留的字节数（转义之前）。坏掉的响应头会整行拼进传输层的错误，一行可以有好几 MB；
// relay 每次刷新失败都打一行日志，客户端也会把它打到终端、写进日志文件。正常的错误远不到这么长。
const maxErrorText = 512

// clipPrintable 把 s 截到 maxErrorText 字节，再经过 Printable。截掉的是中间：错误形如「GET <很长的地址>: HTTP 404」
// 「Get "<地址>": <原因>」，排错要看的状态码和原因都排在地址后面，只留开头会把它们吃掉。
// 截断可能落在一个字符中间，剩下的半个字符由 Printable 写成 \x 转义，成不了控制序列。
func clipPrintable(s string) string {
	if len(s) > maxErrorText {
		s = s[:maxErrorText/2] + "...(truncated)..." + s[len(s)-maxErrorText/2:]
	}
	return Printable(s)
}

// printableError 是文本经过 clipPrintable 的错误，错误链不变。
type printableError struct {
	msg string
	err error
}

func (e *printableError) Error() string { return e.msg }
func (e *printableError) Unwrap() error { return e.err }

// ErrorClass 把验证错误归成稳定的类别（哨兵错误的原文），用来判断状态有没有变化：错误原文里带着时间，每次都不同。
// 不是本包验证错误的返回 "invalid"，nil 返回空串。
func ErrorClass(err error) string {
	if err == nil {
		return ""
	}
	for _, e := range []error{ErrExpired, ErrNotYetValid, ErrRevoked, ErrRollback, ErrBadSignature, ErrMalformed, ErrNoRoots} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return "invalid"
}
