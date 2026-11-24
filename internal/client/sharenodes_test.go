package client

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/directory"
	"github.com/remote-assist/tool/internal/directory/dirtest"
	"github.com/remote-assist/tool/internal/proto"
)

// liveNode 是测试里的一个节点：一台真的 TLS relay（只开 TCP）和它的证书指纹。
type liveNode struct {
	addr string
	fp   string
	stop func()
}

func startLiveNode(t *testing.T) liveNode {
	t.Helper()
	cert, key := newCertPair(t)
	addr := freePort(t)
	stop := startRelay(t, addr, "", cert, key)
	return liveNode{addr: addr, fp: certFP(t, cert, key), stop: stop}
}

// deadNode 是一个连不上的节点：地址上没人监听，指纹随便给一个合法的。
func deadNode(t *testing.T) liveNode {
	t.Helper()
	return liveNode{addr: deadAddr(t), fp: strings.Repeat("ab", 32)}
}

// nodeEntry 造一项列表：节点 prefix 只有 TCP 地址 addr，证书指纹 fp。
func (e *dirEnv) nodeEntry(prefix string, n liveNode, status string, weight int) directory.NodeEntry {
	c := e.f.Cert(prefix, func(c *directory.NodeCert) {
		c.TCP = []string{n.addr}
		c.QUIC = nil
		c.TLSFPs = []string{n.fp}
	})
	return directory.NodeEntry{Cert: e.f.SignCert(c), Status: status, Weight: weight}
}

// publish 把序号为 serial 的列表放进缓存（相当于别的进程或 CF 刚收下它），第一项是默认节点。
func (e *dirEnv) publish(m *dirManager, serial uint64, policy string, nodes ...directory.NodeEntry) {
	e.t.Helper()
	b := e.f.Body(nodes...)
	b.Serial = serial
	if policy != "" {
		b.Policy = policy
	}
	if v := m.Offer(e.f.Sign(b), "test"); v == nil {
		e.t.Fatalf("列表 %d 没有被收下", serial)
	}
}

func (e *dirEnv) setLocalAddr(prefix, addr string) {
	e.t.Helper()
	err := directory.UpdateCache(e.path, func(c *directory.Cache) (bool, error) {
		return c.SetLocalAddr(prefix, addr)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// shareNodesEnv 返回一个节点列表环境、选节点状态和一个 Client（默认地址 defaultAddr）。
func shareNodesEnv(t *testing.T, defaultAddr, hint string) (*dirEnv, *dirManager, *shareNodes, *Client) {
	t.Helper()
	e := newDirEnv(t)
	m := e.manager()
	sn := newShareNodes(m, "", defaultAddr, hint, e.logf)
	return e, m, sn, testClient(t, defaultAddr, "")
}

// connectNode 让 sn 连一次，然后确认连接真的通到一台 relay。
func connectNode(t *testing.T, sn *shareNodes, c *Client) {
	t.Helper()
	c.Close()
	if err := sn.connect(c); err != nil {
		t.Fatalf("connect: %v", err)
	}
	registerRoundTrip(t, c)
}

func wantCurrent(t *testing.T, sn *shareNodes, c *Client, prefix, addr string) {
	t.Helper()
	if got := sn.currentPrefix(); got != prefix {
		t.Fatalf("当前节点 = %q，应为 %q", got, prefix)
	}
	if c.config.ServerAddr != addr {
		t.Fatalf("Client 地址 = %s，应为 %s", c.config.ServerAddr, addr)
	}
	if (c.node != nil) != (prefix != "") {
		t.Fatalf("节点模式 = %v，应为 %v", c.node != nil, prefix != "")
	}
}

// 没有列表时与没有这个功能时一样：连默认地址，不进节点模式。
func TestShareNodesNoListUsesDefaultAddr(t *testing.T) {
	def := startLiveNode(t)
	_, _, sn, c := shareNodesEnv(t, def.addr, "")
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "", def.addr)
}

// 列表过期、作废后同样退回默认地址：从节点模式退出来，按配置连默认地址。
func TestShareNodesLeavesNodeWhenListGone(t *testing.T) {
	def := startLiveNode(t)
	w := startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, def.addr, "")
	e.publish(m, 42, "", e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)

	e.setNow(dirtest.Epoch.Add(32 * 24 * time.Hour)) // 有效期 30 天，再过时钟误差 1 天
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "", def.addr)
}

// prefer-default：默认节点连得上就用它。
func TestShareNodesPreferDefault(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 1), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "D", d.addr)
	e.wantLogs(1, "已连接默认节点 node-D（D）")
}

