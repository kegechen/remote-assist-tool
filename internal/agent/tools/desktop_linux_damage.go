//go:build linux

package tools

import (
	"image"
	"time"

	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/xproto"
)

// XDamage 增量抓屏：X server 在屏幕内容变化时发 DamageNotify（变化的包围盒），
// 我们据此只重取变化区域并合并进常驻帧缓冲；完全没变化时连 GetImage 都省掉。
// 保险：每 damageFullRefresh 强制整幅重取一次，兜住不上报 damage 的内容（如视频叠加层）。
// 不可用（无扩展、多显示器、尺寸变化）时回退整幅 GetImage。
const damageFullRefresh = 2 * time.Second

type x11Damage struct {
	tried  bool
	ok     bool
	id     damage.Damage
	fb     *image.RGBA // 常驻帧缓冲：整个根窗口；写时复制，已发布的图像不再被修改
	dirty  image.Rectangle
	fullAt time.Time
}

func (b *x11Backend) damageInit() bool {
	if b.dmg.tried {
		return b.dmg.ok
	}
	b.dmg.tried = true
	if damage.Init(b.conn) != nil {
		return false
	}
	if _, err := damage.QueryVersion(b.conn, 1, 1).Reply(); err != nil {
		return false
	}
	id, err := damage.NewDamageId(b.conn)
	if err != nil {
		return false
	}
	if damage.CreateChecked(b.conn, id, xproto.Drawable(b.root), damage.ReportLevelBoundingBox).Check() != nil {
		return false
	}
	b.dmg.id, b.dmg.ok = id, true
	return true
}

// drainDamage 取出积压的 DamageNotify，并入 dirty；之后 Subtract 复位以便继续收到事件。
func (b *x11Backend) drainDamage() {
	got := false
	for {
		ev, _ := b.conn.PollForEvent()
		if ev == nil {
			break
		}
		if e, ok := ev.(damage.NotifyEvent); ok {
			r := image.Rect(int(e.Area.X), int(e.Area.Y), int(e.Area.X)+int(e.Area.Width), int(e.Area.Y)+int(e.Area.Height))
			b.dmg.dirty = b.dmg.dirty.Union(r)
			got = true
		}
	}
	if got {
		_ = damage.Subtract(b.conn, b.dmg.id, 0, 0).Check()
	}
}

// captureHinted 返回当前整幅根窗口图像（单显示器快速路径）。
// prev：本次图像所基于的上一幅图像（可能为 nil）；dirty：相对 prev 变化的区域。二者让调用方只重算变化部分的哈希。
func (b *x11Backend) captureHinted(root image.Rectangle) (img, prev *image.RGBA, dirty image.Rectangle, ok bool) {
	if !b.damageInit() {
		return nil, nil, image.Rectangle{}, false
	}
	b.drainDamage()
	full := b.dmg.fb == nil || b.dmg.fb.Bounds() != root || time.Since(b.dmg.fullAt) >= damageFullRefresh
	d := b.dmg.dirty.Intersect(root)
	if !full && d.Empty() {
		return b.dmg.fb, b.dmg.fb, image.Rectangle{}, true // 没有变化：同一个对象
	}
	if full {
		d = root
	}
	next := image.NewRGBA(root)
	if b.dmg.fb != nil && b.dmg.fb.Bounds() == root {
		copy(next.Pix, b.dmg.fb.Pix)
	}
	if err := b.grabInto(next, d); err != nil {
		return nil, nil, image.Rectangle{}, false
	}
	prev = b.dmg.fb
	b.dmg.fb, b.dmg.dirty = next, image.Rectangle{}
	if full {
		b.dmg.fullAt = time.Now()
		if prev == nil || prev.Bounds() != root {
			return next, nil, image.Rectangle{}, true
		}
		// 整幅刷新：与 prev 的差异未知，调用方须整幅重算（dirty=整幅）
		return next, prev, root, true
	}
	return next, prev, d, true
}

// grabInto 把根窗口矩形 d 抓进 dst（RGBA，坐标即根窗口坐标）。
func (b *x11Backend) grabInto(dst *image.RGBA, d image.Rectangle) error {
	w, h := d.Dx(), d.Dy()
	var data []byte
	if s, err := b.shmGetImage(int16(d.Min.X), int16(d.Min.Y), uint16(w), uint16(h)); err == nil {
		data = s
	} else {
		reply, err := xproto.GetImage(b.conn, xproto.ImageFormatZPixmap, xproto.Drawable(b.root),
			int16(d.Min.X), int16(d.Min.Y), uint16(w), uint16(h), 0xffffffff).Reply()
		if err != nil {
			b.drop()
			return err
		}
		if len(reply.Data) < w*h*4 {
			return errShortImage
		}
		data = reply.Data
	}
	sub := dst.SubImage(d).(*image.RGBA)
	swizzleToRGBA(sub, data, w*4, 2, 1, 0)
	return nil
}
