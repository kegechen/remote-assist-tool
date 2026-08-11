# Windows Relay 服务部署

Windows 版本的 relay 同时支持前台模式和原生 Windows 服务模式：

- 无参数双击：打开状态驱动的服务管理菜单。
- `run [relay options]`：在当前控制台前台运行。
- `service install|start|status|stop|uninstall`：非交互管理 Windows 服务。
- 由 Service Control Manager 启动：进入 Session 0 服务模式，不创建终端窗口。

服务名称和 Event Log 事件源均为 `RemoteAssistRelay`。

## 安装和管理

安装、启动、停止和卸载需要管理员权限。非管理员运行这些命令时，程序会通过 UAC 重新启动相同操作。状态查询不需要管理员权限。

```powershell
.\remote-assist-relay-windows-amd64.exe service install
.\remote-assist-relay-windows-amd64.exe service start
.\remote-assist-relay-windows-amd64.exe service status
.\remote-assist-relay-windows-amd64.exe service stop
.\remote-assist-relay-windows-amd64.exe service uninstall
```

安装结果：

| 内容 | 路径或设置 |
|---|---|
| 服务程序 | `C:\Program Files\RemoteAssistRelay\relay.exe` |
| 服务配置 | `C:\ProgramData\RemoteAssistRelay\config.json` |
| 自动证书 | `C:\ProgramData\RemoteAssistRelay\certs` |
| 审计日志 | `C:\ProgramData\RemoteAssistRelay\logs\audit.jsonl` |
| 服务账户 | `NT AUTHORITY\LocalService` |
| 启动方式 | 自动（延迟启动） |
| 故障恢复 | 5 秒、30 秒、60 秒后依次重启；24 小时无故障后清零 |

安装器会把当前 EXE 原子复制到固定安装目录。数据目录会禁用继承 ACL，仅保留 SYSTEM、Administrators 的完全控制权限和 LocalService 的修改权限，防止普通本地用户读取私钥或预创建服务文件。服务运行时 Windows 会保护正在映射的 EXE；服务停止后由 `Program Files` 的目录 ACL 防止普通用户修改。管理员始终可以取得权限并删除文件。

若数据目录在首次安装前已经存在，但所有者或 ACL 不符合上述要求，安装器会拒绝复用该目录。管理员应先检查其中内容并移走不可信目录，再重新安装；已由安装器加固并在卸载时保留的数据目录可直接复用。

`service uninstall` 会先优雅停止服务，再删除 SCM 服务注册。它不会隐式删除程序、配置、证书、审计文件或 Event Log 事件源，避免卸载时丢失数据。

## 服务配置

首次安装会生成完整的 `config.json`。配置只接受已知字段，时长必须为正数，`cert`/`key` 必须成对出现，所有文件和目录字段必须使用绝对路径。

```json
{
  "listen": ":8443",
  "ttl": "30m",
  "length": 10,
  "audit": "C:\\ProgramData\\RemoteAssistRelay\\logs\\audit.jsonl",
  "plain": false,
  "certs_dir": "C:\\ProgramData\\RemoteAssistRelay\\certs",
  "trust_source_ip": true,
  "no_auth": false
}
```

可选字段包括 `cert`、`key`、`stun` 和 `limits_file`。修改配置后需要重启服务：

```powershell
.\relay.exe service stop
.\relay.exe service start
```

也可以在首次安装时导入已准备好的配置。配置会先验证，再复制到固定位置：

```powershell
.\remote-assist-relay-windows-amd64.exe service install --config C:\deploy\relay-config.json
```

若配置引用 `C:\ProgramData\RemoteAssistRelay` 以外的证书、私钥、审计或 limits 文件，需单独授予 LocalService 对相应路径的最小读写权限。

## 日志和状态

服务生命周期、启动错误、TLS/监听错误和普通运行日志写入 Windows Application Event Log。普通日志使用有界异步队列，Event Log 短暂变慢不会阻塞 relay 连接处理；队列满时会丢弃普通事件，并在服务退出时写入丢弃数量警告。

```powershell
Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='RemoteAssistRelay'} -MaxEvents 50
```

| Event ID | 含义 |
|---:|---|
| 1 | 服务启动 |
| 2 | 停止请求或正常停止 |
| 3 | 服务启动或运行失败 |
| 100 | 普通运行日志 |
| 200 | 警告 |
| 300 | 错误 |

完整、高频的安全审计事件仍写入 `audit.jsonl`，不会只依赖容量有限的 Windows Application 日志。

## 前台模式

前台模式保留全部原有参数。Windows 启动时会主动关闭当前控制台的 QuickEdit 模式，防止文本选择阻塞同步控制台输出。

```powershell
.\remote-assist-relay-windows-amd64.exe run --listen :8443 --ttl 1h
```

直接使用旧参数形式仍然兼容：

```powershell
.\remote-assist-relay-windows-amd64.exe --listen :8443 --ttl 1h
```

服务已运行时不要再启动前台实例；第二个实例会因 8443 端口已占用而退出。
