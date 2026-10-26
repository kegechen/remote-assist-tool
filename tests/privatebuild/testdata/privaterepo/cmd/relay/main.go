// 私有仓库的 relay 组装入口：空导入私有模块包（init 中注册），然后运行公共核心的 relay。
package main

import (
	_ "example.com/private/relay-modules/demo"

	"github.com/remote-assist/tool/pkg/relayapp"
)

func main() {
	relayapp.Main()
}