// 默认节点连不上时其余候选并行建连，第一个连上的胜出；默认节点进退避。
func TestShareNodesDefaultDownFallsBack(t *testing.T) {
	d, w := deadNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)
	e.wantLogs(1, "默认节点 node-D（D） 连接失败，已改用节点 node-W（W）")
	if sn.backoff.Allowed("D") {
		t.Fatal("连不上的默认节点应进退避")
	}
}

// fastest：所有候选一起起跑，权重低的晚起跑，RTT 接近时权重高的胜出。
func TestShareNodesFastestPrefersWeight(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, directory.PolicyFastest, e.nodeEntry("D", d, directory.StatusActive, 1), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)
	e.wantLogs(1, "已选择节点 node-W（W）")
}

// 粘性：默认节点恢复后，断线重连仍回当前节点，不迁回。
func TestShareNodesStickyAfterDefaultRecovers(t *testing.T) {
	deadD, w := deadNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, "", e.nodeEntry("D", deadD, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)

	d := startLiveNode(t)
	e.publish(m, 43, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	sn.backoff.Succeed("D")
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)
}

// 断线重连回当前节点时用最新列表里的指纹：节点换了证书、列表跟着更新后照样连得上（不断连、不换码）；
// 旧证书被吊销（列表里没有它的指纹了）后，地址上还出示旧证书就连不上。
func TestShareNodesReconnectUsesLatestFingerprints(t *testing.T) {
	w := startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, "", e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)

	w.stop()
	cert, key := newCertPair(t)
	startRelay(t, w.addr, "", cert, key)
	rotated := liveNode{addr: w.addr, fp: certFP(t, cert, key)}
	e.publish(m, 43, "", e.nodeEntry("W", rotated, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)

	e.publish(m, 44, "", e.nodeEntry("W", liveNode{addr: w.addr, fp: strings.Repeat("cd", 32)}, directory.StatusActive, 100))
	c.Close()
	err := sn.connect(c)
	var mm *CertMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("connect = %v，指纹已不在列表里，应报证书不符", err)
	}
}

// 当前节点连续失败 3 次才重新选；ID 冲突不算失败。
func TestShareNodesRepickAfterThreeFailures(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), w.addr)
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)
	sn.registered(w.addr)

	for range 3 {
		sn.failed(&registerRejected{Code: proto.ErrCodeClientIDConflict})
	}
	if sn.currentPrefix() != "W" {
		t.Fatal("ID 冲突不该算节点失败")
	}

	w.stop()
	for i := 1; i <= shareNodeFailLimit; i++ {
		c.Close()
		err := sn.connect(c)
		if err == nil {
			t.Fatalf("第 %d 次：停掉的节点不该连上", i)
		}
		sn.failed(err)
		if i < shareNodeFailLimit && sn.currentPrefix() != "W" {
			t.Fatalf("第 %d 次失败后就换了节点", i)
		}
	}
	if sn.currentPrefix() != "" {
		t.Fatalf("连续 %d 次失败后应重新选节点", shareNodeFailLimit)
	}
	e.wantLogs(1, "节点 node-W（W） 连续 3 次连接或注册失败，重新选择节点")
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "D", d.addr)
}

// 当前节点被停用、从列表里消失时马上重选，不等连续失败。
func TestShareNodesRepickWhenCurrentGone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update func(e *dirEnv, d, w liveNode) []directory.NodeEntry
		reason string
	}{
		{"停用", func(e *dirEnv, d, w liveNode) []directory.NodeEntry {
			return []directory.NodeEntry{e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusDisabled, 100)}
		}, "已停用"},
		{"消失", func(e *dirEnv, d, w liveNode) []directory.NodeEntry {
			return []directory.NodeEntry{e.nodeEntry("D", d, directory.StatusActive, 100)}
		}, "已不在节点列表里"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, w := startLiveNode(t), startLiveNode(t)
			e, m, sn, c := shareNodesEnv(t, deadAddr(t), w.addr)
			e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
			connectNode(t, sn, c)
			wantCurrent(t, sn, c, "W", w.addr)

			e.publish(m, 43, "", tc.update(e, d, w)...)
			connectNode(t, sn, c)
			wantCurrent(t, sn, c, "D", d.addr)
			e.wantLogs(1, "当前节点 node-W（W） "+tc.reason+"，重新选择节点")
		})
	}
}

