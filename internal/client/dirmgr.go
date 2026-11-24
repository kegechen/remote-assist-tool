package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

// 节点列表（spec 6.2）：自动模式靠它选节点（share）、按协助码首位找节点（help）。
//
// 三个来源拿到的是同一份签名内容：CF（directory.DefaultURL，可用 REMOTE_RELAY_DIRECTORY 覆盖）、relay 随注册/加入响应
// 转发的、本地缓存 ~/.remote_assist_relays.json。前两个一律经 directory.Cache.Offer 验签、按位置取新后写进缓存；
// 用的时候每次从缓存读出来重新验签（Current）：share、help、MCP 多个进程共用这个文件，别的进程随时可能写进更新的列表或新的吊销。
//
// 这里不管怎么选节点（nodepick.go），也不决定什么时候用列表。没有管理器（nil）或没有可用的列表时，调用方退回内置默认节点，
// 与没有这个功能时完全一样。

const (
	// directoryEnv 覆盖 CF 地址；设成空表示不从 CF 取列表（缓存与 relay 转发照常）。
	directoryEnv = "REMOTE_RELAY_DIRECTORY"
	// directoryCacheFile 是缓存文件名，放在用户主目录，与 known_hosts、client_id 同一套约定。
	directoryCacheFile = ".remote_assist_relays.json"
	// directoryFetchTimeout 是从 CF 取一次列表的上限：和连接并行，不拖慢连接；help 找不到前缀时最多为它等这么久。
	directoryFetchTimeout = 3 * time.Second
	// directoryRefreshEvery：share 每隔这么久从 CF 取一次；help 的缓存超过这个年龄才刷新。
	directoryRefreshEvery = 6 * time.Hour
	// directoryRetryDelay：RefreshIfStale 上次向 CF 发过请求后，这么久之内不再发。CF 不通时，
	// MCP 每次连接都会问一次，不能每次都多等 3 秒、多一个请求。
	directoryRetryDelay = time.Minute
)

// errDirectoryOff 表示没有可以取列表的地方：没有管理器，或 CF 地址被设成了空。
var errDirectoryOff = errors.New("node directory fetching is off")

// dirManager 管理一台机器上的节点列表缓存。方法对 nil 安全（当作没有列表）。
type dirManager struct {
	path  string // 缓存文件
	url   string // CF 地址；空表示不取 CF
	roots directory.Roots
	http  *http.Client
	now   func() time.Time
	// logf 输出面向用户的提示（一次一行，format 不带换行）。可能在后台 goroutine 里被并发调用。
	logf  func(format string, args ...any)
	every time.Duration // Run 的间隔

	mu       sync.Mutex
	inflight *dirFetch // 正在进行的 CF 请求；nil 表示没有
	// 下面两个时间只存墙上时间（Round(0) 去掉单调读数）：Linux、macOS 的单调时钟在睡眠时不走，
	// 带着它比较，笔记本睡一夜醒来，「6 小时没确认过」要再醒着过 6 小时才成立。
	attemptedAt time.Time         // 本进程上次向 CF 发请求的时间
	checkedAt   time.Time         // 本进程上次从 CF 确认过列表的时间（收下了，或 CF 那份不比缓存新）
	notes       map[string]string // 每类提示上次的状态：状态变了才再提示
	skipped     map[skipKey]int   // 上次列表更新时每种丢弃各有几项
}

// skipKey 标识一种丢弃：哪个节点（前缀与证书序号，都取自没验过签的证书正文，只用于提示去重）、什么原因（类别）。
// 读不出前缀的坏项都是 ("", 0)，所以要按项数比：多出来的才提示。
type skipKey struct {
	prefix string
	serial uint64
	class  string
}

// dirFetch 是一次 CF 请求；done 关闭后 err 是结果。
type dirFetch struct {
	done chan struct{}
	err  error
}

