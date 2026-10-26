package module

import (
	"fmt"
	"sort"
	"strings"
)

// Plan 是一组启用模块经依赖校验后的启动计划。
type Plan struct {
	// Order 启动顺序（依赖在前）；停止按其逆序。同层按 ID 升序，结果确定。
	Order []string
	// MissingOptional 每个模块缺失（未注册或未启用）的可选依赖；非空的模块以 degraded 运行。
	MissingOptional map[string][]string
	// Dependents 反向依赖：被依赖模块 ID → 依赖它的已启用模块 ID（含可选依赖，升序）。
	Dependents map[string][]string
}

// BuildPlan 校验 enabled 中模块的注册、依赖、版本、冲突和循环依赖，返回启动计划。
// 任何一项不满足都返回错误且不产出部分计划。
func BuildPlan(reg *Registry, enabled []string) (Plan, error) {
	ids := append([]string(nil), enabled...)
	sort.Strings(ids)
	descs := make(map[string]Descriptor, len(ids))
	for i, id := range ids {
		if i > 0 && ids[i-1] == id {
			return Plan{}, fmt.Errorf("module %s listed twice", id)
		}
		d, ok := reg.Descriptor(id)
		if !ok {
			return Plan{}, fmt.Errorf("unknown module %q (not a built-in module)", id)
		}
		descs[id] = d
	}

	plan := Plan{MissingOptional: map[string][]string{}, Dependents: map[string][]string{}}
	edges := make(map[string][]string, len(ids)) // id -> 已启用的依赖
	var errs []string
	for _, id := range ids {
		d := descs[id]
		for _, c := range d.Conflicts {
			if _, on := descs[c]; on {
				errs = append(errs, fmt.Sprintf("module %s conflicts with enabled module %s", id, c))
			}
		}
		for _, dep := range d.Requires {
			depDesc, on := descs[dep.ID]
			if !on {
				if dep.Optional {
					plan.MissingOptional[id] = append(plan.MissingOptional[id], dep.ID)
					continue
				}
				if _, registered := reg.Descriptor(dep.ID); registered {
					errs = append(errs, fmt.Sprintf("module %s requires module %s, which is not enabled", id, dep.ID))
				} else {
					errs = append(errs, fmt.Sprintf("module %s requires unknown module %s", id, dep.ID))
				}
				continue
			}
			if dep.MinVersion != "" {
				ok, err := versionAtLeast(depDesc.Version, dep.MinVersion)
				if err != nil || !ok {
					errs = append(errs, fmt.Sprintf("module %s requires %s >= %s, have %s", id, dep.ID, dep.MinVersion, depDesc.Version))
					continue
				}
			}
			edges[id] = append(edges[id], dep.ID)
			plan.Dependents[dep.ID] = append(plan.Dependents[dep.ID], id)
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return Plan{}, fmt.Errorf("module dependency check failed: %s", strings.Join(errs, "; "))
	}
	if cycle := findCycle(ids, edges); cycle != nil {
		return Plan{}, fmt.Errorf("module dependency cycle: %s", strings.Join(cycle, " -> "))
	}

	// Kahn 拓扑排序；每轮取入度为 0 中 ID 最小者，保证结果确定。
	indeg := make(map[string]int, len(ids))
	for _, id := range ids {
		indeg[id] = len(edges[id])
	}
	for k := range plan.Dependents {
		sort.Strings(plan.Dependents[k])
	}
	ready := []string{}
	for _, id := range ids {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	for len(ready) > 0 {
		sort.Strings(ready)
		id := ready[0]
		ready = ready[1:]
		plan.Order = append(plan.Order, id)
		for _, child := range plan.Dependents[id] {
			indeg[child]--
			if indeg[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if len(plan.Order) != len(ids) {
		// findCycle 已排除环，走到这里说明实现有缺陷；宁可拒绝也不产出残缺计划。
		return Plan{}, fmt.Errorf("module dependency ordering incomplete")
	}
	return plan, nil
}

// findCycle 返回一个依赖环（首尾相同），无环返回 nil。
func findCycle(ids []string, edges map[string][]string) []string {
	const (
		white = iota
		grey
		black
	)
	color := make(map[string]int, len(ids))
	var stack []string
	var cycle []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = grey
		stack = append(stack, id)
		deps := append([]string(nil), edges[id]...)
		sort.Strings(deps)
		for _, dep := range deps {
			switch color[dep] {
			case grey:
				for i, s := range stack {
					if s == dep {
						cycle = append(append([]string(nil), stack[i:]...), dep)
						return true
					}
				}
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	for _, id := range ids {
		if color[id] == white && visit(id) {
			return cycle
		}
	}
	return nil
}

// closureOfDependents 返回 roots 及其所有（传递）依赖者。
func (p Plan) closureOfDependents(roots []string) map[string]bool {
	out := map[string]bool{}
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if out[id] {
			continue
		}
		out[id] = true
		queue = append(queue, p.Dependents[id]...)
	}
	return out
}
