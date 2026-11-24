package client

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/proto"
)

func pickNode(prefix, status string, weight int) directory.Node {
	return directory.Node{
		NodeCert: directory.NodeCert{
			Prefix: prefix,
			Name:   "node-" + prefix,
			TCP:    []string{"relay-" + prefix + ".example.test:8443"},
			QUIC:   []string{"relay-" + prefix + ".example.test:8443"},
		},
		Status: status,
		Weight: weight,
	}
}

func pickDir(policy, def string, nodes ...directory.Node) *directory.Verified {
	return &directory.Verified{Policy: policy, DefaultPrefix: def, Nodes: nodes}
}

func prefixesOf(nodes []directory.Node) string {
	var s string
	for _, n := range nodes {
		s += n.Prefix
	}
	return s
}

func TestRouteNodeForCode(t *testing.T) {
	v := pickDir(directory.PolicyPreferDefault, "A",
		pickNode("A", directory.StatusActive, 100),
		pickNode("K", directory.StatusDraining, 50),
		pickNode("W", directory.StatusDisabled, 50),
		pickNode("N", directory.StatusActive, 50),
		pickNode("S", directory.StatusActive, 50),
	)
	cases := []struct {
		name, code, want string
	}{
		// --no-auth 的固定码不是任何节点发的：首位 n 碰巧是节点 N 的前缀也不能路由过去。
		{"no-auth 固定码不路由", proto.NoAuthCode, ""},
		// relay 用 strings.ToUpper 规范化协助码，ſ（长 s）会变成 S；路由必须与它一致，否则会连错节点。
		{"长 s 按 relay 的大写规则归一", "ſBCDEFGHJKMN", "S"},
		{"大写码按首位", "ABCD-EFGH-JKMN", "A"},
		{"小写码也按首位", "kbcd-efgh-jkmn", "K"},
		{"带空格和下划线", " k_bcdefghjkmn", "K"},
		{"排空的节点仍能路由（它发过的码要能加入）", "KBCDEFGHJKMN", "K"},
		{"停用的节点不路由", "WBCDEFGHJKMN", ""},
		{"列表里没有这个前缀", "ZBCDEFGHJKMN", ""},
		{"空码", "", ""},
		{"只有分隔符", "- _-", ""},
		{"首位不是 ASCII", "ǅBCDEFGHJKMN", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routeNodeForCode(v, tc.code)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("routeNodeForCode(%q) = node %s, want nil", tc.code, got.Prefix)
				}
				return
			}
			if got == nil || got.Prefix != tc.want {
				t.Fatalf("routeNodeForCode(%q) = %v, want node %s", tc.code, got, tc.want)
			}
		})
	}
	if got := routeNodeForCode(nil, "ABCDEFGHJKMN"); got != nil {
		t.Fatalf("没有列表时 routeNodeForCode = %v, want nil", got)
	}
}

func TestShareCandidatesFilterAndOrder(t *testing.T) {
	v := pickDir(directory.PolicyPreferDefault, "A",
		pickNode("A", directory.StatusActive, 100),
		pickNode("B", directory.StatusDraining, 100),
		pickNode("C", directory.StatusDisabled, 100),
		pickNode("D", directory.StatusActive, 0),
		pickNode("E", directory.StatusActive, 60),
		pickNode("F", directory.StatusActive, 80),
		pickNode("G", directory.StatusActive, 60),
	)
	got := prefixesOf(shareCandidates(v, allowAll))
	// active 且权重>0；按权重从高到低，权重相同按前缀字母序。
	if got != "AFEG" {
		t.Fatalf("shareCandidates = %q, want AFEG", got)
	}
	if shareCandidates(nil, allowAll) != nil {
		t.Fatal("没有列表时不该有候选")
	}
}

