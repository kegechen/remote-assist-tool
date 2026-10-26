// Package future 模拟要求更新核心版本的私有模块：核心为正式版本且更旧时 relay 拒绝启动。
package future

import (
	"context"

	"github.com/remote-assist/tool/pkg/relay/module"
)

func init() {
	module.RegisterBuiltin(func() module.Module { return &future{} })
}

type future struct{}

func (*future) Descriptor() module.Descriptor {
	return module.Descriptor{ID: "future", Version: "3.0.0", Source: "example.com/private/relay-modules", APIVersion: module.APIVersion, MinCoreVersion: "v99.0.0"}
}
func (*future) Validate(module.Config) error                      { return nil }
func (*future) Start(context.Context, module.ModuleContext) error { return nil }
func (*future) Stop(context.Context) error                        { return nil }
func (*future) Health() module.Health                             { return module.Health{Status: module.HealthOK} }
