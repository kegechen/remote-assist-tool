package proto

import "encoding/binary"

// 工具通道的 AEAD 附加认证数据（AAD）
//
// 为什么需要：ToolReq 里只有 args 是密文，Tool / ID / DeadlineMs 都是外层明文 JSON，
// 既不加密也不参与认证。于是拿到一条密文的攻击者可以把它改挂到别的工具上——捕获
// 一条 read_file{path:X} 的 args 密文，把 tool 改成 "write_file" 重发，
// WriteFileArgs 解出 Content=nil / At=nil / Create=false，tools/file.go 走
// O_WRONLY|O_TRUNC 把 X 截断成 0 字节。攻击者读不到响应（响应仍用它不知道的 key 加密），
// 但完整性已经被破坏。
//
// 把这些明文字段作为 AAD 传进 Seal/Open，密文就和「哪个工具、哪次调用、哪一帧」
// 绑死了，任何改挂都会让 Open 失败。
//
// 三个方向各有自己的标签，因此请求的密文也没法当成响应或流帧重放。

const (
	aadTagToolReq     = "req"
	aadTagToolResp    = "resp"
	aadTagStreamChunk = "chunk"
)

// aadBuilder 按「标签 + 长度前缀字段」拼 AAD。
//
// 用长度前缀而不是直接拼接，避免字段边界歧义：否则 tool="ab" + 后续字段 "c" 与
// tool="abc" 可能拼出同一串，绑定就形同虚设。
type aadBuilder struct {
	buf []byte
}

func newAADBuilder(tag string) *aadBuilder {
	b := &aadBuilder{buf: make([]byte, 0, 64)}
	return b.addString(tag)
}

func (b *aadBuilder) addString(s string) *aadBuilder {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.buf = append(b.buf, n[:]...)
	b.buf = append(b.buf, s...)
	return b
}

func (b *aadBuilder) addUint64(v uint64) *aadBuilder {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	b.buf = append(b.buf, n[:]...)
	return b
}

func (b *aadBuilder) addUint32(v uint32) *aadBuilder {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], v)
	b.buf = append(b.buf, n[:]...)
	return b
}

func (b *aadBuilder) addBool(v bool) *aadBuilder {
	var n byte
	if v {
		n = 1
	}
	b.buf = append(b.buf, n)
	return b
}

// ToolReqAAD 绑定 ToolReq 的全部明文字段。
func ToolReqAAD(id uint64, tool string, deadlineMs uint32) []byte {
	return newAADBuilder(aadTagToolReq).addUint64(id).addString(tool).addUint32(deadlineMs).buf
}

// ToolRespAAD 绑定 ToolResp 的全部明文字段。
//
// 只绑 id 是不够的：ok / error_code / error_msg 同样是外层明文，中间人把一次成功
// 翻成失败（或反过来）不需要任何密钥。绑进 AAD 之后，改动其中任何一个都会让结果
// 解密失败，接收侧据此把整条响应判为不可信。
func ToolRespAAD(id uint64, ok bool, errorCode, errorMsg string) []byte {
	return newAADBuilder(aadTagToolResp).addUint64(id).addBool(ok).addString(errorCode).addString(errorMsg).buf
}

// StreamChunkAAD 绑定流帧的全部明文字段。Seq 进 AAD 意味着重排或重放某一帧都会
// 解密失败，接收侧据此判定这次调用的输出不完整。
//
// fin 目前全链路恒为 false（字段留着没用起来），一并绑上是为了将来真启用时
// 「漏进 AAD」是编译期看得见的改动，而不是上线后一片解密失败。
func StreamChunkAAD(id uint64, seq uint32, stream string, fin bool) []byte {
	return newAADBuilder(aadTagStreamChunk).addUint64(id).addUint32(seq).addString(stream).addBool(fin).buf
}
