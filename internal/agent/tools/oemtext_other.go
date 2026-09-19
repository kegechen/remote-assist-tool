//go:build !windows

package tools

// 非 Windows 上没有"活动代码页"这回事：Linux/macOS 的 locale 基本都是 UTF-8，命令输出
// 不是合法 UTF-8 时就是二进制，没有可回退的编码。
//
// 于是 decodingWriter 在这些平台上第一次进入代码页模式就会拿到 ok=false，转入
// passthrough 原样透传——行为与引入这一层之前逐字节相同，下游照旧按二进制处理。

func consoleTranscode(b []byte) ([]byte, bool) { return nil, false }

func splitTrailingPartial(b []byte) (head, tail []byte) { return b, nil }
