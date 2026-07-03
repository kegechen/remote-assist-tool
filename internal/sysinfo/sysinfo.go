// Package sysinfo 采集本机身份串（"user@host 系统 架构"），供 share/help 端
// 注册时上报给 relay，用于 web 看板区分连接来源。只用于展示：任一项采集
// 失败就跳过该项，绝不返回 error。
package sysinfo

import (
	"os"
	"os/user"
	"runtime"
	"strings"
)

// Summary 返回形如 "kechen12@LAPTOP-X1 Windows 11 amd64"、
// "uos@uos-PC uos arm64" 的身份串。
func Summary() string {
	parts := make([]string, 0, 3)
	if id := identity(); id != "" {
		parts = append(parts, id)
	}
	parts = append(parts, osName(), runtime.GOARCH)
	return strings.Join(parts, " ")
}

// identity 返回 "user@host"；缺一侧就只返回另一侧，两侧都缺返回 ""。
func identity() string {
	who := username()
	host, _ := os.Hostname()
	switch {
	case who != "" && host != "":
		return who + "@" + host
	case host != "":
		return host
	default:
		return who
	}
}

// username 取当前用户名。Windows 的 user.Current() 返回 "DOMAIN\user"，
// 只保留 user 段；失败时回落 USER / USERNAME 环境变量。
func username() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		name := u.Username
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			name = name[i+1:]
		}
		return name
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return os.Getenv("USERNAME")
}
