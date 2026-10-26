// Package legacy 模拟按不受支持的模块契约版本编写的私有模块：注册失败，relay 拒绝启动。
package legacy

import (
	"context"

	"github.com/remote-assist/tool/pkg/relay/module"
)

func init() {
	module.RegisterBuiltin(func() module.Module { return &legacy{} })
}

type legacy struct{}

func (*legacy) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "legacy", Version: "0.9.0", Source: "example.com/private/relay-modules", APIVersion: 99}
}
func (*legacy) Validate(module.Config) error                      { return nil }
func (*legacy) Start(context.Context, module.ModuleContext) error { return nil }
func (*legacy) Stop(context.Context) error                        { return nil }
func (*legacy) Health() module.Health                             { return module.Health{Status: module.HealthOK} }
