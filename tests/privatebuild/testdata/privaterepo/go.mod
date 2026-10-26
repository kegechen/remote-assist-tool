// 私有模块仓库的测试替身：独立 Go 模块，引用公共核心 github.com/remote-assist/tool，
// 用源码组装出带私有模块的 relay。测试运行时复制到临时目录，并把 replace 改写为公共仓库的绝对路径；
// 真实私有仓库应 require 公共核心的固定 tag，而不是 replace。
module example.com/private/relay-modules

go 1.21

require github.com/remote-assist/tool v0.0.0-00010101000000-000000000000

replace github.com/remote-assist/tool => ../../../..
