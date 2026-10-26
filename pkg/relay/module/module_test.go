package module

import (
	"strings"
	"testing"
	"time"
)

func validDescriptor() Descriptor {
	return Descriptor{ID: "audit", Version: "1.0.0", Source: SourceCore, APIVersion: APIVersion}
}

func TestValidateDescriptorAcceptsMinimal(t *testing.T) {
	if err := ValidateDescriptor(validDescriptor()); err != nil {
		t.Fatalf("minimal descriptor rejected: %v", err)
	}
}

func TestValidateDescriptorRejects(t *testing.T) {
	cases := map[string]func(d *Descriptor){
		"empty id":         func(d *Descriptor) { d.ID = "" },
		"upper id":         func(d *Descriptor) { d.ID = "Audit" },
		"dash id":          func(d *Descriptor) { d.ID = "up-notice" },
		"long id":          func(d *Descriptor) { d.ID = strings.Repeat("a", 33) },
		"bad version":      func(d *Descriptor) { d.Version = "1.x" },
		"empty version":    func(d *Descriptor) { d.Version = "" },
		"plus version":     func(d *Descriptor) { d.Version = "+1" },
		"leading zero":     func(d *Descriptor) { d.Version = "01.2" },
		"config version":   func(d *Descriptor) { d.ConfigVersions = []int{0} },
		"self dependency":  func(d *Descriptor) { d.Requires = []Dependency{{ID: "audit"}} },
		"dup dependency":   func(d *Descriptor) { d.Requires = []Dependency{{ID: "a"}, {ID: "a", Optional: true}} },
		"bad dep id":       func(d *Descriptor) { d.Requires = []Dependency{{ID: "A"}} },
		"bad dep version":  func(d *Descriptor) { d.Requires = []Dependency{{ID: "a", MinVersion: "x"}} },
		"self conflict":    func(d *Descriptor) { d.Conflicts = []string{"audit"} },
		"dep and conflict": func(d *Descriptor) { d.Requires = []Dependency{{ID: "a"}}; d.Conflicts = []string{"a"} },
		"negative timeout": func(d *Descriptor) { d.StartTimeout = -time.Second },
		"huge timeout":     func(d *Descriptor) { d.StopTimeout = MaxLifecycleTimeout + time.Second },
		"too many tasks":   func(d *Descriptor) { d.MaxTasks = MaxTasksLimit + 1 },
		"dup config ver":   func(d *Descriptor) { d.ConfigVersions = []int{1, 1} },
		"dup conflict":     func(d *Descriptor) { d.Conflicts = []string{"a", "a"} },
		"bare v version":   func(d *Descriptor) { d.Version = "v" },
		"blank version":    func(d *Descriptor) { d.Version = " " },
		"five components":  func(d *Descriptor) { d.Version = "1.2.3.4.5" },
		"huge component":   func(d *Descriptor) { d.Version = "1234567890" },
		"upper V version":  func(d *Descriptor) { d.Version = "V1" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := validDescriptor()
			mutate(&d)
			if err := ValidateDescriptor(d); err == nil {
				t.Fatalf("descriptor %+v accepted", d)
			}
		})
	}
}

func TestDescriptorDefaults(t *testing.T) {
	d := validDescriptor()
	if d.startTimeout() != DefaultStartTimeout || d.stopTimeout() != DefaultStopTimeout || d.maxTasks() != DefaultMaxTasks {
		t.Fatalf("unexpected defaults: %v %v %v", d.startTimeout(), d.stopTimeout(), d.maxTasks())
	}
	if !d.SupportsConfigVersion(CurrentConfigVersion) || d.SupportsConfigVersion(CurrentConfigVersion+1) {
		t.Fatal("empty ConfigVersions must mean exactly CurrentConfigVersion")
	}
	d.ConfigVersions = []int{1, 2}
	if !d.SupportsConfigVersion(2) {
		t.Fatal("explicit config version not honored")
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"1.2.0", "1.2", true},
		{"1.2", "1.2.0", true},
		{"v1.10", "1.9", true},
		{"1.9", "1.10", false},
		{"2", "1.99.99", true},
		{"0.9", "1", false},
		{"0", "0.0", true},
	}
	for _, c := range cases {
		got, err := versionAtLeast(c.have, c.want)
		if err != nil || got != c.ok {
			t.Errorf("versionAtLeast(%q,%q)=%v,%v want %v", c.have, c.want, got, err, c.ok)
		}
	}
}

func TestVersionAtLeastRejectsInvalid(t *testing.T) {
	if _, err := versionAtLeast("x", "1"); err == nil {
		t.Fatal("invalid have accepted")
	}
	if _, err := versionAtLeast("1", ""); err == nil {
		t.Fatal("invalid want accepted")
	}
}

func TestConfigIsImmutableCopy(t *testing.T) {
	raw := []byte(`{"interval":"6h"}`)
	cfg := NewConfig("audit", 1, raw)
	raw[2] = 'X'
	out := cfg.Bytes()
	out[3] = 'Y'
	if string(cfg.Bytes()) != `{"interval":"6h"}` {
		t.Fatalf("config mutated through aliasing: %s", cfg.Bytes())
	}
	if string(NewConfig("audit", 1, nil).Bytes()) != "{}" {
		t.Fatal("empty config must be {}")
	}
}

func TestConfigDecodeStrict(t *testing.T) {
	var dst struct {
		Interval string `json:"interval"`
	}
	if err := NewConfig("audit", 1, []byte(`{"interval":"6h"}`)).Decode(&dst); err != nil || dst.Interval != "6h" {
		t.Fatalf("decode: %v %+v", err, dst)
	}
	if err := NewConfig("audit", 1, []byte(`{"intervl":"6h"}`)).Decode(&dst); err == nil {
		t.Fatal("unknown field must be rejected")
	}
	if err := NewConfig("audit", 1, []byte(`{"interval":"6h"} {}`)).Decode(&dst); err == nil {
		t.Fatal("trailing data must be rejected")
	}
}

func TestConfigEqual(t *testing.T) {
	a := NewConfig("audit", 1, []byte(`{"a":1}`))
	if !a.Equal(NewConfig("audit", 1, []byte(`{"a":1}`))) {
		t.Fatal("identical configs not equal")
	}
	if a.Equal(NewConfig("audit", 1, []byte(`{"a":2}`))) || a.Equal(NewConfig("audit", 2, []byte(`{"a":1}`))) {
		t.Fatal("different configs reported equal")
	}
}