// 列表把当前节点标成排空时，断线重连照样回去（relay 接受复用原码的重连），等协助端时再迁走。
func TestShareNodesDrainingStaysOnReconnect(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), w.addr)
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	e.publish(m, 43, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusDraining, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)
}

// relay 拒绝新注册（排空、停用）时不等连续失败，马上换节点，它进退避。
func TestShareNodesRejectedRepicksAtOnce(t *testing.T) {
	for _, code := range []string{proto.ErrCodeNodeDraining, proto.ErrCodeNodeDisabled} {
		t.Run(code, func(t *testing.T) {
			d, w := startLiveNode(t), startLiveNode(t)
			e, m, sn, c := shareNodesEnv(t, deadAddr(t), w.addr)
			e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
			connectNode(t, sn, c)
			sn.failed(&registerRejected{Code: code, Message: "x"})
			if sn.currentPrefix() != "" || sn.backoff.Allowed("W") {
				t.Fatal("被拒绝新注册的节点应马上放弃并进退避")
			}
			e.wantLogs(1, "节点 node-W（W）", "不接受新注册，改选其他节点")
			connectNode(t, sn, c)
			wantCurrent(t, sn, c, "D", d.addr)
		})
	}
}

// --relay-hint：第一次选节点先试升级前所在的节点（即使它不是默认节点），只试这一次。
func TestShareNodesHintFirstPickOnly(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), w.addr)
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)
	e.wantLogs(1, "已回到升级前所在的节点 node-W（W）")

	sn.failed(&registerRejected{Code: proto.ErrCodeNodeDraining})
	sn.backoff.Succeed("W")
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "D", d.addr) // 重选时不再先试 hint
}

// hint 不是列表里的地址时不连它：自动模式只连列表里的节点。
func TestShareNodesHintNotInList(t *testing.T) {
	other, d := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), other.addr)
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "D", d.addr)
}

// 本机专用地址连不上一次就从缓存删掉，按固定格式提示，然后改用列表里的地址。
func TestShareNodesDropsDeadLocalAddr(t *testing.T) {
	d := startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100))
	local := deadAddr(t)
	e.setLocalAddr("D", local)
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "D", d.addr)
	e.wantLogs(1, "[relay] 本机专用地址 "+local+" 连接失败（refused），已从缓存删除，改用节点列表（默认节点优先）。如需固定使用，请设置 REMOTE_RELAY_SERVER。")
	v, cache := m.Current()
	if got := cache.LocalAddr(v, "D"); got != "" {
		t.Fatalf("连不上的本机专用地址没删: %q", got)
	}
}

// 本机专用地址上的 relay 说本机被封：地址本身没坏，被封的是本机，同一台 relay 的哪个地址都一样被拒。
// 不能当连不上删掉，否则解封后还得再用 --server 引导一次。
func TestShareNodesKeepsLocalAddrWhenBlocked(t *testing.T) {
	d := startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100))
	local := freePort(t)
	blockedTCP(t, local)
	e.setLocalAddr("D", local)
	c.Close()
	if err := sn.connect(c); !errors.Is(err, proto.ErrBlocked) {
		t.Fatalf("connect = %v，应是被封", err)
	}
	e.wantLogs(0, "已从缓存删除")
	v, cache := m.Current()
	if got := cache.LocalAddr(v, "D"); got != local {
		t.Fatalf("被封时本机专用地址 = %q，不该删（原为 %q）", got, local)
	}
}

// 能连上的本机专用地址最先用。
func TestShareNodesUsesLocalAddr(t *testing.T) {
	d := startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	// 列表里的地址连不上；本机专用地址上是同一个节点（证书指纹一致）。
	e.publish(m, 42, "", e.nodeEntry("D", liveNode{addr: deadAddr(t), fp: d.fp}, directory.StatusActive, 100))
	e.setLocalAddr("D", d.addr)
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "D", d.addr)
}

// 列表地址出示的证书与列表不符：警告可能被劫持，当作连不上，改用别的节点。
func TestShareNodesCertMismatchWarns(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	otherCert, otherKey := newCertPair(t)
	wrong := liveNode{addr: d.addr, fp: certFP(t, otherCert, otherKey)} // D 的地址上出示的证书不是列表里写的
	e.publish(m, 42, "", e.nodeEntry("D", wrong, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)
	e.wantLogs(1, "警告：节点 node-D（D） 的地址 "+d.addr+" 出示的证书与节点列表不符", "可能被劫持")
}

// 所有节点都在退避期内时不拨号，直接报没有可用节点。
func TestShareNodesAllBackedOff(t *testing.T) {
	d := startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100))
	sn.backoff.Fail("D")
	c.Close()
	if err := sn.connect(c); !errors.Is(err, errNoNodes) {
		t.Fatalf("connect = %v，应为 errNoNodes", err)
	}
}

