package module

import (
	"reflect"
	"strings"
	"testing"
)

func TestRegistryRegisterAndLookup(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "zeta", Requires: []Dependency{{ID: "alpha"}}})
	registerFake(t, reg, env, Descriptor{ID: "alpha"})
	if got := reg.IDs(); !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("IDs = %v", got)
	}
	d, ok := reg.Descriptor("zeta")
	if !ok || d.Requires[0].ID != "alpha" {
		t.Fatalf("lookup: %v %+v", ok, d)
	}
	d.Requires[0].ID = "mutated"
	if d2, _ := reg.Descriptor("zeta"); d2.Requires[0].ID != "alpha" {
		t.Fatal("Descriptor returned an alias into the registry")
	}
	if _, ok := reg.Descriptor("missing"); ok {
		t.Fatal("unregistered module found")
	}
}

func TestRegistryRejects(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	registerFake(t, reg, env, Descriptor{ID: "audit"})
	cases := map[string]Factory{
		"nil factory": nil,
		"duplicate": func() Module {
			return &fakeModule{env: env, desc: Descriptor{ID: "audit", Version: "1", Source: SourceCore, APIVersion: APIVersion}}
		},
		"bad id":        func() Module { return &fakeModule{env: env, desc: Descriptor{ID: "Bad", Version: "1"}} },
		"nil module":    func() Module { return nil },
		"panic factory": func() Module { panic("boom") },
	}
	for name, f := range cases {
		if err := reg.Register(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(error).Error(), "already registered") {
			t.Fatalf("MustRegister did not panic on duplicate: %v", r)
		}
	}()
	reg.MustRegister(cases["duplicate"])
}

func TestRegistryNewInstanceChecksFactoryConsistency(t *testing.T) {
	env := newFakeEnv(t)
	reg := NewRegistry()
	calls := 0
	if err := reg.Register(func() Module {
		calls++
		id := "audit"
		if calls > 1 {
			id = "other"
		}
		return &fakeModule{env: env, desc: Descriptor{ID: id, Version: "1", Source: SourceCore, APIVersion: APIVersion}}
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.newInstance("audit"); err == nil {
		t.Fatal("factory returning a different module ID must be rejected")
	}
	if _, _, err := reg.newInstance("missing"); err == nil {
		t.Fatal("unregistered module instantiated")
	}
}

func TestBuiltinRegistryIsEmptyInPhaseOne(t *testing.T) {
	if ids := Builtin().IDs(); len(ids) != 0 {
		t.Fatalf("phase 1 must not migrate any module, got %v", ids)
	}
}
