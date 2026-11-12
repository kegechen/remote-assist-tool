package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"strings"

	"github.com/remote-assist/tool/internal/agent"
)

// 远程桌面：screen_capture（取屏幕 JPEG）与 input（注入鼠标键盘）。
// 平台相关的捕获/注入由 DesktopOps 抽象，Windows 见 desktop_windows.go，其他平台暂不支持。

const (
	desktopDefaultMaxWidth = 1280
	desktopMinMaxWidth     = 160
	desktopDefaultQuality  = 60
	// desktopMaxJPEGBytes 单帧 JPEG 上限。relay 单条消息上限 4 MiB，base64 后膨胀约 1/3，
	// 这里留足余量；超限时自动降质量重编。
	desktopMaxJPEGBytes = 1 << 20
	desktopMaxEvents    = 200
	desktopMaxTextLen   = 4096
)

// ErrDesktopDisabled 对端未用 --enable-desktop 启动时返回。
var ErrDesktopDisabled = errors.New("desktop_disabled: share 端未启用远程桌面，请带 --enable-desktop 重新启动 share")

// ErrDesktopUnsupported 当前平台没有捕获/注入实现。
var ErrDesktopUnsupported = errors.New("desktop_unsupported: 当前平台暂不支持远程桌面")

// DesktopOps 平台后端。所有坐标均为物理像素（虚拟桌面坐标系）。
type DesktopOps struct {
	// Displays 返回显示器个数（主显示器为 0）。
	Displays func() (int, error)
	// Capture 抓取指定显示器整屏，返回图像及其在虚拟桌面中的矩形。
	Capture func(display int) (*image.RGBA, image.Rectangle, error)
	// Bounds 返回指定显示器在虚拟桌面中的矩形。
	Bounds func(display int) (image.Rectangle, error)
	// Mouse kind: move | down | up | wheel；button: left|right|middle；wheel 用 delta（>0 向下滚）。
	Mouse func(kind string, x, y int, button string, delta int) error
	// Key 按浏览器 KeyboardEvent.key 命名（Enter、ArrowLeft、F5、a ...）。
	Key func(key string, up bool) error
	// Text 直接输入一段 Unicode 文本。
	Text func(s string) error
}

// ---------- screen_capture ----------

type ScreenCaptureArgs struct {
	Display  int `json:"display,omitempty"`
	MaxWidth int `json:"max_width,omitempty"`
	Quality  int `json:"quality,omitempty"`
}

type ScreenCaptureResult struct {
	Format       string `json:"format"`
	Width        int    `json:"width"`         // 返回图像宽
	Height       int    `json:"height"`        // 返回图像高
	ScreenWidth  int    `json:"screen_width"`  // 真实屏幕宽（物理像素）
	ScreenHeight int    `json:"screen_height"` // 真实屏幕高
	Displays     int    `json:"displays"`
	Data         []byte `json:"data"` // JPEG，JSON 中为 base64
}

type ScreenCaptureTool struct {
	enabled bool
	ops     *DesktopOps
}

func NewScreenCapture(enabled bool) *ScreenCaptureTool {
	return &ScreenCaptureTool{enabled: enabled, ops: platformDesktopOps()}
}

func (t *ScreenCaptureTool) Name() string { return "screen_capture" }

