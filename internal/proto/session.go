package proto

// Session 一次工具通道握手的产物：会话密钥 + 协商出的协议版本。
//
// 为什么把两者绑在一起传：v1 与 v2 的差别全是「这条密文要不要带 AAD、这个字段允不允许
// 为空」，全都发生在用 key 做 Seal/Open 的那一行。若版本另走一条路（全局常量、或各自
// 存一个字段），就会出现「用 v2 的 key 配 v1 的 AAD」这种撕裂——表现是每条请求
// decrypt_failed，且从日志里看不出是版本错配还是密钥错配。绑成一个值之后，
// SwapConn / RotateKey 这些原子替换天然把两者一起换掉。
//
// 零值是 fail-safe 的：Version == "" 一律按当前最高版本（最严）解释，而不是按最宽松的
// v1。漏传版本的后果因此是「连不上」而不是「静默降级成无认证通道」。

type Session struct {
	Key [32]byte
	// Version 协商结果，取值见 SupportedToolVersions。空串视作 ToolProtocolVersion。
	Version string
}

// Active 报告握手是否已完成。零 key 表示尚未协商出密钥，此时所有加解密都跳过
// （测试与握手前的窗口会出现）。
func (s Session) Active() bool { return s.Key != [32]byte{} }

// effectiveVersion 把空串归一到最高版本。所有开关都经由它，保证零值最严。
func (s Session) effectiveVersion() string {
	if s.Version == "" {
		return ToolProtocolVersion
	}
	return s.Version
}

// Legacy 报告本会话是否降级到了 v1。仅用于打日志/提示；行为判断请用下面几个语义开关，
// 它们表达的是「要求什么」而不是「对端是谁」。
func (s Session) Legacy() bool { return s.effectiveVersion() == ToolProtocolVersionV1 }

// 注意这里没有 RequireSealedArgs：空 args 在**两个版本**下都被拒。真实 v1 的判据是
// len > 0，但那条"缺 args 字段就跳过解密"的分支合法客户端走不到（ToolReq.args 没有
// omitempty，序列化必定写出 "args":null），只有手写 JSON 的注入方造得出来。给它开
// 兼容口子等于专为攻击者保留一条免密钥执行工具的路，而兼容性收益是零。

// RequireSealedResp v2 起，每条响应都要封，包括 result 为空的错误响应——ok / error_code
// 这些明文字段唯一的认证依据就是它们进了密文的 AAD。
func (s Session) RequireSealedResp() bool { return !s.Legacy() }

// 注意这里没有 RequireSealedStream：流帧的空 Data 在**两个版本**下都按损坏处理。
// 0.0.x 的加封侧也是无条件 Seal（空 data 同样产出带 nonce+tag 的非空密文），且它从不发
// Fin 帧，所以 v1 线上不存在合法的空 Data 帧，不需要也不能给它开豁免——开了就等于
// 允许注入方用空帧抹平丢帧记录。

// RequireStreamTerminator v2 起，流式调用必须以一帧 Fin=true 收尾，接收侧据此区分
// 「真的没有更多输出」和「最后一帧刚好丢了」。
//
// v1 完全没有这个帧：chunkSink.Finish 是 v2 之后的 8873779 才加的，0.0.x 的 agent 里
// 搜不到任何 Fin 的发送点。所以对 v1 对端要求终止帧，等于让每一次流式调用（exec
// stream=true、tail_log follow）都在等满补齐窗口后以 stream_incomplete 收场——
// 而输出其实完整无缺。缺帧检测不受影响，Seq 在 v1 下照样连续，照查。
func (s Session) RequireStreamTerminator() bool { return !s.Legacy() }

// AntiReplay v2 起，接收侧按调用 ID 做滑动窗口去重。v1 的发送方不保证 ID 单调，开了会
// 误杀。
func (s Session) AntiReplay() bool { return !s.Legacy() }

// ReqAAD / RespAAD / StreamAAD 按版本产出 AAD：v2 绑定外层明文字段，v1 返回 nil。
//
// 之所以做成 Session 的方法而不是让调用点写 if：AEAD 的 Seal 与 Open 必须传**完全相同**
// 的 AAD，两边各写一个 if 就有写歪一处的机会，而写歪的表现是运行期 decrypt_failed，
// 编译器不会提醒。收到这里之后，调用点只有一种写法。
func (s Session) ReqAAD(id uint64, tool string, deadlineMs uint32) []byte {
	if s.Legacy() {
		return nil
	}
	return ToolReqAAD(id, tool, deadlineMs)
}

func (s Session) RespAAD(id uint64, ok bool, errorCode, errorMsg string) []byte {
	if s.Legacy() {
		return nil
	}
	return ToolRespAAD(id, ok, errorCode, errorMsg)
}

func (s Session) StreamAAD(id uint64, seq uint32, stream string, fin bool) []byte {
	if s.Legacy() {
		return nil
	}
	return StreamChunkAAD(id, seq, stream, fin)
}
