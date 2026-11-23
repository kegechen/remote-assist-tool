package relay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/remote-assist/tool/internal/directory"
)

// 节点模式（--directory-url）：relay 定时拉取签名的节点列表，用自己 TLS 证书的指纹在列表里认出自己，
// 然后按节点前缀发码、按节点状态（active / draining / disabled）接受注册，并把列表转发给声明了能力的客户端。
//
// 列表的真实性全靠根签名（internal/directory），拉取地址本身不被信任：CF 被盗只能让列表停在旧版本或过期，
// 改不了节点集合。relay 只信任验签通过、位置不回退、未过期的列表；验不过就继续用手里的旧副本，直到它过期。

const (
	// DefaultDirectoryRefresh 是 --directory-refresh 的默认值。
	DefaultDirectoryRefresh = 5 * time.Minute
	// MinDirectoryRefresh 是 --directory-refresh 的下限，防止把 CF 打成被限流。
	MinDirectoryRefresh = time.Minute

	directoryFile         = "directory.json"
	directoryFetchTimeout = 30 * time.Second
	directoryMaxRedirects = 5
	// directoryRecheck 是重新验签手里那份列表的间隔：列表、状态密钥授权、本节点证书都会过期，
	// 过期要在使用时生效，不能等下一次拉取成功（拉取可能一直失败）。验一次只要几十次 ed25519 验签。
	directoryRecheck = time.Minute
)