func TestShareCandidatesCapKeepsDefault(t *testing.T) {
	var nodes []directory.Node
	for i, p := range "ABCDEFGHJK" { // 10 个节点，权重递减
		nodes = append(nodes, pickNode(string(p), directory.StatusActive, 100-i))
	}
	v := pickDir(directory.PolicyPreferDefault, "A", nodes...)
	if got := prefixesOf(shareCandidates(v, allowAll)); got != "ABCDEFGH" {
		t.Fatalf("超过 8 个时按权重取前 8: got %q, want ABCDEFGH", got)
	}

	// 默认节点权重最低、本来排在 8 个之外：prefer-default 要先连它，所以必须保留。
	nodes[0].Weight = 1
	v = pickDir(directory.PolicyPreferDefault, "A", nodes...)
	got := prefixesOf(shareCandidates(v, allowAll))
	if len(got) != 8 || !slices.Contains([]byte(got), 'A') {
		t.Fatalf("默认节点必须留在候选里, got %q", got)
	}
	if got != "BCDEFGHA" {
		t.Fatalf("默认节点顶掉第 8 名: got %q, want BCDEFGHA", got)
	}

	// 默认节点正好排第 9：同样要顶掉第 8 名（边界：被挤出去的第一个）。
	nodes[0].Weight = 100
	v = pickDir(directory.PolicyPreferDefault, "J", nodes...)
	if got := prefixesOf(shareCandidates(v, allowAll)); got != "ABCDEFGJ" {
		t.Fatalf("默认节点排第 9 时也要保留: got %q, want ABCDEFGJ", got)
	}

	// 默认节点不合格（停用）时不为它留位置。
	nodes[0].Weight = 1
	nodes[0].Status = directory.StatusDisabled
	v = pickDir(directory.PolicyPreferDefault, "A", nodes...)
	if got := prefixesOf(shareCandidates(v, allowAll)); got != "BCDEFGHJ" {
		t.Fatalf("got %q, want BCDEFGHJ", got)
	}

	// fastest 下所有候选一视同仁，只按权重取前 8：默认节点权重低就不在里面（spec 6.3、6.9）。
	nodes[0].Weight = 1
	nodes[0].Status = directory.StatusActive
	v = pickDir(directory.PolicyFastest, "A", nodes...)
	if got := prefixesOf(shareCandidates(v, allowAll)); got != "BCDEFGHJ" {
		t.Fatalf("fastest 不为默认节点留位置: got %q, want BCDEFGHJ", got)
	}

	// 退避期内的节点不占名额：默认节点在退避期时，前 8 个名额都留给能拨的节点。
	notA := func(prefix string) bool { return prefix != "A" }
	nodes[0].Weight = 100
	v = pickDir(directory.PolicyPreferDefault, "A", nodes...)
	if got := prefixesOf(shareCandidates(v, notA)); got != "BCDEFGHJ" {
		t.Fatalf("默认节点在退避期（权重最高）: got %q, want BCDEFGHJ", got)
	}
	nodes[0].Weight = 1
	v = pickDir(directory.PolicyPreferDefault, "A", nodes...)
	if got := prefixesOf(shareCandidates(v, notA)); got != "BCDEFGHJ" {
		t.Fatalf("默认节点在退避期（权重最低）不该顶掉别人: got %q, want BCDEFGHJ", got)
	}
	// 非默认节点在退避期：名额顺延给排在 8 名之外的节点。
	nodes[0].Weight = 100
	v = pickDir(directory.PolicyPreferDefault, "A", nodes...)
	notB := func(prefix string) bool { return prefix != "B" }
	if got := prefixesOf(shareCandidates(v, notB)); got != "ACDEFGHJ" {
		t.Fatalf("退避的节点不占名额，名额顺延: got %q, want ACDEFGHJ", got)
	}
}

func allowAll(string) bool { return true }

func TestPlanSharePreferDefault(t *testing.T) {
	v := pickDir(directory.PolicyPreferDefault, "A",
		pickNode("K", directory.StatusActive, 90),
		pickNode("A", directory.StatusActive, 50),
		pickNode("W", directory.StatusActive, 70),
	)
	p := planShare(v, allowAll)
	if p.First == nil || p.First.Prefix != "A" {
		t.Fatalf("prefer-default 先连默认节点 A, got %+v", p.First)
	}
	var got string
	for _, r := range p.Racers {
		got += r.Node.Prefix
		if r.Delay != 0 {
			t.Fatalf("prefer-default 的备选同时起跑，节点 %s 延迟 %v", r.Node.Prefix, r.Delay)
		}
	}
	if got != "KW" {
		t.Fatalf("备选 = %q, want KW（默认节点不重复，按权重降序）", got)
	}
}

