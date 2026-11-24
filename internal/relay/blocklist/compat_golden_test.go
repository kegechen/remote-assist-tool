package blocklist

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// 兼容性金样：线上 relay（a45533f）的 blocklist.json 解码结构，原样冻结在这里，不随新代码改动。
//
// 旧版 loadFile 开了 DisallowUnknownFields：新代码写出的 blocklist.json 只要多一个字段，回滚到旧 relay
// 时整份名单加载失败、进入纯内存模式（手动永久封禁全部失效）。所以新数据（封禁记录、设备名单）一律
// 放进新文件，blocklist.json 一个字段都不能加。改动下列结构 = 改动线上格式，必须先改设计。
type a45533fFileData struct {
	Schema  int              `json:"schema"`
	Salt    string           `json:"salt"`
	SavedAt int64            `json:"saved_at"`
	Entries []*a45533fEntry  `json:"entries"`
	Memory  []*a45533fMemRec `json:"memory"`
}

type a45533fBanRec struct {
	Since     int64  `json:"since"`
	Until     int64  `json:"until"`
	Permanent bool   `json:"permanent"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor"`
	Note      string `json:"note"`
	Kind      string `json:"kind,omitempty"`
}

type a45533fAllowRec struct {
	Since     int64  `json:"since"`
	Until     int64  `json:"until"`
	Permanent bool   `json:"permanent"`
	Actor     string `json:"actor"`
	Note      string `json:"note"`
}

type a45533fClientRec struct {
	Role      string `json:"role"`
	Version   string `json:"version"`
	Host      string `json:"host"`
	ClientFP  string `json:"client_fp"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	Count     int64  `json:"count"`
	Verified  bool   `json:"verified,omitempty"`
}

type a45533fEventRec struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Points int    `json:"points"`
}

type a45533fHistoryRec struct {
	TS     int64  `json:"ts"`
	Action string `json:"action"`
	Actor  string `json:"actor"`
	Note   string `json:"note"`
	Detail string `json:"detail"`
}

type a45533fEntry struct {
	IP          string              `json:"ip"`
	FirstSeen   int64               `json:"first_seen"`
	LastSeen    int64               `json:"last_seen"`
	Connections int64               `json:"connections"`
	Rejected    int64               `json:"rejected"`
	Clients     []a45533fClientRec  `json:"clients"`
	Events      []a45533fEventRec   `json:"events"`
	History     []a45533fHistoryRec `json:"history"`
	Ban         *a45533fBanRec      `json:"ban,omitempty"`
	Allow       *a45533fAllowRec    `json:"allow,omitempty"`
	GraceUntil  int64               `json:"grace_until"`
	BanCount    int                 `json:"ban_count"`
}

type a45533fMemRec struct {
	Key        string `json:"key"`
	Offenses   int    `json:"offenses"`
	LastBanEnd int64  `json:"last_ban_end"`
	LastGood   int64  `json:"last_good"`
}

// 新代码（开着封禁记录）走遍各类状态后写出的 blocklist.json，必须能被线上版本的结构严格解码。
func TestBlocklistFileGoldenCompatA45533f(t *testing.T) {
	env := newRecEnv(t)
	env.open()
	l := env.l
	l.ObserveConnect("198.51.100.2")
	l.ObserveClient("198.51.100.2", ClientInfo{Role: "help", Version: "v1.2.3", Host: "u@h windows amd64", ClientID: "c1"})
	l.ObserveClient("198.51.100.2", ClientInfo{Role: "share", Version: "v1.2.3", Host: "u@h2 linux amd64", ClientID: "c2"})
	autoBan(t, l, "198.51.100.2")
	autoBan(t, l, "2001:db8:5::1")
	hit(l, "198.51.100.3", KindProtoDecode, 3)
	if _, err := l.Ban("198.51.100.4", time.Hour, false, "alice", "手动"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ban("198.51.100.5", 0, true, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Allow("198.51.100.6", time.Hour, false, "bob", "白名单"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ban("198.51.100.7", time.Hour, false, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Unban("198.51.100.7", "bob", "解禁", &AllowSpec{Duration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if len(env.r.Snapshot()) == 0 {
		t.Fatal("封禁记录应在工作（否则这个金样没覆盖到记录开启时的写盘路径）")
	}
	env.close()

	raw, err := os.ReadFile(env.listPath)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fd a45533fFileData
	if err := dec.Decode(&fd); err != nil {
		t.Fatalf("线上版本（a45533f）解码新写出的 blocklist.json 失败——回滚后整份名单会进入纯内存模式: %v\n%s", err, raw)
	}
	if fd.Schema != 2 || len(fd.Entries) < 6 || len(fd.Memory) == 0 {
		t.Fatalf("金样没覆盖到足够的状态: schema=%d entries=%d memory=%d", fd.Schema, len(fd.Entries), len(fd.Memory))
	}
	var bans, allows, clients, events, history int
	for _, e := range fd.Entries {
		if e.Ban != nil {
			bans++
		}
		if e.Allow != nil {
			allows++
		}
		clients += len(e.Clients)
		events += len(e.Events)
		history += len(e.History)
	}
	if bans < 4 || allows < 2 || clients == 0 || events == 0 || history == 0 {
		t.Fatalf("金样没覆盖到封禁/白名单/客户端/事件/历史: %d %d %d %d %d", bans, allows, clients, events, history)
	}
	if strings.Contains(string(raw), `"records"`) || strings.Contains(string(raw), `"rec_id"`) {
		t.Fatal("封禁记录不得写进 blocklist.json")
	}
}
