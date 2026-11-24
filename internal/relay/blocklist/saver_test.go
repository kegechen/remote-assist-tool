package blocklist

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// S2：持续每秒 1000 次违规（每次都触发一次新的自动封禁）时，写盘次数被最小间隔合并，
// 文件大小受自动封禁上限约束。
func TestSaverCoalescesWritesUnderViolationFlood(t *testing.T) {
	if testing.Short() {
		t.Skip("耗时测试")
	}
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "bl.json")
	cfg.Threshold = 2
	cfg.MaxTracked = 1000
	cfg.SaveInterval = 500 * time.Millisecond
	cfg.UrgentSaveInterval = 100 * time.Millisecond
	l, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	before := l.SaveCount()
	start := time.Now()
	n := 0
	for time.Since(start) < 2*time.Second {
		for i := 0; i < 100; i++ {
			l.RecordViolation(fmt.Sprintf("10.%d.%d.%d", (n>>16)&0xff, (n>>8)&0xff, n&0xff), KindProtoDecode, "bad_json")
			n++
		}
		time.Sleep(100 * time.Millisecond)
	}
	saves := l.SaveCount() - before
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(cfg.Path)
	t.Logf("2 秒内 %d 次违规（每次都封禁），写盘 %d 次，最终文件 %d 字节", n, saves, info.Size())
	if saves > 6 {
		t.Fatalf("写盘未合并：2 秒写了 %d 次（间隔 500ms 应 ≤ 6）", saves)
	}
	if info.Size() > 1<<20 {
		t.Fatalf("文件 %d 字节超过 1 MiB", info.Size())
	}
}
