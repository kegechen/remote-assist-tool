// Package demo 是私有内置模块的测试替身：在 init 中注册到公共核心的内置注册表。
package demo

import (
	"context"

	"github.com/remote-assist/tool/pkg/relay/module"
)

func init() {
	module.RegisterBuiltin(func() module.Module { return &demo{} })
}

type demo struct{}

type config struct {
	Greeting string `json:"greeting"`
}

func (*demo) Descriptor() module.Descriptor {
	return module.Descriptor{
		ID:             "demo",
		Version:        "1.2.0",
		Source:         "example.com/private/relay-modules",
		APIVersion:     module.APIVersion,
		MinCoreVersion: "v1.0.0",
	}
}

func (*demo) Validate(cfg module.Config) error {
	var c config
	return cfg.Decode(&c)
}

func (*demo) Start(ctx context.Context, mc module.ModuleContext) error {
	var c config
	if err := mc.Config().Decode(&c); err != nil {
		return err
	}
	mc.Logf("private demo module started: %s (core %s)", c.Greeting, mc.Shared().CoreVersion)
	return nil
}

func (*demo) Stop(ctx context.Context) error { return nil }

func (*demo) Health() module.Health { return module.Health{Status: module.HealthOK} }
