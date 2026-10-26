package config

import (
	"strings"
	"testing"
)

func TestParseValidConfig(t *testing.T) {
	data := []byte(`{
  "version": 1,
  "relay": {},
  "modules": {
    "audit":  {"enabled": true, "output": "file", "nested": {"b": 1,  "a": [1, 2]}},
    "notice": {"interval": "6h"},
    "off":    {"enabled": false}
  }
}`)
	snap, err := Parse(data, "/etc/relay/modules.json")
	if err != nil {
		t.Fatal(err)
	}
	if snap.SchemaVersion() != 1 || snap.Source() != "/etc/relay/modules.json" || snap.Digest() != Digest(data) || snap.Revision() != 0 {
		t.Fatalf("snapshot meta: %+v", snap)
	}
	if ids := snap.ModuleIDs(); strings.Join(ids, ",") != "audit,notice,off" {
		t.Fatalf("ids = %v", ids)
	}
	audit, _ := snap.Module("audit")
	if !audit.Enabled || string(audit.Config.Bytes()) != `{"nested":{"b":1,"a":[1,2]},"output":"file"}` {
		t.Fatalf("audit = %v %s", audit.Enabled, audit.Config.Bytes())
	}
	if notice, _ := snap.Module("notice"); !notice.Enabled {
		t.Fatal("enabled must default to true when the section is present")
	}
	d := snap.Desired()
	if len(d.Modules) != 2 || d.SchemaVersion != 1 {
		t.Fatalf("desired = %+v", d)
	}
	if _, on := d.Modules["off"]; on {
		t.Fatal("disabled module included in desired state")
	}
}

func TestParseCanonicalizesEquivalentConfigs(t *testing.T) {
	a, err := Parse([]byte(`{"version":1,"modules":{"audit":{"x":1,"y":{"k": "v"}}}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte("{\n \"version\": 1,\n \"modules\": {\"audit\": {\"y\": {\"k\":\"v\"},\n \"enabled\": true, \"x\": 1}}}"), "")
	if err != nil {
		t.Fatal(err)
	}
	ma, _ := a.Module("audit")
	mb, _ := b.Module("audit")
	if !ma.Config.Equal(mb.Config) {
		t.Fatalf("equivalent configs differ: %s vs %s", ma.Config.Bytes(), mb.Config.Bytes())
	}
	if a.Digest() == b.Digest() {
		t.Fatal("digest must reflect raw file bytes")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"not json":          `{`,
		"missing version":   `{"modules":{}}`,
		"future version":    `{"version":2}`,
		"zero version":      `{"version":0}`,
		"string version":    `{"version":"1"}`,
		"unknown root":      `{"version":1,"moduls":{}}`,
		"trailing data":     `{"version":1} {}`,
		"relay content":     `{"version":1,"relay":{"listen":":8443"}}`,
		"relay not object":  `{"version":1,"relay":[]}`,
		"bad module id":     `{"version":1,"modules":{"Audit":{}}}`,
		"dash module id":    `{"version":1,"modules":{"up-notice":{}}}`,
		"module not object": `{"version":1,"modules":{"audit":true}}`,
		"module null":       `{"version":1,"modules":{"audit":null}}`,
		"enabled not bool":  `{"version":1,"modules":{"audit":{"enabled":"yes"}}}`,
		"enabled null":      `{"version":1,"modules":{"audit":{"enabled":null}}}`,
		"duplicate root":    `{"version":1,"version":1}`,
		"duplicate module":  `{"version":1,"modules":{"audit":{},"audit":{}}}`,
		"duplicate nested":  `{"version":1,"modules":{"audit":{"a":{"x":1,"x":2}}}}`,
		"duplicate in list": `{"version":1,"modules":{"audit":{"a":[{"x":1,"x":2}]}}}`,
		"case root modules": `{"version":1,"modules":{"audit":{}},"Modules":{"notice":{}}}`,
		"case relay":        `{"version":1,"relay":{"x":1},"RELAY":{}}`,
		"upper root key":    `{"VERSION":1}`,
		"case module field": `{"version":1,"modules":{"audit":{"value":"a","Value":"b"}}}`,
		"modules not obj":   `{"version":1,"modules":[]}`,
		"root not object":   `[1]`,
		"float version":     `{"version":1.0}`,
		"case enabled":      `{"version":1,"modules":{"audit":{"Enabled":false}}}`,
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data), ""); err == nil {
			t.Errorf("%s: accepted %s", name, data)
		}
	}
	big := `{"version":1,"modules":{"audit":{"pad":"` + strings.Repeat("x", MaxFileBytes) + `"}}}`
	if _, err := Parse([]byte(big), ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized config: %v", err)
	}
}

func TestParseDuplicateKeyErrorNamesPath(t *testing.T) {
	_, err := Parse([]byte(`{"version":1,"modules":{"audit":{"a":{"x":1,"x":2}}}}`), "")
	if err == nil || !strings.Contains(err.Error(), `duplicate key "x" in modules.audit.a`) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseEmptyModules(t *testing.T) {
	snap, err := Parse([]byte(`{"version":1}`), "")
	if err != nil || len(snap.ModuleIDs()) != 0 || len(snap.Desired().Modules) != 0 {
		t.Fatalf("%v %v", snap, err)
	}
}

func TestParseNullModulesIsEmpty(t *testing.T) {
	snap, err := Parse([]byte(`{"version":1,"relay":null,"modules":null}`), "")
	if err != nil || len(snap.ModuleIDs()) != 0 {
		t.Fatalf("%v %v", snap, err)
	}
}

func TestParseRequire(t *testing.T) {
	snap, err := Parse([]byte(`{"version":1,"modules":{"audit":{"require":{"source":"example.com/team/mods","min_version":"1.2"},"x":1},"notice":{"enabled":false,"require":{"min_version":"2"}}}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := snap.Module("audit")
	if audit.Require.Source != "example.com/team/mods" || audit.Require.MinVersion != "1.2" || string(audit.Config.Bytes()) != `{"x":1}` {
		t.Fatalf("audit = %+v %s", audit.Require, audit.Config.Bytes())
	}
	d := snap.Desired()
	if len(d.Requirements) != 2 || d.Requirements["notice"].MinVersion != "2" {
		t.Fatalf("requirements = %+v", d.Requirements)
	}
	for name, data := range map[string]string{
		"require not object": `{"version":1,"modules":{"audit":{"require":"x"}}}`,
		"require null":       `{"version":1,"modules":{"audit":{"require":null}}}`,
		"require unknown":    `{"version":1,"modules":{"audit":{"require":{"core":"v1"}}}}`,
		"bad source":         `{"version":1,"modules":{"audit":{"require":{"source":"mods"}}}}`,
		"bad min version":    `{"version":1,"modules":{"audit":{"require":{"min_version":"x"}}}}`,
		"case require":       `{"version":1,"modules":{"audit":{"Require":{}}}}`,
	} {
		if _, err := Parse([]byte(data), ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
