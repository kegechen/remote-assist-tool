package module

import (
	"reflect"
	"strings"
	"testing"
)

func graphRegistry(t *testing.T, descs ...Descriptor) *Registry {
	t.Helper()
	env := newFakeEnv(t)
	reg := NewRegistry()
	for _, d := range descs {
		registerFake(t, reg, env, d)
	}
	return reg
}

func TestBuildPlanOrdersDependenciesDeterministically(t *testing.T) {
	reg := graphRegistry(t,
		Descriptor{ID: "notify", Requires: []Dependency{{ID: "audit"}, {ID: "store"}}},
		Descriptor{ID: "audit", Requires: []Dependency{{ID: "store"}}},
		Descriptor{ID: "store"},
		Descriptor{ID: "alpha"},
	)
	for i := 0; i < 5; i++ {
		plan, err := BuildPlan(reg, []string{"notify", "alpha", "store", "audit"})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"alpha", "store", "audit", "notify"}; !reflect.DeepEqual(plan.Order, want) {
			t.Fatalf("order = %v, want %v", plan.Order, want)
		}
		if want := []string{"audit", "notify"}; !reflect.DeepEqual(plan.Dependents["store"], want) {
			t.Fatalf("dependents = %v", plan.Dependents["store"])
		}
	}
	if plan, err := BuildPlan(reg, nil); err != nil || len(plan.Order) != 0 {
		t.Fatalf("empty plan: %v %v", plan, err)
	}
}

func TestBuildPlanRejects(t *testing.T) {
	reg := graphRegistry(t,
		Descriptor{ID: "a", Requires: []Dependency{{ID: "b"}}},
		Descriptor{ID: "b", Requires: []Dependency{{ID: "a"}}},
		Descriptor{ID: "c", Requires: []Dependency{{ID: "d"}}},
		Descriptor{ID: "d", Requires: []Dependency{{ID: "e"}}},
		Descriptor{ID: "e", Requires: []Dependency{{ID: "c", Optional: true}}},
		Descriptor{ID: "needs_ghost", Requires: []Dependency{{ID: "ghost"}}},
		Descriptor{ID: "needs_new", Requires: []Dependency{{ID: "old", MinVersion: "2.0"}}},
		Descriptor{ID: "old", Version: "1.9.9"},
		Descriptor{ID: "x", Conflicts: []string{"y"}},
		Descriptor{ID: "y"},
		Descriptor{ID: "z"},
	)
	cases := []struct {
		enabled []string
		want    string
	}{
		{[]string{"unknown_mod"}, `unknown module "unknown_mod"`},
		{[]string{"z", "z"}, "listed twice"},
		{[]string{"a", "b"}, "cycle: a -> b -> a"},
		{[]string{"c", "d", "e"}, "cycle: c -> d -> e -> c"},
		{[]string{"c"}, "requires module d, which is not enabled"},
		{[]string{"needs_ghost"}, "requires unknown module ghost"},
		{[]string{"needs_new", "old"}, "requires old >= 2.0, have 1.9.9"},
		{[]string{"x", "y"}, "x conflicts with enabled module y"},
	}
	for _, c := range cases {
		_, err := BuildPlan(reg, c.enabled)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("BuildPlan(%v) error = %v, want %q", c.enabled, err, c.want)
		}
	}
}

func TestBuildPlanOptionalDependencies(t *testing.T) {
	reg := graphRegistry(t,
		Descriptor{ID: "notify", Requires: []Dependency{{ID: "audit", Optional: true}, {ID: "ghost", Optional: true}}},
		Descriptor{ID: "audit"},
	)
	plan, err := BuildPlan(reg, []string{"notify"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"audit", "ghost"}; !reflect.DeepEqual(plan.MissingOptional["notify"], want) {
		t.Fatalf("missing optional = %v", plan.MissingOptional["notify"])
	}
	// 可选依赖启用时参与排序。
	plan, err = BuildPlan(reg, []string{"notify", "audit"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"audit", "notify"}; !reflect.DeepEqual(plan.Order, want) {
		t.Fatalf("order = %v", plan.Order)
	}
	if !reflect.DeepEqual(plan.MissingOptional["notify"], []string{"ghost"}) {
		t.Fatalf("missing optional = %v", plan.MissingOptional["notify"])
	}
	if got := plan.closureOfDependents([]string{"audit"}); !got["audit"] || !got["notify"] || len(got) != 2 {
		t.Fatalf("closure = %v", got)
	}
}
