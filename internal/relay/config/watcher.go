package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// 监听默认值。配置文件很小，轮询 stat 的开销可以忽略；仓库没有 fsnotify 依赖，轮询也避免了
// 不同平台文件事件语义（rename、编辑器先删后写、网络盘丢事件）的差异。
const (
	DefaultPollInterval   = time.Second
	DefaultDebounce       = 500 * time.Millisecond
	DefaultStableDelay    = 100 * time.Millisecond
	DefaultResyncInterval = time.Minute
	minPollInterval       = 10 * time.Millisecond
	// unstableReportAfter 文件持续「仍在写入/被锁定」超过这么久就告警一次（之后继续重试）。
	unstableReportAfter = time.Minute
)

// WatchOptions 配置文件监听参数；零值字段使用默认值。
type WatchOptions struct {
	// Path 统一配置文件的绝对路径。
	Path string
	// PollInterval 检查文件元数据（存在性、大小、修改时间）的间隔。
	PollInterval time.Duration
	// Debounce 最后一次观察到变化后需保持静默多久才读取，合并编辑器/部署脚本的连续写入。
	Debounce time.Duration
	// StableDelay 两次读取之间的间隔；两次元数据与内容一致才认为文件已写完。
	StableDelay time.Duration
	// ResyncInterval 即使元数据没变也定期重读一次（兜住修改时间精度不足的文件系统）；负数关闭。
	ResyncInterval time.Duration
}

func (o WatchOptions) withDefaults() WatchOptions {
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.PollInterval < minPollInterval {
		o.PollInterval = minPollInterval
	}
	if o.Debounce <= 0 {
		o.Debounce = DefaultDebounce
	}
	if o.StableDelay <= 0 {
		o.StableDelay = DefaultStableDelay
	}
	if o.ResyncInterval == 0 {
		o.ResyncInterval = DefaultResyncInterval
	}
	return o
}

// fileSig 是一次 stat 观察到的文件元数据。
type fileSig struct {
	exists  bool
	isDir   bool
	size    int64
	modTime time.Time
	mode    os.FileMode
}

func (a fileSig) equal(b fileSig) bool {
	return a.exists == b.exists && a.isDir == b.isDir && a.size == b.size && a.modTime.Equal(b.modTime) && a.mode == b.mode
}

// Watcher 轮询监听统一配置文件，对变化做防抖与稳定读取后交给 Reloader。
//
// 只看目标路径而不持有文件句柄，因此「写临时文件后 rename 覆盖」「先删后写」「目录被整体替换」
// 都会表现为元数据变化而被发现；文件暂时缺失时同样经过防抖，避免替换窗口里误判为删除。
type Watcher struct {
	opts     WatchOptions
	reloader *Reloader
	logf     func(format string, args ...any)
	now      func() time.Time
	sleep    func(time.Duration)

	sig        fileSig
	sigKnown   bool
	pending    bool
	changedAt  time.Time
	lastRead   time.Time
	warnedMode os.FileMode // 已告警过的权限位；权限不变时不重复告警

	unstableSince    time.Time // 连续读到不稳定内容的起点；稳定读取后清零
	unstableReported bool
}

// NewWatcher 创建监听器。Path 必须是绝对路径，避免服务模式下工作目录不同导致读错文件。
func NewWatcher(opts WatchOptions, reloader *Reloader) (*Watcher, error) {
	if opts.Path == "" || !filepath.IsAbs(opts.Path) {
		return nil, fmt.Errorf("module config path must be absolute: %q", opts.Path)
	}
	opts.Path = filepath.Clean(opts.Path)
	return &Watcher{
		opts:     opts.withDefaults(),
		reloader: reloader,
		logf:     log.Printf,
		now:      time.Now,
		sleep:    time.Sleep,
	}, nil
}

// LoadInitial 启动时同步读取一次（不防抖，但仍要求读取稳定），返回加载结果。
func (w *Watcher) LoadInitial(ctx context.Context) ReloadReport {
	var data []byte
	var sig fileSig
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		data, sig, err = w.readStable()
		if !errors.Is(err, errUnstable) {
			break
		}
	}
	w.sig, w.sigKnown, w.pending, w.lastRead = sig, true, false, w.now()
	if errors.Is(err, errUnstable) {
		// 启动时文件一直在写：交给轮询在防抖后继续尝试，不计为一次失败。
		w.pending, w.changedAt = true, w.now()
		return w.reloader.Pending(err)
	}
	return w.deliver(ctx, data, sig, err)
}

// Run 按 PollInterval 轮询直到 ctx 取消。
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