// ValidateNodeMode 校验节点模式（cfg.DirectoryURL 非空时）的参数及其与其它配置的组合。不读任何文件，
// 解析命令行、校验 Windows 服务配置时就能调用；证书文件与根公钥由 NewServer 再查。cfg.DirectoryRefresh 为 0 表示用默认值。
func ValidateNodeMode(cfg *Config) error {
	if cfg.DirectoryURL == "" {
		return nil
	}
	u, err := url.Parse(cfg.DirectoryURL)
	if err != nil {
		return fmt.Errorf("directory URL %q: %w", cfg.DirectoryURL, err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("directory URL %q must be an https URL with a host and no credentials", cfg.DirectoryURL)
	}
	if cfg.DirectoryRefresh != 0 && cfg.DirectoryRefresh < MinDirectoryRefresh {
		return fmt.Errorf("directory refresh %s is shorter than the minimum %s", cfg.DirectoryRefresh, MinDirectoryRefresh)
	}
	switch {
	case cfg.NoAuth:
		return errors.New("node mode (directory URL) cannot be combined with no-auth")
	case cfg.StateDir == "":
		return errors.New("node mode (directory URL) requires a state dir to keep the node directory")
	case !cfg.UseTLS:
		return errors.New("node mode (directory URL) requires TLS: the node is identified by its certificate fingerprint")
	case cfg.CodeLength != defaultCodeLength:
		return fmt.Errorf("node mode (directory URL) requires the network-wide code length %d, got %d", defaultCodeLength, cfg.CodeLength)
	}
	return nil
}

// directoryPosition 是一份列表的新旧位置：（状态密钥授权序号, 列表序号），按字典序比较，与 directory.State 一致。
type directoryPosition struct {
	keySerial uint64
	serial    uint64
}

func (p directoryPosition) newerThan(o directoryPosition) bool {
	if p.keySerial != o.keySerial {
		return p.keySerial > o.keySerial
	}
	return p.serial > o.serial
}

// clientDirectoryPosition 从请求字段取出客户端缓存的列表位置；没声明 directory_serial（旧客户端）时返回 nil。
func clientDirectoryPosition(serial *uint64, keySerial uint64) *directoryPosition {
	if serial == nil {
		return nil
	}
	return &directoryPosition{keySerial: keySerial, serial: *serial}
}

// nodeView 是某一时刻可用的列表与本节点。零值表示没有可用的列表（未开节点模式、还没拉到、已过期）。
type nodeView struct {
	raw json.RawMessage   // 要转发的签名信封（缓存里重新编码过的那份）
	pos directoryPosition // raw 的位置
	// self 是列表里本机证书指纹对应的节点；nil 表示不在列表里（或没有列表），此时不进入节点模式：
	// 发全随机码、不执行任何节点状态，但有效的列表照样转发。
	self *directory.Node
}

// forwardTo 返回应附在响应里的列表：只给声明了能力、且手里的列表比 relay 旧的客户端。
func (v nodeView) forwardTo(client *directoryPosition) json.RawMessage {
	if client == nil || v.raw == nil || !v.pos.newerThan(*client) {
		return nil
	}
	return v.raw
}

// prefix 是本节点的码前缀；不在节点模式时为空。
func (v nodeView) prefix() string {
	if v.self == nil {
		return ""
	}
	return v.self.Prefix
}

// nodeDirectory 持有节点模式的配置与当前列表。
type nodeDirectory struct {
	path    string
	url     string
	refresh time.Duration
	roots   directory.Roots
	now     func() time.Time
	client  *http.Client
	selfFP  string

	fetchMu sync.Mutex // 串行化「拉取 → 写回」，两次刷新不会交错

	mu        sync.Mutex
	cache     *directory.Cache // 最近一次读到或写回的缓存；只整体替换，不原地修改
	view      nodeView
	checkedAt time.Time // view 的计算时间
	stale     bool      // 缓存换过，view 需要重算
	logged    string    // 上次写日志时的状态摘要，状态变了才再写
}

// newNodeDirectory 校验节点模式的配置、读出本机证书指纹与 <StateDir>/directory.json。
// 调用前 StateDir 必须已经存在（newReuseSigner 会建）。
func newNodeDirectory(cfg *Config) (*nodeDirectory, error) {
	if err := ValidateNodeMode(cfg); err != nil {
		return nil, err
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return nil, errors.New("node mode (directory URL) requires a TLS certificate file: the node is identified by its certificate fingerprint")
	}
	if cfg.DirectoryRefresh == 0 {
		cfg.DirectoryRefresh = DefaultDirectoryRefresh
	}
	roots := directory.BuiltinRoots()
	if cfg.directoryRoots != nil {
		roots = *cfg.directoryRoots
	}
	if !roots.Usable() {
		return nil, errors.New("node mode (directory URL) is unavailable: this build has no directory root keys")
	}
	pair, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("node mode: load TLS certificate: %w", err)
	}
	d := &nodeDirectory{
		path:    filepath.Join(cfg.StateDir, directoryFile),
		url:     cfg.DirectoryURL,
		refresh: cfg.DirectoryRefresh,
		roots:   roots,
		now:     cfg.directoryNow,
		client:  cfg.directoryClient,
		selfFP:  directory.CertFingerprint(pair.Certificate[0]),
		stale:   true,
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.client == nil {
		d.client = defaultDirectoryClient()
	}
	// 缓存坏了不能带病启动：之后的每次写回都会被拒（见 directory.UpdateCache），节点模式永远起不来。
	// 错误里带着路径；损坏的删掉即可重置，更新版本写的要升级 relay。
	d.cache, err = directory.LoadCache(d.path)
	if err != nil {
		switch {
		case errors.Is(err, directory.ErrCacheNewer):
			return nil, fmt.Errorf("node directory cache: %w (upgrade the relay; do not delete the file)", err)
		case errors.Is(err, directory.ErrCacheCorrupt):
			return nil, fmt.Errorf("node directory cache: %w (delete the file to reset it)", err)
		default:
			return nil, fmt.Errorf("node directory cache %s: %w", d.path, err)
		}
	}
	log.Printf("Node mode: certificate fingerprint %s, directory %s every %s", d.selfFP, d.url, d.refresh)
	d.current()
	return d, nil
}

func defaultDirectoryClient() *http.Client {
	return &http.Client{
		Timeout: directoryFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to non-https URL %s", req.URL.Redacted())
			}
			if len(via) >= directoryMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", directoryMaxRedirects)
			}
			return nil
		},
	}
}

