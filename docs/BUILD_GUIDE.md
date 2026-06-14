# 构建和测试指南

## 前置条件

1. **安装 Go 1.21 或更高版本**
   - 下载: https://golang.org/dl/
   - 安装后验证: `go version`

2. **Git (可选)**
   - 用于版本管理

## 快速构建

### Windows

```cmd
# 方式1: 使用构建脚本
build.bat

# 方式2: 手动构建
mkdir bin
go build -o bin/relay.exe ./cmd/relay
go build -o bin/remote.exe ./cmd/remote
```

### Linux / macOS

```bash
# 方式1: 使用构建脚本
chmod +x build.sh
./build.sh

# 方式2: 手动构建
mkdir -p bin
go build -o bin/relay ./cmd/relay
go build -o bin/remote ./cmd/remote
```

## 运行测试

### 单元测试

```bash
# 运行所有单元测试
go test -v ./internal/...

# 运行特定包的测试
go test -v ./internal/relay
go test -v ./internal/proto

# 运行带竞态检测的测试
go test -race -v ./internal/...

# 生成覆盖率报告
go test -coverprofile=coverage.out ./internal/...
go tool cover -html=coverage.out
```

### 快速测试验证

如果Go已安装，可以运行以下命令验证代码可编译：

```bash
# 只编译不运行
go build ./internal/...
go build ./cmd/relay
go build ./cmd/remote
```

## 手动功能测试

### 1. 启动中继服务器

```bash
# 生成自签名证书并启动
bin/relay --listen :8443 --gen-certs --certs-dir ./certs

# 或者先生成证书，再启动
bin/relay --gen-certs --certs-dir ./certs
bin/relay --listen :8443 --cert ./certs/server.crt --key ./certs/server.key
```

### 2. 被协助端 (Share)

在另一个终端:

```bash
bin/remote share --server localhost:8443 --insecure
```

会显示类似:
```
协助码已生成: ABCD-EFGHIJ
有效期至: 2026-02-28 18:30:00

等待协助端连接...
```

### 3. 协助端 (Help)

在第三个终端:

```bash
bin/remote help --server localhost:8443 --code ABCD-EFGHIJ --insecure
```

### 4. SSH 连接

在第四个终端:

```bash
# 如果被协助端有SSH服务器
ssh -p 2222 user@127.0.0.1
```

## 测试清单

### 单元测试检查
- [ ] `go test ./internal/relay` - 通过
- [ ] `go test ./internal/proto` - 通过
- [ ] `go test -race ./internal/...` - 通过

### 功能测试检查
- [ ] 服务器可以启动
- [ ] Share模式可以生成协助码
- [ ] Help模式可以使用有效码连接
- [ ] 审计日志正常记录

### 安全测试检查
- [ ] 协助码格式正确（无易混淆字符）
- [ ] 过期码无法使用
- [ ] 无效码返回清晰错误

## 故障排除

### 问题: `go: command not found`

**解决方案**: 确保Go已安装并添加到PATH

Windows:
```cmd
set PATH=%PATH%;C:\Program Files\Go\bin
```

Linux/macOS:
```bash
export PATH=$PATH:/usr/local/go/bin
```

### 问题: 证书验证失败

**解决方案**: 使用 `--insecure` 标志（仅限开发环境）
```bash
bin/remote share --server localhost:8443 --insecure
```

### 问题: 端口被占用

**解决方案**: 使用其他端口
```bash
bin/relay --listen :9443
bin/remote share --server localhost:9443 --insecure
```

## 项目结构

```
remote-assist-tool/
├── cmd/
│   ├── relay/          # 中继服务器
│   │   └── main.go
│   └── remote/         # 统一客户端
│       └── main.go
├── internal/
│   ├── relay/          # 中继逻辑
│   │   ├── server.go
│   │   ├── session.go
│   │   ├── code.go
│   │   ├── tunnel.go
│   │   └── *_test.go   # 测试
│   ├── client/         # 客户端逻辑
│   │   ├── client.go
│   │   ├── share.go
│   │   └── help.go
│   ├── proto/          # 协议定义
│   │   ├── message.go
│   │   └── message_test.go
│   ├── crypto/         # TLS配置
│   │   └── tls.go
│   └── logger/         # 审计日志
│       └── audit.go
└── tests/              # 集成测试
```
