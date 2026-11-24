package tools

import (
	"context"
	"encoding/json"
	"image"
	"sync/atomic"
	"testing"
	"time"
)

func waitOps(frame *atomic.Pointer[image.RGBA]) *DesktopOps {
	return &DesktopOps{
		Displays: func() (int, error) { return 1, nil },
		Bounds:   func(int) (image.Rectangle, error) { return image.Rect(0, 0, 128, 96), nil },
		Capture: func(int) (*image.RGBA, image.Rectangle, error) {
			return frame.Load(), image.Rect(0, 0, 128, 96), nil
		},
	}
}

func lpCap(t *testing.T, tool *ScreenCaptureTool, args string) ScreenCaptureResult {
	t.Helper()
	raw, err := tool.Run(context.Background(), json.RawMessage(args), nil)
	if err != nil {
		t.Fatal(err)
	}
	var r ScreenCaptureResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// 带 wait 的请求：画面一变立即返回新帧，而不是等满；到期仍没变则返回 unchanged，并标注 waited。
func TestScreenCaptureLongPoll(t *testing.T) {
	var frame atomic.Pointer[image.RGBA]
	a := image.NewRGBA(image.Rect(0, 0, 128, 96))
	frame.Store(a)
	tool := &ScreenCaptureTool{enabled: true, ops: waitOps(&frame)}
	first := lpCap(t, tool, `{"quality":60}`)
	if first.Seq == 0 || first.Unchanged {
		t.Fatalf("首帧异常: %+v", first)
	}

	t0 := time.Now()
	r := lpCap(t, tool, `{"quality":60,"since":`+u64s(first.Seq)+`,"wait":300}`)
	if !r.Unchanged || !r.Waited || r.WaitMS < 250 || time.Since(t0) < 250*time.Millisecond {
		t.Fatalf("没变化时应等到期后返回 unchanged: %+v 用时 %v", r, time.Since(t0))
	}

	go func() {
		time.Sleep(80 * time.Millisecond)
		b := image.NewRGBA(image.Rect(0, 0, 128, 96))
		b.Pix[0] = 200
		frame.Store(b)
	}()
	t0 = time.Now()
	r = lpCap(t, tool, `{"quality":60,"since":`+u64s(first.Seq)+`,"wait":2000,"delta":true}`)
	if r.Unchanged || len(r.Data) == 0 || r.Seq == first.Seq {
		t.Fatalf("画面变化后应返回新帧: %+v", r)
	}
	if d := time.Since(t0); d > 600*time.Millisecond {
		t.Fatalf("变化后应很快返回，实际 %v", d)
	}
	if r.WaitMS < 50 || r.WaitMS > 500 {
		t.Fatalf("wait_ms 应反映实际等待: %d", r.WaitMS)
	}

	// 不带 wait：行为不变，立即返回 unchanged
	t0 = time.Now()
	r = lpCap(t, tool, `{"quality":60,"since":`+u64s(r.Seq)+`}`)
	if !r.Unchanged || r.Waited || time.Since(t0) > 200*time.Millisecond {
		t.Fatalf("无 wait 应立即返回: %+v", r)
	}
}

func TestScreenCaptureLongPollHonorsCancel(t *testing.T) {
	var frame atomic.Pointer[image.RGBA]
	frame.Store(image.NewRGBA(image.Rect(0, 0, 128, 96)))
	tool := &ScreenCaptureTool{enabled: true, ops: waitOps(&frame)}
	first := lpCap(t, tool, `{}`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := tool.Run(ctx, json.RawMessage(`{"since":`+u64s(first.Seq)+`,"wait":2000}`), nil)
	if err == nil || time.Since(t0) > time.Second {
		t.Fatalf("取消后应尽快返回错误: err=%v 用时 %v", err, time.Since(t0))
	}
}

// 负数 wait 表示「不等待」：必须按 0 处理，不能挂起，也不能回报 waited。
func TestScreenCaptureNegativeWaitDoesNotBlock(t *testing.T) {
	var frame atomic.Pointer[image.RGBA]
	frame.Store(image.NewRGBA(image.Rect(0, 0, 128, 96)))
	tool := &ScreenCaptureTool{enabled: true, ops: waitOps(&frame)}
	first := lpCap(t, tool, `{"quality":60}`)
	t0 := time.Now()
	r := lpCap(t, tool, `{"quality":60,"since":`+u64s(first.Seq)+`,"wait":-1}`)
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Fatalf("负 wait 不应挂起，实际耗时 %v", d)
	}
	if !r.Unchanged || r.Waited || r.WaitMS > 100 {
		t.Fatalf("负 wait 应立即返回 unchanged 且 waited=false: %+v", r)
	}
}

// since==0（对端没有可复用的旧帧）时不会进入长轮询等待，waited 必须为 false。
func TestScreenCaptureWaitedFalseWithoutSince(t *testing.T) {
	var frame atomic.Pointer[image.RGBA]
	frame.Store(image.NewRGBA(image.Rect(0, 0, 128, 96)))
	tool := &ScreenCaptureTool{enabled: true, ops: waitOps(&frame)}
	r := lpCap(t, tool, `{"quality":60,"wait":2000}`)
	if r.Unchanged || r.Waited || r.WaitMS > 100 {
		t.Fatalf("since==0 不应等待: %+v", r)
	}
}
