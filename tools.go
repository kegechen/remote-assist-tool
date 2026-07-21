//go:build tools

// 本文件只为把构建期工具钉进 go.mod/go.sum：go build 不会编译它（tools tag 挡着），
// 但 go mod 会因这条 import 记录版本，从而 `go mod download` 一次之后离线也能构建
// （工具进本地 module cache）。没有它，go run pkg@version 每次都要联网解析版本。
package tools

import _ "github.com/josephspurrier/goversioninfo/cmd/goversioninfo"