// current 返回当前可用的列表与本节点。每隔 directoryRecheck（或时钟回拨、缓存换过）重新验签一次，
// 过期、吊销在使用时生效。
func (d *nodeDirectory) current() nodeView {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.stale && !now.Before(d.checkedAt) && now.Sub(d.checkedAt) < directoryRecheck {
		return d.view
	}
	v, err := d.cache.Current(d.roots, now)
	var view nodeView
	var summary string
	switch {
	case err != nil:
		summary = "Node mode: the node directory is no longer valid (" + errorClass(err) + "), issuing random codes and enforcing no node status until a valid one is fetched"
	case v == nil:
		summary = "Node mode: no node directory yet, issuing random codes until one is fetched"
	default:
		view = nodeView{raw: d.cache.Directory, pos: directoryPosition{keySerial: v.StatusKeySerial, serial: v.Serial}, self: v.NodeByFingerprint(d.selfFP)}
		if view.self == nil {
			summary = fmt.Sprintf("Node mode: directory (%d, %d) valid until %s does not list this relay's certificate %s, issuing random codes",
				v.StatusKeySerial, v.Serial, v.ExpiresAt.UTC().Format(time.RFC3339), d.selfFP)
		} else {
			summary = fmt.Sprintf("Node mode: directory (%d, %d) valid until %s, this relay is node %s %q, status %s",
				v.StatusKeySerial, v.Serial, v.ExpiresAt.UTC().Format(time.RFC3339), view.self.Prefix, view.self.Name, view.self.Status)
		}
	}
	// 只在状态变化时写日志：每分钟都会重算一次，原样重复的日志没有用。错误原文里带着时间，所以按类别比较。
	if summary != d.logged {
		if err != nil {
			log.Printf("Node mode: directory %s: %v", d.path, err)
		}
		if v != nil {
			for _, sk := range v.Skipped {
				log.Printf("Node mode: directory entry %q (serial %d) skipped: %v", sk.Prefix, sk.Serial, sk.Err)
			}
		}
		log.Print(summary)
		d.logged = summary
	}
	d.view, d.checkedAt, d.stale = view, now, false
	return view
}

// errorClass 把验证错误归成稳定的类别，用于判断状态有没有变化（错误原文里带着时间，每次都不同）。
func errorClass(err error) string {
	for _, e := range []error{directory.ErrExpired, directory.ErrNotYetValid, directory.ErrRevoked, directory.ErrRollback, directory.ErrBadSignature, directory.ErrMalformed, directory.ErrNoRoots} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return "invalid"
}

// fetch 拉取一份列表原文，大小不超过 directory.MaxRawSize。
func (d *nodeDirectory) fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, directoryFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", d.url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, directory.MaxRawSize+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", d.url, err)
	}
	if len(data) > directory.MaxRawSize {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", d.url, directory.MaxRawSize)
	}
	return data, nil
}

// refreshDirectory 拉取一次列表：验签、位置不回退、未过期才原子写回 <StateDir>/directory.json 并生效。
// 任何一步失败都返回错误，手里的列表不变（直到它过期）。未开节点模式时什么都不做。
func (s *Server) refreshDirectory(ctx context.Context) error {
	d := s.directory
	if d == nil {
		return nil
	}
	d.fetchMu.Lock()
	defer d.fetchMu.Unlock()
	raw, err := d.fetch(ctx)
	if err != nil {
		return err
	}
	var updated *directory.Cache
	err = directory.UpdateCache(d.path, func(c *directory.Cache) (bool, error) {
		_, changed, err := c.Offer(raw, d.roots, d.now())
		if err != nil {
			return false, err
		}
		updated = c
		return changed, nil
	})
	if err != nil {
		return fmt.Errorf("node directory from %s rejected: %w", d.url, err)
	}
	d.mu.Lock()
	d.cache, d.stale = updated, true
	d.mu.Unlock()
	d.current()
	return nil
}

// directoryLoop 启动后立即拉取一次，之后每 refresh 拉一次。失败只记日志，继续用手里的列表。
func (s *Server) directoryLoop(ctx context.Context) {
	if s.directory == nil {
		return
	}
	ticker := time.NewTicker(s.directory.refresh)
	defer ticker.Stop()
	for {
		if err := s.refreshDirectory(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Node mode: directory refresh failed, keeping the current copy until it expires: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// nodeView 返回当前可用的列表与本节点；未开节点模式时返回零值。
func (s *Server) nodeView() nodeView {
	if s.directory == nil {
		return nodeView{}
	}
	return s.directory.current()
}
