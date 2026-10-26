// Package config 负责 relay 统一模块配置文件的解析、校验、版本管理、监听与热加载编排。
//
// 统一配置文件是新增的可选能力（--module-config），只描述模块；relay 核心配置仍由命令行
// 参数、--limits-file 与 --update-info-file 决定，与之前完全一致。仓库没有 YAML 依赖，
// 且现有配置文件均为 JSON，因此统一配置同样使用 JSON：
//
//	{
//	  "version": 1,
//	  "modules": {
//	    "audit": {"enabled": true, "output": "file"}
//	  }
//	}
//
// 只产生不可变的 Snapshot，不直接修改模块内部状态；模块启停由 module.Manager 完成。
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/remote-assist/tool/pkg/relay/module"
)

// MaxFileBytes 统一配置文件大小上限。
const MaxFileBytes = 1 << 20

// SupportedVersions 本版本 relay 能读取的配置格式版本。旧版本在这里登记迁移函数后即可兼容读取。
var SupportedVersions = []int{module.CurrentConfigVersion}

var moduleKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ModuleSection 是 modules.<id> 的解析结果。
type ModuleSection struct {
	Enabled bool
	// Require 可选框架字段 "require"：对该模块构建来源与最低版本的期望，用于发现配置与
	// 二进制不匹配（例如为私有模块编写的配置被纯核心或另一来源的构建加载）。
	Require module.Requirement
	// Config 去掉框架字段 enabled/require 后的模块配置（规范化 JSON）。
	Config module.Config
}

// frameworkKeys 是 modules.<id> 中由框架解释、不会传给模块的保留键。
var frameworkKeys = []string{"enabled", "require"}

// Snapshot 是一份通过格式校验的配置快照，创建后不可变（只通过方法读取）。
type Snapshot struct {
	revision      uint64
	digest        string
	schemaVersion int
	source        string
	loadedAt      time.Time
	modules       map[string]ModuleSection
}

func (s *Snapshot) Revision() uint64    { return s.revision }
func (s *Snapshot) Digest() string      { return s.digest }
func (s *Snapshot) SchemaVersion() int  { return s.schemaVersion }
func (s *Snapshot) Source() string      { return s.source }
func (s *Snapshot) LoadedAt() time.Time { return s.loadedAt }

// ModuleIDs 返回配置中出现的所有模块 ID（含 enabled=false 的，升序）。
func (s *Snapshot) ModuleIDs() []string {
	ids := make([]string, 0, len(s.modules))
	for id := range s.modules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Module 返回某个模块段的副本。
func (s *Snapshot) Module(id string) (ModuleSection, bool) {
	m, ok := s.modules[id]
	return m, ok
}

// Desired 把快照转换为模块管理器的期望状态：Modules 只包含 enabled 的模块，
// enabled=false 的模块列入 Disabled（仍需是已注册模块）。
func (s *Snapshot) Desired() module.Desired {
	d := module.Desired{Revision: s.revision, Digest: s.digest, SchemaVersion: s.schemaVersion, Modules: map[string]module.Config{}}
	for _, id := range s.ModuleIDs() {
		m := s.modules[id]
		if m.Require != (module.Requirement{}) {
			if d.Requirements == nil {
				d.Requirements = map[string]module.Requirement{}
			}
			d.Requirements[id] = m.Require
		}
		if m.Enabled {
			d.Modules[id] = m.Config
		} else {
			d.Disabled = append(d.Disabled, id)
		}
	}
	return d
}

// withRevision 复制快照并赋予修订号（Store 在接受快照时调用）。
func (s *Snapshot) withRevision(rev uint64, at time.Time) *Snapshot {
	c := *s
	c.revision, c.loadedAt = rev, at
	return &c
}

// Digest 返回配置内容摘要（原始字节的 SHA-256）。
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// rootKeys 是根对象允许的键（大小写敏感：encoding/json 解码 struct 时大小写不敏感，
// 会把 "Modules" 与 "modules" 合并，因此根对象先解成 map 再逐键精确匹配）。
var rootKeys = map[string]bool{"version": true, "relay": true, "modules": true}

// Parse 严格解析统一配置：拒绝未知根字段、重复键、尾随内容、不支持的版本、非法模块 ID
// 和非对象的模块段。返回的快照尚无修订号。是否存在对应的内置模块由模块管理器校验。
func Parse(data []byte, source string) (*Snapshot, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("config exceeds %d bytes", MaxFileBytes)
	}
	if err := checkDuplicateKeys(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var root map[string]json.RawMessage
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if root == nil {
		return nil, errors.New("parse config: the configuration must be a JSON object")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse config: unexpected data after the JSON object")
	}
	for k := range root {
		if !rootKeys[k] {
			return nil, fmt.Errorf("config: unknown top-level key %q (allowed: version, relay, modules)", k)
		}
	}
	rawVersion, ok := root["version"]
	if !ok {
		return nil, errors.New(`config: missing "version" (current version is 1)`)
	}
	var version int
	if err := json.Unmarshal(rawVersion, &version); err != nil || bytes.Equal(bytes.TrimSpace(rawVersion), []byte("null")) {
		return nil, errors.New(`config: "version" must be an integer`)
	}
	if !versionSupported(version) {
		return nil, fmt.Errorf("config: version %d is not supported by this relay (supported: %v)", version, SupportedVersions)
	}
	if err := checkRelaySection(root["relay"]); err != nil {
		return nil, err
	}
	var modules map[string]json.RawMessage
	if raw, ok := root["modules"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &modules); err != nil || modules == nil {
			return nil, errors.New(`config: "modules" must be an object`)
		}
	}
	snap := &Snapshot{
		digest:        Digest(data),
		schemaVersion: version,
		source:        source,
		modules:       make(map[string]ModuleSection, len(modules)),
	}
	for id, raw := range modules {
		if !moduleKeyPattern.MatchString(id) {
			return nil, fmt.Errorf("config: module id %q must match %s", id, moduleKeyPattern.String())
		}
		sec, err := parseModuleSection(id, version, raw)
		if err != nil {
			return nil, err
		}
		snap.modules[id] = sec
	}
	return snap, nil
}