// newDirManager 返回默认配置的管理器：缓存放在主目录，CF 地址取自 REMOTE_RELAY_DIRECTORY（未设置时用内置地址）。
// 拿不到主目录、本程序没有内置根公钥时返回 nil。每个进程只建一个：合并请求、限制重试、提示去重的状态都在实例里，
// 每次连接新建一个，这些就都不起作用了。
func newDirManager(logf func(format string, args ...any)) *dirManager {
	roots := directory.BuiltinRoots()
	if !roots.Usable() {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return &dirManager{
		path:  filepath.Join(home, directoryCacheFile),
		url:   directoryURLFromEnv(logf),
		roots: roots,
		http:  directory.NewHTTPClient(directoryFetchTimeout),
		now:   time.Now,
		logf:  logf,
		every: directoryRefreshEvery,
	}
}

// directoryURLFromEnv 返回要用的 CF 地址：没设 REMOTE_RELAY_DIRECTORY 时是内置地址，设成空（或只有空白）或 off 时是空。
// off 是给 Windows 用的：cmd 里 set REMOTE_RELAY_DIRECTORY= 是删掉变量，留不出空值。
// 设了但不是合法的 https 地址时提示并返回空：用户想换地址，悄悄改用内置地址不是他要的。提示里不写地址的内容（可能带着令牌）。
func directoryURLFromEnv(logf func(format string, args ...any)) string {
	v, ok := os.LookupEnv(directoryEnv)
	if !ok {
		return directory.DefaultURL
	}
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "off") {
		return ""
	}
	if err := directory.CheckURL(v); err != nil {
		logf("[relay] 环境变量 %s 的值不可用（%v），不从网络获取节点列表", directoryEnv, err)
		return ""
	}
	return v
}

// Current 读缓存并用内置根重新验签，返回可用的列表与缓存本身（本机专用地址在缓存里）。
// 没有列表、列表已失效、缓存读不了时列表为 nil，调用方退回内置默认节点；缓存读不了时返回空缓存。问题只在出现时提示一次。
func (m *dirManager) Current() (*directory.Verified, *directory.Cache) {
	if m == nil {
		return nil, nil
	}
	v, c, err := m.usable(m.now())
	switch {
	case err == nil:
		m.note("list", "", "")
		return v, c
	case errors.Is(err, directory.ErrExpired):
		m.note("list", directory.ErrorClass(err), "[relay] 缓存的节点列表已过期（%v），改用内置默认节点", err)
	default:
		m.note("list", directory.ErrorClass(err), "[relay] 缓存的节点列表已失效（%v），改用内置默认节点", err)
	}
	return nil, c
}

// usable 读缓存并按 now 重新验签，不提示列表本身的状态（缓存读不了照常提示）。
func (m *dirManager) usable(now time.Time) (*directory.Verified, *directory.Cache, error) {
	c := m.load()
	v, err := c.Current(m.roots, now)
	return v, c, err
}

// Position 返回缓存里已接受的最新列表位置（列表序号, 状态密钥授权序号），用来填 Register/Join 请求的
// directory_serial / directory_key_serial：relay 手里的列表比它新才会随响应下发。没有缓存、缓存读不了时是 (0, 0)。
// 用记下的位置而不是「现在能用的列表」：缓存的列表过期了，relay 那份同一位置的也一样过期，发来没用。
//
// 带不带 directory_serial 是能力声明（本程序认得节点状态、错误码与转发的列表），只看程序版本，与缓存能不能用无关：
// 没有管理器、缓存读不了时照样带 (0, 0)。代价只是 relay 每次都转发一份列表，Offer 收不下时会提示原因。
func (m *dirManager) Position() (serial, keySerial uint64) {
	if m == nil {
		return 0, 0
	}
	c := m.load()
	return c.State.MaxSerial, c.State.MaxStatusSerial
}

// Offer 收下 relay 随注册/加入响应转发来的列表（source 写进提示，如 "relay <地址>"）。raw 为空时什么都不做。
// 返回验过的列表（与缓存同一位置时也返回）；验不过、不比缓存新、缓存读写不了时返回 nil 并提示。
// 要持缓存写锁读写文件：通常几毫秒，别的进程占着锁时最多等 10 秒。
func (m *dirManager) Offer(raw []byte, source string) *directory.Verified {
	if m == nil || len(raw) == 0 {
		return nil
	}
	v, _ := m.offer(raw, source)
	return v
}

// offer 验签并收下 raw，提示结果。
func (m *dirManager) offer(raw []byte, source string) (*directory.Verified, error) {
	var (
		v      *directory.Verified
		before directory.State
	)
	err := directory.UpdateCache(m.path, func(c *directory.Cache) (bool, error) {
		before = c.State
		got, changed, err := c.Offer(raw, m.roots, m.now())
		if err != nil {
			return false, err
		}
		v = got
		return changed, nil
	})
	var verr *directory.VerifyError
	if err != nil && !errors.As(err, &verr) {
		m.cacheNote(err)
		return nil, err
	}
	// 成功了，或者走到了验签：缓存读得出来。
	m.cacheNote(nil)
	switch {
	case err == nil:
	case errors.Is(err, directory.ErrRollback):
		m.note("offer:"+source, directory.ErrorClass(err), "[relay] 来自 %s 的节点列表不比本机缓存的新，未采用（%v）", source, err)
		return nil, err
	default:
		m.note("offer:"+source, directory.ErrorClass(err), "[relay] 来自 %s 的节点列表验签失败，已丢弃（%v）", source, err)
		return nil, err
	}
	m.note("offer:"+source, "", "")
	if before.MaxStatusSerial != v.NewState.MaxStatusSerial || before.MaxSerial != v.NewState.MaxSerial {
		m.logUpdate(before, v, source)
	}
	return v, nil
}

