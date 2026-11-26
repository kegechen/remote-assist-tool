package e2e

import (
	"testing"
	"time"
)

// waitCode 要取出完整的协助码：12 位码显示成三段（XXXX-XXXX-XXXX），只取前两段的话截成 8 位，拿去加入只会被 relay
// 拒绝，依赖它的 e2e 全挂。旧版的 8 位码只有两段，照样要认得。不需要预编的程序，CI 上也跑。
func TestWaitCodeKeepsWholeCode(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		{"协助码: AB2C-DE3F-GH4J（已复制到剪贴板）\n", "AB2CDE3FGH4J"},
		{"协助码：AB2C-DE3F\n", "AB2CDE3F"},
	} {
		buf := &lockedBuffer{}
		_, _ = buf.Write([]byte(tc.out))
		if got := waitCode(t, buf, 50*time.Millisecond); got != tc.want {
			t.Errorf("waitCode(%q) = %q，期望 %q", tc.out, got, tc.want)
		}
	}
}

// extractCode 从 share 重连后的输出里取新码，同样要认得三段的 12 位码；「原协助码 X 无法继续使用」「协助码仍有效: X」
// 里的旧码不能当成新码。
func TestExtractCodeKeepsWholeCode(t *testing.T) {
	out := "连接中断: read tcp: wsarecv: forcibly closed.，协助码仍有效: JGDC-QGE8-Y3BD，正在重连...\n" +
		"[!] 原协助码 JGDC-QGE8-Y3BD 无法继续使用，协助码已变更为 RVVB-SGJ7-N7BN，请把新码告知协助端\n" +
		"协助码: RVVB-SGJ7-N7BN\n"
	if got := extractCode(out); got != "RVVBSGJ7N7BN" {
		t.Fatalf("extractCode = %q，期望 RVVBSGJ7N7BN", got)
	}
	if got := extractCode("协助码：AB2C-DE3F\n"); got != "AB2CDE3F" {
		t.Fatalf("旧版 8 位码 extractCode = %q，期望 AB2CDE3F", got)
	}
}