func TestPlanSharePreferDefaultWithoutUsableDefault(t *testing.T) {
	for name, mod := range map[string]func(*directory.Node){
		"默认节点停用":   func(n *directory.Node) { n.Status = directory.StatusDisabled },
		"默认节点排空":   func(n *directory.Node) { n.Status = directory.StatusDraining },
		"默认节点权重 0": func(n *directory.Node) { n.Weight = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			def := pickNode("A", directory.StatusActive, 100)
			mod(&def)
			v := pickDir(directory.PolicyPreferDefault, "A", def,
				pickNode("K", directory.StatusActive, 90), pickNode("W", directory.StatusActive, 70))
			p := planShare(v, allowAll)
			if p.First != nil {
				t.Fatalf("默认节点不可用时没有「先连」的节点, got %s", p.First.Prefix)
			}
			if len(p.Racers) != 2 || p.Racers[0].Node.Prefix != "K" || p.Racers[1].Node.Prefix != "W" {
				t.Fatalf("其余候选并行建连, got %+v", p.Racers)
			}
		})
	}
	// 默认节点在失败退避期内：同样不先连，也不出现在备选里。
	v := pickDir(directory.PolicyPreferDefault, "A",
		pickNode("A", directory.StatusActive, 100), pickNode("K", directory.StatusActive, 90))
	p := planShare(v, func(prefix string) bool { return prefix != "A" })
	if p.First != nil || len(p.Racers) != 1 || p.Racers[0].Node.Prefix != "K" {
		t.Fatalf("默认节点退避期内: First=%v Racers=%+v", p.First, p.Racers)
	}
}

func TestPlanShareFastest(t *testing.T) {
	v := pickDir(directory.PolicyFastest, "A",
		pickNode("A", directory.StatusActive, 100),
		pickNode("K", directory.StatusActive, 90),
		pickNode("W", directory.StatusActive, 1),
		pickNode("X", directory.StatusActive, 0),
	)
	p := planShare(v, allowAll)
	if p.First != nil {
		t.Fatalf("fastest 不先单独连默认节点, got %s", p.First.Prefix)
	}
	want := map[string]time.Duration{"A": 0, "K": 30 * time.Millisecond, "W": 297 * time.Millisecond}
	if len(p.Racers) != len(want) {
		t.Fatalf("Racers = %+v, want %d 个（权重 0 不参与）", p.Racers, len(want))
	}
	for _, r := range p.Racers {
		if d, ok := want[r.Node.Prefix]; !ok || r.Delay != d {
			t.Fatalf("节点 %s 起跑延迟 %v, want %v", r.Node.Prefix, r.Delay, d)
		}
	}
}

func TestPlanShareAppliesBackoffFilter(t *testing.T) {
	v := pickDir(directory.PolicyFastest, "A",
		pickNode("A", directory.StatusActive, 100),
		pickNode("K", directory.StatusActive, 90),
	)
	p := planShare(v, func(prefix string) bool { return prefix == "K" })
	if len(p.Racers) != 1 || p.Racers[0].Node.Prefix != "K" {
		t.Fatalf("退避期内的节点不拨: %+v", p.Racers)
	}
	p = planShare(v, func(string) bool { return false })
	if p.First != nil || len(p.Racers) != 0 {
		t.Fatalf("全都在退避期时计划为空: %+v", p)
	}
	if p := planShare(nil, allowAll); p.First != nil || len(p.Racers) != 0 {
		t.Fatalf("没有列表: %+v", p)
	}
}

// 默认节点在退避期、节点超过 8 个：不拨默认节点，但也不能因为给它留过名额而少拨一个。
func TestPlanShareBackoffDoesNotWasteSlots(t *testing.T) {
	var nodes []directory.Node
	for i, p := range "ABCDEFGHJK" {
		nodes = append(nodes, pickNode(string(p), directory.StatusActive, 100-i))
	}
	nodes[0].Weight = 1 // 默认节点 A 权重最低，原本排在 8 个之外
	v := pickDir(directory.PolicyPreferDefault, "A", nodes...)
	p := planShare(v, func(prefix string) bool { return prefix != "A" })
	var got string
	for _, r := range p.Racers {
		got += r.Node.Prefix
	}
	if p.First != nil || got != "BCDEFGHJ" {
		t.Fatalf("First=%v Racers=%q, want 无 First 与 BCDEFGHJ（8 个都能拨）", p.First, got)
	}
}

// 未知的策略值按 prefer-default 处理（保守：默认节点优先，和没有这个功能时最接近）。
func TestPlanShareUnknownPolicyIsPreferDefault(t *testing.T) {
	v := pickDir("something-new", "A",
		pickNode("A", directory.StatusActive, 100), pickNode("K", directory.StatusActive, 90))
	p := planShare(v, allowAll)
	if p.First == nil || p.First.Prefix != "A" {
		t.Fatalf("未知策略应按 prefer-default 先连默认节点, got %+v", p.First)
	}
}

