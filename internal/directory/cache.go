package directory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Cache 是节点列表的本地缓存：客户端放在 ~/.remote_assist_relays.json，relay 放在 <state-dir>/directory.json。
//
// 文件内容不被信任：列表每次使用前都用内置根重新验签（Current）。State 直接沿用，
// 能改这个文件的人已经拥有当前用户权限（可以直接换掉程序），不在防御范围内。
//
// 新旧版本共用这个文件，格式演进的规则：
//   - 不认识的顶层字段写回时原样保留，所以新版本可以加顶层字段而不升 cacheVersion，
//     前提是旧版本原样带着它不出错（例如只在列表里还有对应节点时才使用的数据）。
//   - 需要随列表更新而改写的字段，以及 State 里的任何新字段（旧版本用 Verify 重建 State，会丢掉它们），
//     都必须升 cacheVersion。旧版本见到更高的版本号返回 ErrCacheNewer：不用、不覆盖。
//   - version 永远是文件里的第一个字段（MarshalJSON 保证）。更新的版本可以放宽大小上限，
//     旧版本读不全超大的文件，只能从开头认出版本号；认不出就当成损坏，会提示用户删除。
type Cache struct {
	Version   int             `json:"version"`
	Directory json.RawMessage `json:"directory,omitempty"` // 重新编码过的签名信封（见 Offer）
	State     State           `json:"state"`
	// LocalAddrs 是本机专用地址（前缀 → host:port），见 localaddr.go。属于「只在列表里还有对应节点时才使用的数据」，
	// 所以不升 cacheVersion；旧版本把它当不认识的顶层字段原样带着。
	LocalAddrs localAddrs `json:"local_addrs,omitempty"`

	extra map[string]json.RawMessage // 本版本不认识的顶层字段
}

// cacheFields 是 Cache 自己的顶层字段，与上面的 json 标签一致。
var cacheFields = []string{"version", "directory", "state", "local_addrs"}

// cacheJSON 与 Cache 字段相同、没有自定义编解码，用来复用标准的结构体编解码。
type cacheJSON Cache

func (c *Cache) UnmarshalJSON(data []byte) error {
	var known cacheJSON
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	// encoding/json 匹配字段名不区分大小写，这里也一样，免得同一个字段既被读进结构体、又被当成未知字段带回去。
	maps.DeleteFunc(all, func(k string, _ json.RawMessage) bool {
		return slices.ContainsFunc(cacheFields, func(f string) bool { return strings.EqualFold(k, f) })
	})
	*c = Cache(known)
	c.extra = nil
	if len(all) > 0 {
		c.extra = all
	}
	return nil
}

func (c Cache) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(cacheJSON(c))
	if err != nil || len(c.extra) == 0 {
		return data, err
	}
	// 不认识的字段接在已知字段后面，version 留在最前（见 leadingVersion）。extra 里没有已知字段
	// （UnmarshalJSON 已去掉），不会重复；值是读入时的原文，encoding/json 会校验并压缩这里的输出。
	var buf bytes.Buffer
	buf.Write(data[:len(data)-1]) // 去掉结尾的 '}'
	for _, k := range slices.Sorted(maps.Keys(c.extra)) {
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(c.extra[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

const (
	cacheVersion = 1
	maxCacheSize = 1 << 20
)

// ErrCacheCorrupt 表示缓存文件无法解析、过大或版本不认识。LoadCache 此时返回空缓存，调用方可以照常用内置节点运行；
// UpdateCache 则拒绝更新（见其说明），错误里带文件路径，要提示用户删掉它。
var ErrCacheCorrupt = errors.New("directory: cache file corrupt")

// ErrCacheNewer 表示缓存文件由更新的版本写入（版本号比本程序认识的高）。处理与 ErrCacheCorrupt 相同：
// LoadCache 返回空缓存，UpdateCache 拒绝更新、不覆盖。但不能提示用户删除——删掉会丢掉新版本记住的位置和吊销，
// 要提示升级。
var ErrCacheNewer = errors.New("directory: cache file written by a newer version")

// LoadCache 读缓存。文件不存在时返回空缓存和 nil；其它错误也总是返回一个可用的空缓存。
// 读的时候持共享锁：Windows 上有进程开着缓存文件时，写入方的 rename 会失败。
func LoadCache(path string) (*Cache, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return &Cache{}, nil
	}
	unlock, err := lockCache(path, false)
	if err != nil {
		return &Cache{}, err
	}
	defer unlock()
	return loadCache(path)
}

// loadCache 是不加锁的 LoadCache，调用方负责持锁。
func loadCache(path string) (*Cache, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Cache{}, nil
	}
	if err != nil {
		return &Cache{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCacheSize+1))
	if err != nil {
		return &Cache{}, err
	}
	newer := func(v int) error {
		return fmt.Errorf("%w: %s has version %d, this program understands up to %d", ErrCacheNewer, path, v, cacheVersion)
	}
	if len(data) > maxCacheSize {
		// 更新的版本可能放宽了大小上限：读不全，但开头认得出更高的版本号，就不是损坏，不能让人删掉它。
		if v := leadingVersion(data); v > cacheVersion {
			return &Cache{}, newer(v)
		}
		return &Cache{}, fmt.Errorf("%w: %s is larger than %d bytes", ErrCacheCorrupt, path, maxCacheSize)
	}
	// 先只看版本号：更新的版本可能改了其余字段的格式，按本版本的结构解析会失败，那不是损坏。
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return &Cache{}, fmt.Errorf("%w: %s: %v", ErrCacheCorrupt, path, err)
	}
	if head.Version > cacheVersion {
		return &Cache{}, newer(head.Version)
	}
	if head.Version != cacheVersion {
		return &Cache{}, fmt.Errorf("%w: %s has version %d, want %d", ErrCacheCorrupt, path, head.Version, cacheVersion)
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return &Cache{}, fmt.Errorf("%w: %s: %v", ErrCacheCorrupt, path, err)
	}
	return &c, nil
}

