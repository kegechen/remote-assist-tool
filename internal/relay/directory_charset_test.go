package relay

import (
	"testing"

	"github.com/remote-assist/tool/internal/directory"
)

// 节点前缀就是协助码的首字符：节点列表允许的前缀字符集必须与 relay 发码用的字符集完全一致，
// 否则会出现 relay 发得出、却路由不到任何节点的码（或反过来）。
func TestDirectoryCharsetMatchesCodeCharset(t *testing.T) {
	if directory.CodeCharset != charset {
		t.Fatalf("directory.CodeCharset = %q, relay charset = %q", directory.CodeCharset, charset)
	}
}
