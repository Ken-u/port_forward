# port_forward

通过 **一个主端口**（可配合 frp `stcp`）做 TCP 多路复用端口转发，并内置管理网页。

适合公司和家里已用 frp 打通、但不想每加一个端口就改 frp 配置的场景。业务流量是原始 TCP 转发，不会改 HTTP 路径，网站一般不会因路径拼接出错。

## 功能

- 单二进制：`port_fwd --host 127.0.0.1 --port 3000`
- 管理网页：打开主端口即可配置节点与转发规则
- 双向转发：两边都跑同一程序，互相当节点
- 动态生效：增删改规则立即开/关本地监听，无需重启
- 错误可见：端口占用、地址无效等监听失败原因直接显示在管理页
- 规则辅助：提示可用本地端口范围、点击监听地址打开新标签、记住常用目标 IP
- 可转发局域网目标：目标地址填内网 IP 即可
- 配置持久化：默认写入 `~/.port_fwd.json`

## 下载

从 [Releases](https://github.com/Ken-u/port_forward/releases) 下载对应平台压缩包：

| 文件 | 平台 |
|------|------|
| `port_fwd-darwin-arm64.tar.gz` | macOS Apple Silicon |
| `port_fwd-darwin-amd64.tar.gz` | macOS Intel |
| `port_fwd-linux-arm64.tar.gz` | Linux / Debian arm64 |
| `port_fwd-linux-amd64.tar.gz` | Linux x86_64 |
| `port_fwd-linux-386.tar.gz` | Linux x86 (32-bit) |
| `port_fwd-windows-amd64.zip` | Windows x64 |
| `port_fwd-windows-arm64.zip` | Windows ARM64 |

也可在仓库 Actions 的 workflow artifacts 里下载未打包二进制。

## 一键安装

macOS / Linux：

```bash
curl -fsSL https://raw.githubusercontent.com/Ken-u/port_forward/main/install.sh | bash
```

脚本会：

1. 识别当前平台并下载最新 Release 二进制
2. 安装到 `~/.local/bin/port_fwd`
3. **Linux** 下交互询问是否安装到当前用户的 systemd 服务（`systemctl --user`）

常用环境变量：

```bash
# 指定版本
PORT_FWD_VERSION=v0.1.5 bash install.sh

# 非交互安装用户 systemd 服务
PORT_FWD_SYSTEMD=yes PORT_FWD_HOST=0.0.0.0 PORT_FWD_PORT=9000 \
  curl -fsSL https://raw.githubusercontent.com/Ken-u/port_forward/main/install.sh | bash

# 自定义安装目录
PORT_FWD_INSTALL_DIR=/usr/local/bin curl -fsSL ... | bash
```

Linux systemd 常用命令：

```bash
systemctl --user status port_fwd
systemctl --user restart port_fwd
journalctl --user -u port_fwd -f
```

如需注销后仍保持服务运行：

```bash
loginctl enable-linger "$USER"
```

## 快速开始

```bash
# macOS / Linux
tar -xzf port_fwd-darwin-arm64.tar.gz
chmod +x port_fwd-darwin-arm64
./port_fwd-darwin-arm64 --port 3000
```

浏览器打开：http://127.0.0.1:3000

### 参数

| 参数 | 说明 | 默认 |
|------|------|------|
| `--host` | 主端口监听 IPv4 地址 | `0.0.0.0` |
| `--port` | 主端口（管理网页 + 隧道） | `9000` |
| `--config` | 配置文件路径 | `~/.port_fwd.json` |

程序默认监听所有 IPv4 网卡，但强制使用 `tcp4`，不会监听或暴露 IPv6。若只允许本机和本机 frp 访问：

```bash
./port_fwd --host 127.0.0.1 --port 3000
```

默认模式下可用手机访问 `http://电脑局域网IP:3000`。请配合系统防火墙限制可信网段。

首次启动随机生成的 Token 只在管理页显示一次，请立即复制到另一端。已经配置的 Token 不会再次通过管理 API 回显。

清除全部节点、规则和 Token：

```bash
./port_fwd reset
```

使用自定义配置文件时，参数放在命令前面：

```bash
./port_fwd --config /path/to/config.json reset
```

## 和 frp stcp 配合

假设家里访问公司：

```ini
# 公司 frpc.ini：暴露本机 port_fwd 主端口
[mux]
type = stcp
sk = your_secret
local_ip = 127.0.0.1
local_port = 3000

# 家里 frpc.ini：访问上面的 stcp
[mux-visitor]
type = stcp
role = visitor
server_name = mux
sk = your_secret
bind_addr = 127.0.0.1
bind_port = 7000
```

然后：

1. 两边都运行：`./port_fwd --port 3000`
2. 两边管理页设置 **相同 Token**
3. 家里添加节点：`127.0.0.1:7000`（visitor 本地端口）
4. 家里添加规则，例如：
   - 本地监听 `127.0.0.1:13000`
   - 目标 `192.168.1.20:8080`（公司局域网任意可达地址）
5. 访问 `http://127.0.0.1:13000` 即可

公司访问家里同理：再开一条反向 stcp，或在已连通的隧道上由对端添加反向规则。

> 传输加密交给 frp TLS / stcp 即可，本程序管理页默认 HTTP。

## 本地编译

需要 Go 1.22+（仓库 `go.mod` 指定版本）：

```bash
go build -o port_fwd .
./port_fwd --port 3000
```

交叉编译示例：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o port_fwd-linux-arm64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o port_fwd-darwin-arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o port_fwd-windows-amd64.exe .
```

## CI / Release

GitHub Actions（`.github/workflows/build.yml`）会：

1. **push / PR**：编译全部平台并上传 artifact  
2. **推送 `v*` tag**：编译全部平台，打包 `.tar.gz` / `.zip`，生成 `checksums.txt`，并自动创建 GitHub Release  
3. **Actions 手动发版**：打开 [Build & Release](https://github.com/Ken-u/port_forward/actions/workflows/build.yml) → Run workflow → 填写版本号（如 `1.0.0`）→ 自动打 `v1.0.0` tag，随后触发编译与发版

手动打 tag 同样可以：

```bash
git tag v1.0.0
git push origin v1.0.0
```

## 安全建议

- 修改默认 Token（两边必须一致）
- 主端口仅通过 frp stcp 等私有通道暴露，不要直接挂公网
- 转发目标尽量限制在可信内网地址

## License

MIT