// 没有列表时注册在默认地址上，之后拿到了列表、默认地址正是列表里的节点：当作当前节点，不重新选
// （fastest 下重新选会选到权重更高的 W，换码）。
func TestShareNodesUnlistedAddrBecomesCurrent(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, d.addr, "")
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "", d.addr)
	sn.registered(d.addr)

	e.publish(m, 42, directory.PolicyFastest, e.nodeEntry("D", d, directory.StatusActive, 1), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "D", d.addr)
}

// 排空迁移：当前节点被列表标成排空时拨别的节点，连上了留给下一次 connect 接管。
func TestShareNodesMigrateOnListDraining(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), w.addr)
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	if sn.migrate(false, c.peerFingerprint()) {
		t.Fatal("当前节点正常时不该迁移")
	}

	e.publish(m, 43, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusDraining, 100))
	if !sn.migrate(false, c.peerFingerprint()) {
		t.Fatal("当前节点排空时应迁到别的节点")
	}
	e.wantLogs(1, "当前节点 node-W（W） 正在排空，已改用节点 node-D（D）")
	d.stop() // 已经连上的不用再拨：接管的就是迁移时建好的连接
	c.Close()
	if err := sn.connect(c); err != nil {
		t.Fatal(err)
	}
	wantCurrent(t, sn, c, "D", d.addr)
}

// 注册响应说当前节点在排空（列表还说它启用）时同样迁移。
func TestShareNodesMigrateOnRelayDraining(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), w.addr)
	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	if !sn.migrate(true, c.peerFingerprint()) {
		t.Fatal("relay 说排空时应迁移")
	}
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "D", d.addr)
}

// 不在列表节点上（没有列表时在默认地址 D 上注册，之后才有列表）时按当前连接的证书认出所在节点：D 正常不迁移，
// relay 说 D 在排空就迁走。认不出（没有指纹）不迁移。
func TestShareNodesUnlistedMigrateByCert(t *testing.T) {
	d, w := startLiveNode(t), startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, d.addr, "")
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "", d.addr)
	sn.registered(d.addr)

	e.publish(m, 42, "", e.nodeEntry("D", d, directory.StatusActive, 100), e.nodeEntry("W", w, directory.StatusActive, 100))
	if sn.migrate(true, "") {
		t.Fatal("认不出所在节点时不该迁移")
	}
	if sn.migrate(false, c.peerFingerprint()) {
		t.Fatal("所在节点正常时不该迁移")
	}
	if !sn.migrate(true, c.peerFingerprint()) {
		t.Fatal("relay 说所在节点 D 在排空时应迁走")
	}
	e.wantLogs(1, "当前节点 node-D（D） 正在排空，已改用节点 node-W（W）")
	connectNode(t, sn, c)
	wantCurrent(t, sn, c, "W", w.addr)
}

// 没有别的节点可去时留在原节点，提示只打一次；排空解除后再排空会重新提示。
func TestShareNodesMigrateNowhere(t *testing.T) {
	w := startLiveNode(t)
	e, m, sn, c := shareNodesEnv(t, deadAddr(t), "")
	e.publish(m, 42, "", e.nodeEntry("W", w, directory.StatusActive, 100))
	connectNode(t, sn, c)
	for range 2 {
		if sn.migrate(true, c.peerFingerprint()) {
			t.Fatal("没有别的节点时不该迁移")
		}
	}
	e.wantLogs(1, "当前节点 node-W（W） 正在排空，但暂时连不上其他节点")
	sn.migrate(false, c.peerFingerprint())
	sn.migrate(true, c.peerFingerprint())
	e.wantLogs(2, "当前节点 node-W（W） 正在排空，但暂时连不上其他节点")
	wantCurrent(t, sn, c, "W", w.addr)
}

// 胜者出来之后才连上的那几路要关掉，不能泄漏。
func TestCloseLateNodes(t *testing.T) {
	results := make(chan raceResult, 3)
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	results <- raceResult{r: &nodeConn{Conn: a1}}
	results <- raceResult{err: errors.New("x")}
	results <- raceResult{r: &nodeConn{Conn: b1}}
	closeLateNodes(results, 3)
	for _, peer := range []net.Conn{a2, b2} {
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("晚到的连接没有被关掉: %v", err)
		}
	}
}
