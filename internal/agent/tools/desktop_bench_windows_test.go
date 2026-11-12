//go:build windows

package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// 分段计时：抓屏 / 缩放 / JPEG / JSON+base64，需交互桌面。
func TestWindowsStageTiming(t *testing.T) {
	ops := platformDesktopOps()
	if _, _, err := ops.Capture(0); err != nil {
		t.Skip(err)
	}
	const n = 40
	var tc, ts, te, tj time.Duration
	for i := 0; i < n; i++ {
		a := time.Now()
		img, rect, _ := ops.Capture(0)
		b := time.Now()
		out := scaleToWidth(img, 1280)
		c := time.Now()
		data, _ := encodeJPEGCapped(out, 60, desktopMaxJPEGBytes)
		d := time.Now()
		_, _ = json.Marshal(ScreenCaptureResult{Format: "jpeg", Width: out.Bounds().Dx(), Height: out.Bounds().Dy(), ScreenWidth: rect.Dx(), Data: data})
		e := time.Now()
		tc += b.Sub(a)
		ts += c.Sub(b)
		te += d.Sub(c)
		tj += e.Sub(d)
	}
	t.Logf("每帧平均 抓屏=%v 缩放=%v JPEG=%v JSON+b64=%v 合计=%v", tc/n, ts/n, te/n, tj/n, (tc+ts+te+tj)/n)
}

func TestWindowsRunTiming(t *testing.T) {
	sc := NewScreenCapture(true)
	if _, err := sc.Run(context.Background(), nil, nil); err != nil {
		t.Skip(err)
	}
	const n = 40
	st := time.Now()
	for i := 0; i < n; i++ {
		sc.Run(context.Background(), json.RawMessage(`{"max_width":1280,"quality":60}`), nil)
	}
	t.Logf("Run 端到端每帧 %v", time.Since(st)/n)
}


func TestWindowsCaptureOnly(t *testing.T) {
	if _, _, err := winCapture(0); err != nil {
		t.Skip(err)
	}
	const n = 40
	st := time.Now()
	for i := 0; i < n; i++ {
		winCapture(0)
	}
	t.Logf("winCapture(复用DIB section)每帧 %v", time.Since(st)/n)
}