func (t *ScreenCaptureTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	if !t.enabled {
		return nil, ErrDesktopDisabled
	}
	if t.ops == nil {
		return nil, ErrDesktopUnsupported
	}
	var a ScreenCaptureArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
	}
	if a.Display < 0 {
		return nil, errors.New("invalid args: display must be >= 0")
	}
	if a.MaxWidth == 0 {
		a.MaxWidth = desktopDefaultMaxWidth
	}
	if a.MaxWidth < desktopMinMaxWidth {
		a.MaxWidth = desktopMinMaxWidth
	}
	if a.Quality == 0 {
		a.Quality = desktopDefaultQuality
	}
	if a.Quality < 1 || a.Quality > 100 {
		return nil, errors.New("invalid args: quality must be 1..100")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	img, rect, err := t.ops.Capture(a.Display)
	if err != nil {
		return nil, fmt.Errorf("capture failed: %w", err)
	}
	n := 1
	if t.ops.Displays != nil {
		if c, e := t.ops.Displays(); e == nil && c > 0 {
			n = c
		}
	}
	out := scaleToWidth(img, a.MaxWidth)
	data, err := encodeJPEGCapped(out, a.Quality, desktopMaxJPEGBytes)
	if err != nil {
		return nil, err
	}
	return json.Marshal(ScreenCaptureResult{
		Format: "jpeg", Width: out.Bounds().Dx(), Height: out.Bounds().Dy(),
		ScreenWidth: rect.Dx(), ScreenHeight: rect.Dy(), Displays: n, Data: data,
	})
}

// scaleToWidth 用面积平均缩小到不超过 maxW 宽；不放大。面积平均比最近邻更利于文字可读。
func scaleToWidth(src *image.RGBA, maxW int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= maxW || sw == 0 {
		return src
	}
	dw := maxW
	dh := sh * dw / sw
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		y0, y1 := dy*sh/dh, (dy+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < dw; dx++ {
			x0, x1 := dx*sw/dw, (dx+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, cnt uint32
			for y := y0; y < y1; y++ {
				off := y*src.Stride + x0*4
				for x := x0; x < x1; x++ {
					r += uint32(src.Pix[off])
					g += uint32(src.Pix[off+1])
					b += uint32(src.Pix[off+2])
					off += 4
					cnt++
				}
			}
			o := dy*dst.Stride + dx*4
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/cnt), uint8(g/cnt), uint8(b/cnt), 255
		}
	}
	return dst
}

// encodeJPEGCapped 编码为 JPEG；超过 limit 时逐步降质量，直到不超限或到最低质量。
func encodeJPEGCapped(img image.Image, quality, limit int) ([]byte, error) {
	for q := quality; ; q -= 15 {
		if q < 10 {
			q = 10
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, fmt.Errorf("jpeg encode: %w", err)
		}
		if buf.Len() <= limit || q == 10 {
			if buf.Len() > limit {
				return nil, fmt.Errorf("frame too large: %d bytes even at quality %d", buf.Len(), q)
			}
			return buf.Bytes(), nil
		}
	}
}

// ---------- input ----------

type InputEvent struct {
	Type   string  `json:"type"`
	X      float64 `json:"x,omitempty"` // 0..1，相对该显示器左上角
	Y      float64 `json:"y,omitempty"`
	Button string  `json:"button,omitempty"`
	DY     int     `json:"dy,omitempty"` // wheel：>0 向下
	Key    string  `json:"key,omitempty"`
	Text   string  `json:"text,omitempty"`
}

type InputArgs struct {
	Display int          `json:"display,omitempty"`
	Events  []InputEvent `json:"events"`
}

type InputResult struct {
	Applied int `json:"applied"`
}

type InputTool struct {
	enabled bool
	ops     *DesktopOps
}

func NewInput(enabled bool) *InputTool {
	return &InputTool{enabled: enabled, ops: platformDesktopOps()}
}

func (t *InputTool) Name() string { return "input" }