// poll 执行一次检查；只有真正读取并交给 Reloader 时返回 true。
func (w *Watcher) poll(ctx context.Context) (ReloadReport, bool) {
	now := w.now()
	sig, statErr := statSig(w.opts.Path)
	if statErr != nil {
		// stat 本身失败（权限等）：当作一次变化，防抖后由 readStable 给出具体错误。
		sig = fileSig{}
	}
	if !w.sigKnown || !sig.equal(w.sig) {
		if w.pending && w.unstableSince.IsZero() {
			// 防抖期间又变了：文件可能一直在被改写，同样计入不稳定时长。
			w.unstableSince = w.changedAt
		}
		w.sig, w.sigKnown, w.pending, w.changedAt = sig, true, true, now
		return w.reportLongUnstable(now)
	}
	if !w.pending {
		if w.opts.ResyncInterval < 0 || now.Sub(w.lastRead) < w.opts.ResyncInterval {
			return ReloadReport{}, false
		}
	} else if now.Sub(w.changedAt) < w.opts.Debounce {
		return ReloadReport{}, false
	}
	data, readSig, err := w.readStable()
	if errors.Is(err, errUnstable) {
		// 仍在写入：重新开始防抖。长期如此（例如某进程一直独占锁住文件）时告警一次。
		w.sig, w.pending, w.changedAt = readSig, true, now
		if w.unstableSince.IsZero() {
			w.unstableSince = now
		}
		return w.reportLongUnstable(now)
	}
	w.unstableSince, w.unstableReported = time.Time{}, false
	w.sig, w.pending, w.lastRead = readSig, false, now
	return w.deliver(ctx, data, readSig, err), true
}

// reportLongUnstable 在文件持续改写或被锁定超过 unstableReportAfter 时告警一次（不改变当前配置）。
func (w *Watcher) reportLongUnstable(now time.Time) (ReloadReport, bool) {
	if w.unstableSince.IsZero() || w.unstableReported || now.Sub(w.unstableSince) < unstableReportAfter {
		return ReloadReport{}, false
	}
	w.unstableReported = true
	err := fmt.Errorf("module config %s has been changing or locked for %s; still retrying", w.opts.Path, now.Sub(w.unstableSince).Round(time.Second))
	return w.reloader.Reject(err), true
}

func (w *Watcher) deliver(ctx context.Context, data []byte, sig fileSig, err error) ReloadReport {
	switch {
	case err != nil:
		return w.reloader.Reject(err)
	case !sig.exists:
		return w.reloader.Reject(fmt.Errorf("%w: %s (keeping the current module configuration)", ErrNoConfig, w.opts.Path))
	}
	w.warnPermissions(sig)
	return w.reloader.Load(ctx, data, w.opts.Path)
}

// warnPermissions 在类 Unix 系统上提示组/其他用户可写的配置文件（只提示一次，不拒绝加载）。
func (w *Watcher) warnPermissions(sig fileSig) {
	perm := sig.mode.Perm()
	if runtime.GOOS == "windows" || perm&0o022 == 0 || perm == w.warnedMode {
		return
	}
	w.warnedMode = perm
	w.logf("WARNING: module config %s is writable by group/others (mode %s); restrict it to the relay service account", w.opts.Path, sig.mode.Perm())
}

var errUnstable = errors.New("module config file is still being written")

// readStable 读两次，元数据与内容都一致才返回；文件不存在时返回 exists=false 且无错误。
func (w *Watcher) readStable() ([]byte, fileSig, error) {
	first, sig1, err := readOnce(w.opts.Path)
	if err != nil || !sig1.exists {
		return nil, sig1, err
	}
	w.sleep(w.opts.StableDelay)
	second, sig2, err := readOnce(w.opts.Path)
	if err != nil {
		return nil, sig2, err
	}
	if !sig2.exists || !sig1.equal(sig2) || !bytes.Equal(first, second) {
		return nil, sig2, errUnstable
	}
	return second, sig2, nil
}

func readOnce(path string) ([]byte, fileSig, error) {
	sig, err := statSig(path)
	if err != nil || !sig.exists {
		return nil, sig, err
	}
	if sig.isDir {
		return nil, sig, fmt.Errorf("module config path is a directory: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fileSig{}, nil
		}
		if isTransientOpenError(err) {
			return nil, sig, errUnstable
		}
		return nil, sig, fmt.Errorf("open module config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, sig, fmt.Errorf("read module config: %w", err)
	}
	if len(data) > MaxFileBytes {
		return nil, sig, fmt.Errorf("module config exceeds %d bytes", MaxFileBytes)
	}
	after, err := statSig(path)
	if err != nil {
		return nil, after, err
	}
	if !after.equal(sig) || int64(len(data)) != sig.size {
		return nil, after, errUnstable
	}
	return data, sig, nil
}

func statSig(path string) (fileSig, error) {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileSig{}, nil
		}
		return fileSig{}, fmt.Errorf("stat module config: %w", err)
	}
	return fileSig{exists: true, isDir: st.IsDir(), size: st.Size(), modTime: st.ModTime(), mode: st.Mode()}, nil
}