func TestStaggerDelay(t *testing.T) {
	cases := []struct {
		weight int
		want   time.Duration
	}{
		{100, 0}, {99, 3 * time.Millisecond}, {90, 30 * time.Millisecond},
		{50, 150 * time.Millisecond}, {1, 297 * time.Millisecond}, {0, 300 * time.Millisecond},
		{-5, 300 * time.Millisecond}, {250, 0},
	}
	for _, tc := range cases {
		if got := staggerDelay(tc.weight); got != tc.want {
			t.Errorf("staggerDelay(%d) = %v, want %v", tc.weight, got, tc.want)
		}
	}
}

func TestNodeDialOrder(t *testing.T) {
	type tgt = nodeTarget
	node := func(tcp, quic []string) directory.Node {
		n := pickNode("K", directory.StatusActive, 100)
		n.TCP, n.QUIC = tcp, quic
		return n
	}
	cases := []struct {
		name  string
		node  directory.Node
		local string
		want  []nodeTarget
	}{
		{"TCP 与 QUIC 同址", node([]string{"a.example.test:8443"}, []string{"a.example.test:8443"}), "",
			[]tgt{{"a.example.test:8443", true, true}}},
		{"只有 TCP", node([]string{"a.example.test:8443"}, nil), "",
			[]tgt{{"a.example.test:8443", true, false}}},
		{"多个地址按列表顺序，QUIC 独有的排在后面",
			node([]string{"a.example.test:8443", "b.example.test:8443"}, []string{"a.example.test:8443", "c.example.test:8443"}), "",
			[]tgt{{"a.example.test:8443", true, true}, {"b.example.test:8443", true, false}, {"c.example.test:8443", false, true}}},
		{"本机专用地址排最前，节点声明了 QUIC 时也允许 QUIC",
			node([]string{"a.example.test:8443"}, []string{"a.example.test:8443"}), "192.0.2.10:8443",
			[]tgt{{"192.0.2.10:8443", true, true}, {"a.example.test:8443", true, true}}},
		{"节点没有 QUIC 时本机专用地址只走 TCP",
			node([]string{"a.example.test:8443"}, nil), "192.0.2.10:8443",
			[]tgt{{"192.0.2.10:8443", true, false}, {"a.example.test:8443", true, false}}},
		{"本机专用地址与列表地址重复：提到最前并合并传输、不重复拨",
			node([]string{"a.example.test:8443", "b.example.test:8443"}, []string{"a.example.test:8443"}), "b.example.test:8443",
			[]tgt{{"b.example.test:8443", true, true}, {"a.example.test:8443", true, true}}},
		{"主机名只差大小写算同一个地址：保留本机专用地址的写法、只拨一次",
			node([]string{"relay-k.example.test:8443"}, nil), "Relay-K.example.test:8443",
			[]tgt{{"Relay-K.example.test:8443", true, false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeDialOrder(tc.node, tc.local)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("nodeDialOrder = %+v\nwant           %+v", got, tc.want)
			}
		})
	}
}

type backoffClock struct{ t time.Time }

func (c *backoffClock) now() time.Time { return c.t }

func newTestBackoff(clock *backoffClock, jitter float64) *nodeBackoff {
	b := newNodeBackoff()
	b.now = clock.now
	b.jitter = func() float64 { return jitter }
	return b
}

func TestNodeBackoffProgression(t *testing.T) {
	clock := &backoffClock{t: time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)}
	b := newTestBackoff(clock, 0)
	if !b.Allowed("K") {
		t.Fatal("没失败过的节点应当可拨")
	}
	for i, want := range []time.Duration{
		60 * time.Second, 120 * time.Second, 240 * time.Second, 480 * time.Second,
		600 * time.Second, 600 * time.Second, 600 * time.Second,
	} {
		b.Fail("K")
		if b.Allowed("K") {
			t.Fatalf("第 %d 次失败后立刻就该在退避期内（包括退避恰好是最长 10 分钟的那几次）", i+1)
		}
		clock.t = clock.t.Add(want - time.Second)
		if b.Allowed("K") {
			t.Fatalf("第 %d 次失败后 %v 还没到，不该可拨", i+1, want-time.Second)
		}
		clock.t = clock.t.Add(time.Second)
		if !b.Allowed("K") {
			t.Fatalf("第 %d 次失败后过了 %v 应当可拨", i+1, want)
		}
	}
}

