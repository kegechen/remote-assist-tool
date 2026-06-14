# 部署和测试说明

## 服务器已部署成功！

**服务器地址**: `192.168.1.101:8443`

**状态**: ✅ 运行中

---

## 快速开始测试

### 1. 使用 Windows 客户端 (已编译)

在本地 Windows 机器上：

```cmd
cd C:\Users\mike\src\remote-assist-tool\bin
```

#### 被协助端 (Share)
```cmd
remote.exe share --server 192.168.1.101:8443 --insecure
```

#### 协助端 (Help)
```cmd
remote.exe help --server 192.168.1.101:8443 --insecure --code <协助码>
```

---

### 2. 使用 Linux 客户端 (在服务器上)

```bash
ssh root@192.168.1.101
cd /root/remote-assist

# Share 模式
./remote-linux share --server 127.0.0.1:8443 --plain

# Help 模式
./remote-linux help --server 127.0.0.1:8443 --plain --code <协助码>
```

---

## 服务器管理

### 查看服务器状态
```bash
ssh root@192.168.1.101
cd /root/remote-assist

# 查看进程
ps aux | grep relay-linux

# 查看日志
cat relay.log

# 查看监听端口
netstat -tlnp | grep 8443
```

### 重启服务器
```bash
ssh root@192.168.1.101
cd /root/remote-assist

# 停止
kill $(cat relay.pid)

# 启动
nohup ./relay-linux --listen :8443 --cert ./certs/server.crt --key ./certs/server.key > relay.log 2>&1 &
echo $! > relay.pid
```

---

## 文件位置

### 本地 (Windows)
```
C:\Users\mike\src\remote-assist-tool\
├── bin\
│   ├── relay.exe       # Windows 服务器
│   ├── remote.exe      # Windows 客户端
│   ├── relay-linux     # Linux 服务器
│   └── remote-linux    # Linux 客户端
```

### 远程服务器
```
/root/remote-assist/
├── relay-linux         # 服务器程序
├── remote-linux        # 客户端程序
├── relay.pid           # 进程PID
├── relay.log           # 运行日志
├── audit.log           # 审计日志
└── certs/
    ├── server.crt      # TLS证书
    └── server.key      # TLS私钥
```

---

## 测试流程建议

### 测试 1: 基本连接
1. 终端 A: `remote.exe share --server 192.168.1.101:8443 --insecure`
2. 记录协助码，例如 `ABCD-EFGHIJ`
3. 终端 B: `remote.exe help --server 192.168.1.101:8443 --insecure --code ABCD-EFGHIJ`
4. 验证连接成功

### 测试 2: 协助码过期
1. 生成协助码，等待30分钟
2. 验证过期后无法使用

### 测试 3: 无效码处理
1. 尝试使用无效码连接
2. 验证返回友好错误提示

### 测试 4: 网络中断
1. 建立连接后，拔掉网线/中断网络
2. 验证程序优雅退出

---

## 故障排除

### 连接失败
- 确认防火墙已开放 8443 端口
- 检查服务器是否运行: `ps aux | grep relay`
- 查看日志: `cat /root/remote-assist/relay.log`

### 证书错误
- 使用 `--insecure` 跳过验证 (仅开发测试)
- 或配置正确的 CA 证书

### 需要重启服务器
```bash
ssh root@192.168.1.101
cd /root/remote-assist
kill $(cat relay.pid) 2>/dev/null
nohup ./relay-linux --listen :8443 --cert ./certs/server.crt --key ./certs/server.key > relay.log 2>&1 &
echo $! > relay.pid
```