// logUpdate 提示列表位置变了（spec 6.5 的「列表更新」一行），并逐个列出新出现（或原因变了）的被丢弃节点：
// 长期留在列表里的坏节点，Worker 每重签一次就提示一遍会刷屏。
func (m *dirManager) logUpdate(before directory.State, v *directory.Verified, source string) {
	active := 0
	for _, n := range v.Nodes {
		if n.Status == directory.StatusActive {
			active++
		}
	}
	switch {
	case before.MaxStatusSerial == 0 && before.MaxSerial == 0:
		m.logf("[relay] 已获取节点列表（来自 %s）: 序号 %d，启用 %d 个节点", source, v.Serial, active)
	case before.MaxStatusSerial != v.StatusKeySerial:
		m.logf("[relay] 节点列表已更新（来自 %s）: 状态密钥授权 %d → %d，序号 %d → %d，启用 %d 个节点",
			source, before.MaxStatusSerial, v.StatusKeySerial, before.MaxSerial, v.Serial, active)
	default:
		m.logf("[relay] 节点列表已更新（来自 %s）: 序号 %d → %d，启用 %d 个节点", source, before.MaxSerial, v.Serial, active)
	}
	seen := make(map[skipKey]int, len(v.Skipped))
	var fresh []directory.Skipped
	m.mu.Lock()
	for _, sk := range v.Skipped {
		key := skipKey{sk.Prefix, sk.Serial, directory.ErrorClass(sk.Err)}
		seen[key]++
		if seen[key] > m.skipped[key] {
			fresh = append(fresh, sk)
		}
	}
	m.skipped = seen
	m.mu.Unlock()
	// 前缀取自没验过签的证书正文，只用于提示，用 %q 转义。
	for _, sk := range fresh {
		m.logf("[relay] 节点列表里的节点 %q（证书序号 %d）已丢弃: %v", sk.Prefix, sk.Serial, sk.Err)
	}
}

// load 读缓存。读不了（损坏、更新版本写的、权限、等锁超时）时提示，返回空缓存（directory.LoadCache 出错时就是这样返回的）：
// 空缓存没有列表、位置是 (0, 0)，调用方自然退回内置默认节点。
func (m *dirManager) load() *directory.Cache {
	c, err := directory.LoadCache(m.path)
	m.cacheNote(err)
	return c
}

// cacheNote 提示缓存读写的问题；err 为 nil 表示没问题。错误里带着文件路径。
func (m *dirManager) cacheNote(err error) {
	switch {
	case err == nil:
		m.note("cache", "", "")
	case errors.Is(err, directory.ErrCacheNewer):
		// 不能让人删：删掉会丢掉新版本记住的位置和吊销。
		m.note("cache", "newer", "[relay] 节点列表缓存由更新版本的程序写入（%v），本程序不使用也不改动它，按内置默认节点连接。请升级到最新版本，不要删掉这个文件", err)
	case errors.Is(err, directory.ErrCacheCorrupt):
		m.note("cache", "corrupt", "[relay] 节点列表缓存已损坏（%v），本程序不使用也不改动它，按内置默认节点连接。删除这个文件即可重置", err)
	default:
		m.note("cache", "io", "[relay] 读写节点列表缓存失败（%v），按内置默认节点连接", err)
	}
}

// note 在 topic 的状态变成 state 时提示一次（format 为空时只记状态）：同一个问题反复遇到只说一遍，消失后再出现会再说。
func (m *dirManager) note(topic, state, format string, args ...any) {
	m.mu.Lock()
	if m.notes == nil {
		m.notes = make(map[string]string)
	}
	changed := m.notes[topic] != state
	m.notes[topic] = state
	m.mu.Unlock()
	if changed && format != "" {
		m.logf(format, args...)
	}
}