// 抖动只拉长（最多 +20%）：spec 6.9 的「60 秒内不再拨」是下限、「最长 10 分钟」是上限，抖动都不能突破。
func TestNodeBackoffJitterOnlyLengthens(t *testing.T) {
	clock := &backoffClock{t: time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)}
	// 抖动取最大：第一次退避拉长到约 72s。
	b := newTestBackoff(clock, 0.999999)
	b.Fail("K")
	clock.t = clock.t.Add(59 * time.Second)
	if b.Allowed("K") {
		t.Fatal("59s 时还在退避期内（抖动不能把 60s 缩短）")
	}
	clock.t = clock.t.Add(12 * time.Second) // 71s
	if b.Allowed("K") {
		t.Fatal("71s 时还在退避期内（抖动取最大，60s 拉长到约 72s）")
	}
	clock.t = clock.t.Add(2 * time.Second) // 73s
	if !b.Allowed("K") {
		t.Fatal("73s 时应当可拨（抖动最多拉长 20%）")
	}
	// 封顶：失败很多次后单次退避不超过 10 分钟，抖动也不能把它再拉长。
	// 失败次数多到逐次翻倍会溢出时，退避也得停在上限上，不能算出负数而放行。
	// 逐次检查：翻倍溢出回绕出的值可能是负数、零或很大的正数，只看最后一次会碰巧通过。
	for i := 0; i < 80; i++ {
		b.Fail("K")
		if b.Allowed("K") {
			t.Fatalf("连续失败 %d 次后立刻就该在退避期内", i+2)
		}
	}
	clock.t = clock.t.Add(599 * time.Second)
	if b.Allowed("K") {
		t.Fatal("失败很多次后 599s 时仍在退避期内（封顶 10 分钟，不是放行）")
	}
	clock.t = clock.t.Add(2 * time.Second) // 601s
	if !b.Allowed("K") {
		t.Fatal("退避最长 10 分钟：抖动不能把封顶的退避拉到 601s 之后")
	}
}

// 多个 goroutine 同时失败/成功/查询同一个 nodeBackoff：不能出现并发写 map（本机没有 gcc，跑不了 -race，
// 并发写 map 即使没有 -race 也会让运行时直接 fatal，所以这里至少能抓到去掉锁的情形）。
func TestNodeBackoffConcurrent(t *testing.T) {
	b := newNodeBackoff()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(prefix string) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				b.Fail(prefix)
				b.Allowed(prefix)
				if i%7 == 0 {
					b.Succeed(prefix)
				}
			}
		}(string(rune('A' + g)))
	}
	wg.Wait()
}

func TestNodeBackoffSucceedResetsAndNodesAreIndependent(t *testing.T) {
	clock := &backoffClock{t: time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)}
	b := newTestBackoff(clock, 0)
	b.Fail("K")
	b.Fail("K")
	b.Fail("K")
	if !b.Allowed("A") {
		t.Fatal("别的节点不受影响")
	}
	b.Succeed("K")
	if !b.Allowed("K") {
		t.Fatal("成功后立即可拨")
	}
	b.Fail("K")
	clock.t = clock.t.Add(61 * time.Second)
	if !b.Allowed("K") {
		t.Fatal("成功后失败计数清零：再失败一次只退避 60 秒")
	}
}

// 墙钟被调回很久以前：剩余的等待时间不可能超过最长退避，超过就说明时钟乱了，放行，
// 不能让一个节点被封一整天。（真实时钟带单调读数，不受调表影响；这里模拟的是没有单调读数的情况。）
func TestNodeBackoffClockGoingBackwards(t *testing.T) {
	clock := &backoffClock{t: time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)}
	b := newTestBackoff(clock, 0)
	b.Fail("K")
	clock.t = clock.t.Add(-24 * time.Hour)
	if !b.Allowed("K") {
		t.Fatal("时钟被调回一天，节点不该被封到时钟追上为止")
	}
	// 小幅回拨（仍在最长退避之内）照常等。
	clock.t = clock.t.Add(24*time.Hour + 30*time.Second)
	b.Fail("A")
	clock.t = clock.t.Add(-5 * time.Second)
	if b.Allowed("A") {
		t.Fatal("小幅回拨时仍在退避期内")
	}
}