// checkRelaySection 第一阶段 relay 核心配置仍来自命令行参数，relay 段只作为预留：
// 允许省略或为空对象，有内容时拒绝，避免运维误以为在这里改核心参数会生效。
func checkRelaySection(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return errors.New(`config: "relay" must be an object`)
	}
	if len(obj) > 0 {
		return errors.New(`config: "relay" section is reserved; core relay settings are still configured by command-line flags in this version`)
	}
	return nil
}

func parseModuleSection(id string, version int, raw json.RawMessage) (ModuleSection, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ModuleSection{}, fmt.Errorf("config: module %s must be a JSON object", id)
	}
	sec := ModuleSection{Enabled: true}
	for k := range fields {
		for _, fk := range frameworkKeys {
			if k != fk && strings.EqualFold(k, fk) {
				return ModuleSection{}, fmt.Errorf("config: module %s: use lowercase %q, not %q", id, fk, k)
			}
		}
	}
	if v, ok := fields["require"]; ok {
		req, err := parseRequire(v)
		if err != nil {
			return ModuleSection{}, fmt.Errorf("config: module %s: \"require\": %w", id, err)
		}
		sec.Require = req
		delete(fields, "require")
	}
	if v, ok := fields["enabled"]; ok {
		if err := json.Unmarshal(v, &sec.Enabled); err != nil || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return ModuleSection{}, fmt.Errorf("config: module %s: \"enabled\" must be true or false", id)
		}
		delete(fields, "enabled")
	}
	// 规范化：键排序（json.Marshal 对 map 排序）+ 每个值去空白，使同义配置得到相同字节。
	for k, v := range fields {
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			return ModuleSection{}, fmt.Errorf("config: module %s: field %s: %w", id, k, err)
		}
		fields[k] = buf.Bytes()
	}
	canonical, err := json.Marshal(fields)
	if err != nil {
		return ModuleSection{}, fmt.Errorf("config: module %s: %w", id, err)
	}
	sec.Config = module.NewConfig(id, version, canonical)
	return sec, nil
}

// parseRequire 解析 {"source": "...", "min_version": "..."}，两项都可省略，未知键拒绝。
func parseRequire(raw json.RawMessage) (module.Requirement, error) {
	var r struct {
		Source     string `json:"source"`
		MinVersion string `json:"min_version"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || dec.Decode(&r) != nil {
		return module.Requirement{}, errors.New(`must be an object with optional "source" and "min_version" strings`)
	}
	req := module.Requirement{Source: r.Source, MinVersion: r.MinVersion}
	if err := module.ValidateRequirement(req); err != nil {
		return module.Requirement{}, err
	}
	return req, nil
}

func versionSupported(v int) bool {
	for _, s := range SupportedVersions {
		if s == v {
			return true
		}
	}
	return false
}

// checkDuplicateKeys 拒绝任意层级对象中的重复键（大小写不敏感）：encoding/json 默认静默取
// 最后一个，且解码到 struct 时按大小写不敏感匹配字段，"value" 与 "Value" 会互相覆盖，
// 让写错的配置看起来“生效了另一半”。
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := keyTok.(string)
				folded := strings.ToLower(key)
				if seen[folded] {
					return fmt.Errorf("config: duplicate key %q in %s (keys are compared case-insensitively)", key, displayPath(path))
				}
				seen[folded] = true
				if err := walk(path + "." + key); err != nil {
					return err
				}
			}
		case '[':
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // 结束符
		return err
	}
	if err := walk(""); err != nil {
		if strings.HasPrefix(err.Error(), "config:") {
			return err
		}
		return fmt.Errorf("parse config: %w", err)
	}
	return nil
}

func displayPath(p string) string {
	if p == "" {
		return "the root object"
	}
	return strings.TrimPrefix(p, ".")
}
