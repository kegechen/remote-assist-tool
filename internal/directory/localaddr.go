package directory

import (
	"encoding/json"
	"fmt"
)

// 本机专用地址（spec 6.5）：用户显式连上地址 A，A 处 relay 出示的证书指纹在列表里节点 N 的 tls_fps 中，
// 就把 A 记为 N 的本机专用地址（内网地址、Tailscale 地址等），自动模式连 N 时先试它。
//
// 它存在缓存文件的顶层字段 local_addrs 里（前缀 → host:port）。内容不被信任：自动模式拨号时仍按列表里的
// tls_fps 严格核对证书，所以文件被改过最多让连接失败（失败一次就删），骗不到协助码。自动模式 MCP 进程里
// 带 server 连这个地址时，节点 N 的证书免 TOFU（spec 6.1）：放行的也只是列表里签过名的 N，改得了这个文件的
// 本来也改得了指纹表。

// localAddrs 是 local_addrs 的取值。读入时只留下合法的条目，写坏的部分整个丢掉、不报错：
// 缓存里还记着列表位置与吊销，因为这点附带数据写坏就整体判成损坏（之后所有更新被拒），得不偿失。
type localAddrs map[string]string

func (m *localAddrs) UnmarshalJSON(data []byte) error {
	*m = nil
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return nil // 不是对象（字符串、数组、null……）：当作没有
	}
	for k, v := range raw {
		var addr string
		if !ValidPrefix(k) || json.Unmarshal(v, &addr) != nil || ValidAddr(addr) != nil {
			continue
		}
		if *m == nil {
			*m = make(localAddrs)
		}
		(*m)[k] = addr
	}
	return nil
}

// LocalAddr 返回节点 prefix 的本机专用地址。节点不在 v 里（v 为 nil、节点已从列表消失或被丢弃）时返回空：
// 只在列表里还有这个节点时才使用，节点回到列表后记下的地址仍在。
func (c *Cache) LocalAddr(v *Verified, prefix string) string {
	if c == nil {
		return ""
	}
	// 用列表里节点自己的前缀查表：NodeByPrefix 不分大小写，查表也得一致，
	// 否则拿用户输入的小写码首位来查会悄悄查不到。
	n := v.NodeByPrefix(prefix)
	if n == nil {
		return ""
	}
	return c.LocalAddrs[n.Prefix]
}

// SetLocalAddr 记下节点 prefix 的本机专用地址，返回是否有变化。前缀必须是码字符集里的大写字符，
// 地址必须是合法的 host:port（与节点证书里的地址同一套规则），否则拒绝、不改动。
func (c *Cache) SetLocalAddr(prefix, addr string) (bool, error) {
	if !ValidPrefix(prefix) {
		return false, fmt.Errorf("%w: local address prefix %q", ErrMalformed, prefix)
	}
	if err := ValidAddr(addr); err != nil {
		return false, err
	}
	if c.LocalAddrs[prefix] == addr {
		return false, nil
	}
	if c.LocalAddrs == nil {
		c.LocalAddrs = make(localAddrs)
	}
	c.LocalAddrs[prefix] = addr
	return true, nil
}

// DeleteLocalAddr 删除节点 prefix 的本机专用地址，但只在当前记的就是 addr（刚才连不上的那个）时才删，返回是否删了。
// 多个进程共用缓存文件：长时间运行的 share 读到的是旧地址，期间别的进程可能已把这个节点改记为新地址，
// 按前缀删会把刚记下的、能用的地址一起删掉。
func (c *Cache) DeleteLocalAddr(prefix, addr string) bool {
	if cur, ok := c.LocalAddrs[prefix]; !ok || cur != addr {
		return false
	}
	delete(c.LocalAddrs, prefix)
	return true
}
