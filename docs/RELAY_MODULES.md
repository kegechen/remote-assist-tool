## relay 模块化扩展

relay 的扩展模块在构建期以 Go 源码注册到同一个可执行文件中。公共核心不加载动态库、运行时插件，
也不为模块启动独立进程；没有注册模块时，`cmd/relay` 仍可独立构建、启动和运行。

### 公共 API

| 包 | 用途 |
| --- | --- |
| `github.com/remote-assist/tool/pkg/relay/module` | `Module`、`Descriptor`、`Config`、`ModuleContext`、`Health`、`RegisterBuiltin` 和契约版本 |
| `github.com/remote-assist/tool/pkg/relayapp` | relay 程序入口 `Main()`，包含参数、服务生命周期和模块框架接线 |

模块只依赖 `pkg/relay/module`。`ModuleContext` 提供日志、取消信号和共享只读信息，模块不直接访问
relay 内部连接、会话或服务器实现。

### 实现模块

模块实现 `Descriptor`、`Validate`、`Start`、`Stop` 和 `Health`，并在其构建入口导入后注册构造器：

```go
package example

import (
	"context"

	"github.com/remote-assist/tool/pkg/relay/module"
)

func init() { module.RegisterBuiltin(func() module.Module { return &exampleModule{} }) }

type exampleModule struct{}

func (*exampleModule) Descriptor() module.Descriptor {
	return module.Descriptor{
		ID: "example", Version: "1.0.0", Source: "example.com/team/relay-modules",
		APIVersion: module.APIVersion,
	}
}

func (*exampleModule) Validate(module.Config) error                      { return nil }
func (*exampleModule) Start(context.Context, module.ModuleContext) error { return nil }
func (*exampleModule) Stop(context.Context) error                         { return nil }
func (*exampleModule) Health() module.Health                              { return module.Health{Status: module.HealthOK} }
```

构建入口调用 `relayapp.Main()`。`cmd/relay` 是无扩展模块的参考入口；其他入口可以通过 Go 的空导入
引入模块包，再调用同一个 `Main()`。

### 兼容性和配置

注册时校验模块 ID、来源、版本、依赖和 `Descriptor.APIVersion`。核心版本要求和依赖版本在启动及
配置校验时检查；不兼容的注册会阻止 relay 启动。`--list-modules` 列出模块元数据和校验结果，
发现不兼容时返回非零退出码。

`--module-config` 指向 JSON 配置文件。配置可声明顶层 `version`、模块键及其 `require` 约束；未知模块、
来源或版本不匹配会拒绝该份配置并保留当前 relay 服务，修正文件后监听器会自动重试。

### 验证

`pkg/relay/module` 覆盖契约、注册表、依赖、生命周期和兼容性；`pkg/relayapp` 覆盖参数、加载、热加载、
停止和错误处理；`tests/privatebuild` 使用独立 Go 模块夹具验证无模块构建、构建期注册和不兼容构建的
失败路径。