// Refresh 从 CF 取一次列表并收下，等到有结果或 ctx 结束。已经有请求在进行时不再发，等它的结果。
// 请求不跟随调用方的 ctx：放弃等待的调用方不该取消别人也在等的请求，上限由 HTTP 客户端的总超时保证，
// 取到的列表照样写进缓存。没有 CF 地址时返回 errDirectoryOff。
func (m *dirManager) Refresh(ctx context.Context) error {
	if m == nil || m.url == "" {
		return errDirectoryOff
	}
	now := m.now().Round(0)
	m.mu.Lock()
	f := m.inflight
	if f == nil {
		f = m.startLocked(now)
	}
	m.mu.Unlock()
	return f.wait(ctx)
}

// startLocked 发起一次 CF 请求。持 m.mu、且没有正在进行的请求时调用；now 是去掉了单调读数的当前时间。
func (m *dirManager) startLocked(now time.Time) *dirFetch {
	f := &dirFetch{done: make(chan struct{})}
	m.inflight = f
	m.attemptedAt = now
	go m.fetch(f)
	return f
}

// wait 等到请求有结果或 ctx 结束。
func (f *dirFetch) wait(ctx context.Context) error {
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fetch 执行一次 CF 请求，结果写进 f。时长上限是 m.http 的总超时（directoryFetchTimeout，含读完响应）。
func (m *dirManager) fetch(f *dirFetch) {
	src := m.source()
	raw, err := directory.Fetch(context.Background(), m.http, m.url)
	if err != nil {
		m.note("fetch", "failing", "[relay] 从 %s 获取节点列表失败（%v），先用缓存的列表或内置默认节点", src, err)
	} else {
		m.note("fetch", "", "")
		_, err = m.offer(raw, src)
	}
	now := m.now().Round(0)
	m.mu.Lock()
	if err == nil || errors.Is(err, directory.ErrRollback) {
		m.checkedAt = now
	}
	m.inflight = nil
	m.mu.Unlock()
	f.err = err
	close(f.done)
}

// source 是提示里 CF 的名字：地址里的主机名。
func (m *dirManager) source() string {
	if u, err := url.Parse(m.url); err == nil && u.Host != "" {
		return u.Host
	}
	return m.url
}

// RefreshIfStale 在缓存的列表不新鲜时从 CF 取一次（help 用：启动时在后台调，找不到前缀时同步调）。
// 列表的年龄取本进程上次从 CF 确认过的时间与列表签发时间中较晚的那个，不到 directoryRefreshEvery 就不取；
// 没有能用的列表（没有、过期了、缓存读不了）时谈不上年龄，要取。不管怎样，本进程上次请求不到 directoryRetryDelay 时不再发。
// 已经有请求在进行时等它的结果。没有 CF 地址、不需要取时返回 nil。
func (m *dirManager) RefreshIfStale(ctx context.Context) error {
	if m == nil || m.url == "" {
		return nil
	}
	now := m.now().Round(0)
	// 锁外读文件。只为判断，不提示列表的状态：不然列表刚过期时，后台刷新先说「已过期，改用内置默认节点」，紧接着又说「已更新」。
	// 读完到下面拿锁之间若有别的请求做完了，它已经记下了 attemptedAt，下面照样不会再发。
	v, _, _ := m.usable(now)
	// 判断要不要发与发出请求在同一把锁里：不然两个调用同时到来、先发的那个很快失败时，后一个会拿着失败之前读到的状态再发一个。
	m.mu.Lock()
	f := m.inflight
	if f == nil {
		fresh := v != nil && (within(now, m.checkedAt, directoryRefreshEvery) || within(now, v.IssuedAt, directoryRefreshEvery))
		if fresh || within(now, m.attemptedAt, directoryRetryDelay) {
			m.mu.Unlock()
			return nil
		}
		f = m.startLocked(now)
	}
	m.mu.Unlock()
	return f.wait(ctx)
}

// within 报告 t 早于 now 不到 d（比墙上时间）。t 在 now 之后（时钟往回调过、签发时间在本机时钟的未来）不算：宁可多取一次。
// t 是零值（从没发生过）也不算：与零值相差上千年，time.Time.Sub 饱和到最大值。
func within(now, t time.Time, d time.Duration) bool {
	age := now.Sub(t)
	return age >= 0 && age < d
}

// Run 立即从 CF 取一次，之后每 every 取一次，直到 ctx 结束（share 用）。结果都已提示过，失败不重试，等下一轮。
func (m *dirManager) Run(ctx context.Context) {
	if m == nil || m.url == "" {
		return
	}
	every := m.every
	if every <= 0 {
		every = directoryRefreshEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		_ = m.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
