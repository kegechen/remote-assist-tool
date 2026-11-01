package p2p

// NAT 类型常量
const (
	NATUnknown   = ""          // 未检测
	NATOpen      = "open"      // 公网直连（无 NAT）
	NATCone      = "cone"      // 锥型 NAT（NAT1/2/3）— 打洞友好
	NATSymmetric = "symmetric" // 对称型 NAT（NAT4）— 需要生日攻击
)