// leadingVersion 返回 data 第一个字段 version 的值，data 后半截被截断也读得出；第一个字段不是 version 时返回 0。
func leadingVersion(data []byte) int {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return 0
	}
	if t, err := dec.Token(); err != nil || t != "version" {
		return 0
	}
	var v int
	if err := dec.Decode(&v); err != nil {
		return 0
	}
	return v
}

// Current 用 roots 重新验签缓存里的列表。缓存里没有列表时返回 (nil, nil)。
func (c *Cache) Current(roots Roots, now time.Time) (*Verified, error) {
	if c == nil || len(c.Directory) == 0 {
		return nil, nil
	}
	return Verify(c.Directory, roots, c.State, now)
}

// Offer 校验 raw，比缓存里的新就替换并更新 State。返回验过的列表与缓存是否有变化。验签失败时缓存不变。
// 位置（状态密钥授权与列表序号）与缓存相同的列表不替换：Worker 每次签名都递增列表序号，
// 同一位置只应有一份内容。照样返回验过的结果。例外是缓存里那份已经验不过（文件被改坏）时，用这份修好它。
// 列表不换，State 照样更新：同位置的另一份列表（Worker 违约重签、relay 转来的另一份）可能带着缓存里还没有的吊销，
// 吊销只增不减，要记下来，不能只在这一次会话里生效。
//
// 存的是重新编码后的信封，不是 raw 原文：raw 里不参与签名的部分（重复的键、空白）不能带进缓存。
// 否则转发者塞进来的内容经 json.Marshal 的 HTML 转义（一个 '<' 变成 6 个字节）后，能把缓存撑过上限，
// 下次读取被当成损坏——已记住的位置和吊销就丢了。
func (c *Cache) Offer(raw []byte, roots Roots, now time.Time) (*Verified, bool, error) {
	v, err := Verify(raw, roots, c.State, now)
	if err != nil {
		return nil, false, err
	}
	// Verify 已保证位置不低于缓存里的（或缓存的位置因根被吊销而作废），所以位置不同就是更新。
	if len(c.Directory) > 0 && samePosition(v.NewState, c.State) {
		old := c.State
		c.State = v.NewState
		if _, err := c.Current(roots, now); err == nil {
			return v, !sameState(old, v.NewState), nil
		}
	}
	env, err := parseEnvelope(raw)
	if err != nil {
		return nil, false, err // Verify 刚解析过同一份 raw，不会走到这里
	}
	canon, err := json.Marshal(env)
	if err != nil {
		return nil, false, err
	}
	c.Version = cacheVersion
	c.Directory = canon
	c.State = v.NewState
	return v, true, nil
}

// sameState 报告两个状态写进缓存文件后是否一样（「要不要重写文件」只看这个；nil 与空切片都省略，视为相同）。
func sameState(a, b State) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

// UpdateCache 在跨进程锁内从磁盘读出最新内容交给 fn 修改，fn 返回 true 时原子写回（同目录临时文件 + rename，0600）。
// 加锁并每次读最新的磁盘内容，是为了让同一台机器上的多个进程（share、help、MCP）互不覆盖对方刚写入的更新。
// fn 在持锁期间运行，里面不要做网络请求。
//
// 文件损坏（ErrCacheCorrupt）、由更新的版本写入（ErrCacheNewer）、读不了（权限、被占用）或等锁超时都直接返回错误，
// 不调用 fn、不覆盖文件。损坏时不能自动从空缓存重来：那等于忘掉已记住的位置与吊销，之后旧列表就能回放。
// 错误里带着路径，损坏的由人删掉文件重置；更新版本写的则提示升级，不能让人删。
func UpdateCache(path string, fn func(*Cache) (bool, error)) error {
	unlock, err := lockCache(path, true)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := loadCache(path)
	if err != nil {
		return err
	}
	changed, err := fn(c)
	if err != nil || !changed {
		return err
	}
	c.Version = cacheVersion
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic 先写同目录的临时文件并 fsync，再 rename 覆盖目标，避免崩溃或断电留下半截文件。
// os.CreateTemp 在类 Unix 上以 0600 创建，rename 保留该权限。
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := renameWithRetry(tmpPath, path); err != nil {
		return err
	}
	done = true
	return nil
}

// 文件被短暂占用时 rename 的重试次数与间隔，最多等约一秒。杀毒软件扫一个小文件通常只要几十毫秒。
// 这期间持着缓存写锁，读者跟着等，远在 lockTimeout 之内。
const (
	renameAttempts = 20
	renameDelay    = 50 * time.Millisecond
)

// renameWithRetry 是 os.Rename，失败而 renameBusy 认为只是被短暂占用时（只有 Windows 会这样）等一会儿再试。
func renameWithRetry(from, to string) error {
	for i := 1; ; i++ {
		err := os.Rename(from, to)
		if err == nil || i == renameAttempts || !renameBusy(err) {
			return err
		}
		time.Sleep(renameDelay)
	}
}
