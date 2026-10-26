# 构建和测试指南

## 前置条件

1. 安装 Go 1.21 或更高版本，并确认 `go version` 可执行。
2. 安装 Git（构建脚本用 `git describe` 写入版本信息）。
3. Windows 构建脚本还会生成 VERSIONINFO 资源；首次构建时 Go 需要下载 `goversioninfo` 工具依赖。

## 快速构建

构建脚本在 `scripts/` 下，从任何目录调用都会自动切到仓库根目录执行：

### Windows

```cmd
scripts\build.bat
```

### Linux / macOS

```bash
chmod +x scripts/build.sh
./scripts/build.sh
```

本脚本只构建 relay；外部业务模块在独立仓库组装，公共 release 只包含 relay。

```text
bin/remote-assist-relay-<os>-<arch>[.exe]
```

版本号来自 `git describe --tags --always --dirty`。如果只需要临时本机构建，也可以直接指定输出文件：

```bash
mkdir -p bin
go build -o bin/remote-assist-relay-$(go env GOOS)-$(go env GOARCH) ./cmd/relay
```

## 自动检查

```bash
# 格式、静态检查和全部测试
gofmt -l cmd internal tests
go vet ./...
go test ./... -count=1

# 需要 C 编译器和 cgo；本机不满足时交给 CI
go test -race ./...

# 覆盖率
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

`go build ./...` 可作为不运行程序的编译检查。跨平台发布时为每个目标设置 `GOOS`、`GOARCH`，并使用脚本或相同的 `go build -o` 规则生成带平台后缀的文件名。

## 手动功能测试

下面示例使用手动生成的 `bin/relay` 文件名；如果使用脚本构建，请替换为实际的 `remote-assist-relay-*` 产物。

### 1. 启动 relay

```bash
mkdir -p certs
# relay 会在 certs/server.crt 和 server.key 不存在时自动生成自签证书
bin/relay run --listen :18443 --certs-dir ./certs --audit ./audit.log
```

也可以先单独生成证书：

```bash
bin/relay run --gen-certs --certs-dir ./certs
bin/relay run --listen :18443 --cert ./certs/server.crt --key ./certs/server.key --audit ./audit.log
```

### 2. 被协助端（Share）

在第二个终端运行：

```bash
bin/remote share --server localhost:18443 --insecure
```

程序会显示协助码和有效期，并等待协助端连接。这里的 `--server localhost:18443` 是因为本节启动了本地 relay；如果不启动本地 relay，省略 `--server` 即可使用内置公共 relay `23.95.78.14:8443`。`--insecure` 仅用于本地自签证书测试。

### 3. 协助端（Help）

MCP 工具模式需要把 `--mcp-stdio` 接到 MCP 客户端；只验证传统 SSH 隧道时，在第三个终端运行：

```bash
bin/remote help --server localhost:18443 --code <协助码> --legacy-ssh --insecure
```

成功后，help 在本机监听默认的 `127.0.0.1:2222`，可用 SSH 客户端连接：

```bash
ssh -p 2222 user@127.0.0.1
```

### 4. MCP smoke test

在支持 MCP 的客户端中，把 `bin/remote help --server localhost:18443 --mcp-stdio --insecure` 配成 stdio MCP server，再调用 `connect` 并传入 share 显示的协助码。工具列表和参数以 `README.md` 的 MCP 章节为准。

## 测试清单

### 编译与单元测试

- [ ] `go build ./...` 通过
- [ ] `go vet ./...` 通过
- [ ] `go test ./... -count=1` 通过
- [ ] 在具备 cgo/C 编译器的环境运行 `go test -race ./...`

### 功能测试

- [ ] relay 启动并监听预期端口
- [ ] share 生成协助码，help 能使用有效码连接
- [ ] `--legacy-ssh` 模式能建立本地 SSH 转发
- [ ] MCP 模式能完成 `connect` 和至少一个只读工具调用
- [ ] 无效码和过期码返回清晰错误
- [ ] relay 审计日志记录连接和拒绝事件

### 安全测试

- [ ] 协助码使用无易混淆字符的随机格式
- [ ] 生产客户端使用 `--insecure=false` 或 `--ca`
- [ ] 公网 relay 不使用 `--plain` 或 `--no-auth`
- [ ] 配置 `--root` 时明确它只是文件工具防手滑护栏，不能限制 exec

## 故障排除

### `go: command not found`

确认 Go 已安装并加入 PATH：

```powershell
$env:Path += ';C:\Program Files\Go\bin'
```

Linux/macOS：

```bash
export PATH="$PATH:/usr/local/go/bin"
```

### 证书验证失败

本地自签证书测试可在客户端加 `--insecure`。生产环境应配置受信 CA，并使用 `--insecure=false` 或 `--ca <file>`；不要用 `--plain` 绕过 TLS。

### 端口被占用

为 relay 和 help 使用其他端口：

```bash
bin/relay run --listen :19443
bin/remote share --server localhost:19443 --insecure
```

## 项目结构

```text
remote-assist-tool/
├── cmd/
│   ├── relay/       # relay 服务端和 Windows 服务入口
│   ├── remote/      # share/help 客户端和 MCP 入口
├── internal/
│   ├── agent/       # 远端工具 daemon 和工具实现
│   ├── client/      # share/help 会话与传输
│   ├── crypto/      # TLS、TOFU 和工具通道加密
│   ├── logger/       # 审计日志
│   ├── mcp/         # MCP stdio server
│   ├── p2p/         # STUN、打洞与 UDP relay
│   ├── proto/       # relay 和工具通道协议
│   ├── relay/       # TCP relay 与会话管理
│   └── version/     # 版本信息
└── tests/
    └── e2e/         # 端到端测试
```

## relay 模块化构建

`cmd/relay` 复用 `pkg/relayapp.Main()`，模块契约和注册表位于 `pkg/relay/module`。
需要扩展 relay 的构建入口可以导入模块包后调用同一个 `Main()`；模块会在构建期注册，
不使用动态库、运行时插件或独立进程。`--list-modules` 可检查编译进二进制的模块及兼容性，
`--module-config` 用于加载模块配置。客户端仍可手动构建，不属于 relay-only 发布产物。