func (t *InputTool) Run(ctx context.Context, raw json.RawMessage, _ agent.StreamSink) (json.RawMessage, error) {
	if !t.enabled {
		return nil, ErrDesktopDisabled
	}
	if t.ops == nil {
		return nil, ErrDesktopUnsupported
	}
	var a InputArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if len(a.Events) == 0 {
		return nil, errors.New("invalid args: events is empty")
	}
	if len(a.Events) > desktopMaxEvents {
		return nil, fmt.Errorf("invalid args: too many events (max %d)", desktopMaxEvents)
	}
	if a.Display < 0 {
		return nil, errors.New("invalid args: display must be >= 0")
	}
	// 先整体校验再执行，避免执行到一半才发现后面的事件非法。
	needBounds := false
	for i, ev := range a.Events {
		if err := validateEvent(ev); err != nil {
			return nil, fmt.Errorf("invalid args: events[%d]: %w", i, err)
		}
		switch ev.Type {
		case "move", "down", "up", "click", "dblclick", "wheel":
			needBounds = true
		}
	}
	var rect image.Rectangle
	if needBounds {
		var err error
		if rect, err = t.ops.Bounds(a.Display); err != nil {
			return nil, fmt.Errorf("display bounds: %w", err)
		}
	}
	applied := 0
	for i, ev := range a.Events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := t.apply(rect, ev); err != nil {
			return nil, fmt.Errorf("events[%d] (%s) failed after %d applied: %w", i, ev.Type, applied, err)
		}
		applied++
	}
	return json.Marshal(InputResult{Applied: applied})
}

func validateEvent(ev InputEvent) error {
	switch ev.Type {
	case "move", "down", "up", "click", "dblclick", "wheel":
		if ev.X < 0 || ev.X > 1 || ev.Y < 0 || ev.Y > 1 {
			return errors.New("x/y must be within 0..1")
		}
		if ev.Type != "move" && ev.Type != "wheel" {
			if _, err := normButton(ev.Button); err != nil {
				return err
			}
		}
	case "key_down", "key_up":
		if ev.Key == "" {
			return errors.New("key is required")
		}
	case "text":
		if ev.Text == "" {
			return errors.New("text is required")
		}
		if len(ev.Text) > desktopMaxTextLen {
			return fmt.Errorf("text too long (max %d bytes)", desktopMaxTextLen)
		}
	default:
		return fmt.Errorf("unknown event type %q", ev.Type)
	}
	return nil
}

func normButton(b string) (string, error) {
	switch strings.ToLower(b) {
	case "", "left":
		return "left", nil
	case "right", "middle":
		return strings.ToLower(b), nil
	}
	return "", fmt.Errorf("unknown button %q", b)
}

// toPixel 把 0..1 的归一化坐标映射到虚拟桌面物理像素，落在矩形内（含边界夹取）。
func toPixel(r image.Rectangle, nx, ny float64) (int, int) {
	x := r.Min.X + int(nx*float64(r.Dx()))
	y := r.Min.Y + int(ny*float64(r.Dy()))
	if x >= r.Max.X {
		x = r.Max.X - 1
	}
	if y >= r.Max.Y {
		y = r.Max.Y - 1
	}
	return x, y
}

func (t *InputTool) apply(rect image.Rectangle, ev InputEvent) error {
	o := t.ops
	switch ev.Type {
	case "move":
		x, y := toPixel(rect, ev.X, ev.Y)
		return o.Mouse("move", x, y, "", 0)
	case "down", "up":
		x, y := toPixel(rect, ev.X, ev.Y)
		b, _ := normButton(ev.Button)
		if err := o.Mouse("move", x, y, "", 0); err != nil {
			return err
		}
		return o.Mouse(ev.Type, x, y, b, 0)
	case "click", "dblclick":
		x, y := toPixel(rect, ev.X, ev.Y)
		b, _ := normButton(ev.Button)
		if err := o.Mouse("move", x, y, "", 0); err != nil {
			return err
		}
		times := 1
		if ev.Type == "dblclick" {
			times = 2
		}
		for i := 0; i < times; i++ {
			if err := o.Mouse("down", x, y, b, 0); err != nil {
				return err
			}
			if err := o.Mouse("up", x, y, b, 0); err != nil {
				return err
			}
		}
		return nil
	case "wheel":
		x, y := toPixel(rect, ev.X, ev.Y)
		if err := o.Mouse("move", x, y, "", 0); err != nil {
			return err
		}
		return o.Mouse("wheel", x, y, "", ev.DY)
	case "key_down":
		return o.Key(ev.Key, false)
	case "key_up":
		return o.Key(ev.Key, true)
	case "text":
		return o.Text(ev.Text)
	}
	return fmt.Errorf("unknown event type %q", ev.Type)
}
