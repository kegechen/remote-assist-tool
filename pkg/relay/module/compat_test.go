package module

import (
	"context"
	"strings"
	"testing"
)

func TestValidateDescriptorCompatibilityMetadata(t *testing.T) {
	cases := map[string]func(d *Descriptor){
		"missing source":   func(d *Descriptor) { d.Source = "" },
		"bare word source": func(d *Descriptor) { d.Source = "private" },
		"source scheme":    func(d *Descriptor) { d.Source = "https://example.com/x" },
		"missing api":      func(d *Descriptor) { d.APIVersion = 0 },
		"future api":       func(d *Descriptor) { d.APIVersion = APIVersion + 1 },
		"bad min core":     func(d *Descriptor) { d.MinCoreVersion = "v1.x" },
	}
	for name, mutate := range cases {
		d := validDescriptor()
		mutate(&d)
		if err := ValidateDescriptor(d); err == nil {
			t.Errorf("%s: accepted %+v", name, d)
		}
	}
	d := validDescriptor()
	d.Source, d.MinCoreVersion = "example.com/team/relay-modules", "v1.4.0"
	if err := ValidateDescriptor(d); err != nil {
		t.Fatalf("private descriptor rejected: %v", err)
	}
	d.APIVersion = 99
	if err := ValidateDescriptor(d); err == nil || !strings.Contains(err.Error(), "module API v99") || !strings.Contains(err.Error(), "example.com/team/relay-modules") {
		t.Fatalf("api mismatch error not explicit: %v", err)
	}
}

func TestCheckCoreCompatibility(t *testing.T) {
	d := validDescriptor()
	if note, err := CheckCoreCompatibility(d, "dev"); note != "" || err != nil {
		t.Fatalf("no requirement: %q %v", note, err)
	}
	d.MinCoreVersion = "v1.4.0"
	if _, err := CheckCoreCompatibility(d, "v1.3.9"); err == nil || !strings.Contains(err.Error(), "requires relay core >= v1.4.0") {
		t.Fatalf("older core accepted: %v", err)
	}
	for _, core := range []string{"v1.4.0", "1.5", "v2.0.0"} {
		if note, err := CheckCoreCompatibility(d, core); note != "" || err != nil {
			t.Fatalf("core %s: %q %v", core, note, err)
		}
	}
	for _, core := range []string{"dev", "v0.0.0-20261024000000-abcdef123456", "v1.4.0-rc1", ""} {
		note, err := CheckCoreCompatibility(d, core)
		if err != nil || !strings.Contains(note, "not verified") {
			t.Fatalf("core %q: %q %v", core, note, err)
		}
	}
}

func TestCheckRequirement(t *testing.T) {
	d := validDescriptor()
	d.Source, d.Version = "example.com/team/relay-modules", "1.2.0"
	if err := CheckRequirement(d, Requirement{Source: "example.com/team/relay-modules", MinVersion: "1.2"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckRequirement(d, Requirement{Source: SourceCore}); err == nil || !strings.Contains(err.Error(), "config expects source") {
		t.Fatalf("source mismatch accepted: %v", err)
	}
	if err := CheckRequirement(d, Requirement{MinVersion: "1.3"}); err == nil || !strings.Contains(err.Error(), "requires version >= 1.3") {
		t.Fatalf("old module accepted: %v", err)
	}
	for _, bad := range []Requirement{{Source: "nope"}, {MinVersion: "x"}} {
		if err := ValidateRequirement(bad); err == nil {
			t.Errorf("invalid requirement accepted: %+v", bad)
		}
	}
}

func TestManagerRejectsIncompatibleCoreAndRequirements(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "future", MinCoreVersion: "v2.0.0"})
	registerFake(t, reg, env, Descriptor{ID: "private", Version: "1.2.0", Source: "example.com/team/relay-modules"})
	m := NewManager(reg, SharedInfo{RelayVersion: "v1.5.0", CoreVersion: "v1.5.0"})
	m.logf = func(string, ...any) {}
	if _, err := m.Apply(context.Background(), desired(1, "future")); err == nil || !strings.Contains(err.Error(), "requires relay core >= v2.0.0, this relay core is v1.5.0") {
		t.Fatalf("incompatible core accepted: %v", err)
	}
	d := desired(2, "private")
	d.Requirements = map[string]Requirement{"private": {Source: SourceCore}}
	if _, err := m.Apply(context.Background(), d); err == nil || !strings.Contains(err.Error(), "config expects source") {
		t.Fatalf("source mismatch accepted: %v", err)
	}
	d.Requirements = map[string]Requirement{"ghost": {MinVersion: "1"}}
	if _, err := m.Apply(context.Background(), d); err == nil || !strings.Contains(err.Error(), `unknown module "ghost"`) {
		t.Fatalf("requirement for unknown module accepted: %v", err)
	}
	d.Requirements = map[string]Requirement{"private": {Source: "example.com/team/relay-modules", MinVersion: "1.2"}}
	if _, err := m.Apply(context.Background(), d); err != nil {
		t.Fatalf("matching requirement rejected: %v", err)
	}
	st := statusOf(t, m, "private")
	if st.Source != "example.com/team/relay-modules" || st.APIVersion != APIVersion || st.State != StateReady {
		t.Fatalf("status = %+v", st)
	}
	// dev 核心：核心版本要求无法验证，只告警、允许启用。
	var warned []string
	m2 := NewManager(reg, SharedInfo{CoreVersion: "dev"})
	m2.logf = func(format string, args ...any) { warned = append(warned, format) }
	if _, err := m2.Apply(context.Background(), desired(1, "future")); err != nil {
		t.Fatalf("dev core rejected: %v", err)
	}
	if len(warned) == 0 || !strings.Contains(warned[0], "WARNING") {
		t.Fatalf("no warning for unverified core: %v", warned)
	}
}

func TestRegistryCheckCoreAndDuplicateSource(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "a", MinCoreVersion: "v1.0.0"})
	registerFake(t, reg, env, Descriptor{ID: "b", MinCoreVersion: "v3.0.0"})
	notes, errs := reg.CheckCore("v2.0.0")
	if len(notes) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "module b") {
		t.Fatalf("notes=%v errs=%v", notes, errs)
	}
	err := reg.Register(func() Module {
		return &fakeModule{env: env, desc: Descriptor{ID: "a", Version: "1", Source: "example.com/other/mods", APIVersion: APIVersion}}
	})
	if err == nil || !strings.Contains(err.Error(), "existing source "+SourceCore+", new source example.com/other/mods") {
		t.Fatalf("duplicate id error lacks sources: %v", err)
	}
}

func TestRegisterBuiltinRecordsErrorsInsteadOfPanicking(t *testing.T) {
	builtinErrMu.Lock()
	saved := builtinErrs
	builtinErrs = nil
	builtinErrMu.Unlock()
	t.Cleanup(func() {
		builtinErrMu.Lock()
		builtinErrs = saved
		builtinErrMu.Unlock()
	})
	RegisterBuiltin(func() Module {
		return &fakeModule{desc: Descriptor{ID: "legacy", Version: "1", Source: "example.com/team/mods", APIVersion: 99}}
	})
	errs := BuiltinErrors()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "module API v99") {
		t.Fatalf("errors = %v", errs)
	}
	if ids := Builtin().IDs(); len(ids) != 0 {
		t.Fatalf("incompatible module registered: %v", ids)
	}
}
