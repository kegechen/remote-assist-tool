package gui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/remote-assist/tool/internal/gui/assets"
)

// 上游评审指出：被控端返回的 screen_width/screen_height 未转义就拼进 innerHTML，
// 恶意或被篡改的被控端可借此在持有 token 的 GUI 页面里执行脚本；displays 与 canvas 宽高也无上限，
// 能被极大值卡死页面。行为由 Playwright 端到端验证（伪造恶意响应），这里锁住关键接线，防止回归。
func TestDeskPeerDataIsSanitized(t *testing.T) {
	html := strings.ReplaceAll(string(assets.IndexHTML), "\r\n", "\n")

	// 1) 有统一的净化入口，且帧循环解析后立即使用它
	if !strings.Contains(html, "function deskSanitize(d)") {
		t.Fatal("缺少 deskSanitize：对端返回的字段必须在使用前统一净化")
	}
	if !strings.Contains(html, "d=deskSanitize(parseMCP(r.result))") {
		t.Error("帧循环必须在 parseMCP 之后立即 deskSanitize，之后只使用净化后的值")
	}

	// 2) 上限常量存在且合理
	for _, want := range []string{"DESK_MAX_DIM=16384", "DESK_MAX_DISPLAYS=16"} {
		if !strings.Contains(html, want) {
			t.Errorf("缺少上限常量 %q", want)
		}
	}

	// 3) 显示器列表自身也限幅（防御纵深：即使有人传入未净化值也不会卡死）
	i := strings.Index(html, "function deskSyncDisplays(n){")
	if i < 0 {
		t.Fatal("找不到 deskSyncDisplays")
	}
	if !strings.Contains(html[i:i+200], "deskInt(n,1,DESK_MAX_DISPLAYS,1)") {
		t.Error("deskSyncDisplays 必须先对 n 限幅到 DESK_MAX_DISPLAYS")
	}

	// 4) 画布尺寸只在合法时赋值
	if !strings.Contains(html, "if(d.width>0&&d.height>0&&(cv.width!==d.width||cv.height!==d.height)){cv.width=d.width;cv.height=d.height}") {
		t.Error("canvas 宽高赋值前必须确认是净化后的合法正数")
	}

	// 5) 通用不变式：任何把 d.xxx（对端返回的原始字段）直接拼进 innerHTML 的写法都不允许。
	// 抓“innerHTML= ... +d.<字段>”这种模式；净化后的值应先放进局部 const（sw/sh/fpsT 等）再拼。
	bad := regexp.MustCompile(`innerHTML\s*=[^;\n]*[+]\s*d\.[A-Za-z_]+`)
	if m := bad.FindString(html); m != "" {
		t.Errorf("发现把对端字段直接拼进 innerHTML 的写法: %s", m)
	}
}

// 统计栏里拼进 innerHTML 的分辨率必须是 deskInt 转出的整数，而不是原始的 d.screen_width。
func TestDeskStatUsesIntegerResolution(t *testing.T) {
	html := strings.ReplaceAll(string(assets.IndexHTML), "\r\n", "\n")
	if !strings.Contains(html, "const sw=deskInt(d.screen_width||desk.sw,0,DESK_MAX_DIM,0),sh=deskInt(d.screen_height||desk.sh,0,DESK_MAX_DIM,0);") {
		t.Error("统计栏的 sw/sh 必须经 deskInt 转成有界整数")
	}
}
