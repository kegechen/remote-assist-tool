package proto

import "errors"

// BlockedMessage 是 relay 拒绝被封禁来源（IP 或设备）时给用户看的唯一一句话，两端共用：
//   - relay 把它放进 JoinResponse.Error（设备被封）和 QUIC 的关闭原因（IP 被封）；
//   - TCP 上被封时还没有应用层通道，relay 改回 TLS 致命 alert access_denied，客户端同样
//     归为 ErrBlocked。
//
// 客户端识别出封禁后只显示这一句，不再附「请重新运行 / 重新 connect」：封禁期内重试无效。
const BlockedMessage = "访问受限，请联系管理员。"

// ErrBlocked 客户端识别出「被 relay 封禁」时返回的错误，Error() 即 BlockedMessage。
var ErrBlocked = errors.New(BlockedMessage)
