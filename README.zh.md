# DeltaChat Notify Bot

一个将 webhook 消息转发至 Delta Chat 的工具。接收 HTTP POST 请求，并将其作为 Delta Chat 消息发送给已配置的收件人。支持 Slack 兼容的 JSON 载荷和 multipart 文件上传。

## 工作原理

Bot 会启动一个 `deltachat-rpc-server` 子进程（通过 [deltabot-cli-go](https://github.com/deltachat-bot/deltabot-cli-go) 框架），负责处理所有 Delta Chat 的 IMAP/SMTP 操作。

启动时，`NOTIFY_BOT_RECIPIENTS` 中的每个条目都会被解析为一个 Delta Chat 聊天：

- **普通邮箱** — `CreateContact` + `CreateChatByContactId`。消息立即发送，但不具备端对端加密。
- **`OPENPGP4FPR:` SecureJoin 链接** — 触发异步密钥验证握手。在握手完成之前，该聊天状态标记为"待确认"。对待确认聊天的 webhook 推送请求将被跳过，并返回 `503 Retry-After` 响应。

HTTP 服务器在一个 goroutine 中与 Delta Chat 事件循环并行运行。收到的请求将被分发给所有就绪的收件人（或通过 `recipient` 字段指定的特定子集）。

## 安装

### Nix flake

```nix
# flake.nix
{
  inputs.dc-notify-bot.url = "github:x3ps/dc-notify-bot";

  outputs = { nixpkgs, dc-notify-bot, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      modules = [
        dc-notify-bot.nixosModules.default
        {
          services.dc-notify-bot = {
            enable = true;
            email = "bot@example.com";
            passwordFile = "/run/secrets/dc-notify-bot-password";
            recipients = [
              "alice@example.com"
              "OPENPGP4FPR:AABB1234...#a=bob@example.com"
            ];
          };
        }
      ];
    };
  };
}
```

该模块会创建一个专用的 `dc-notify-bot` 系统用户，在首次启动时自动初始化 Delta Chat 账户，并在加固的 systemd 单元下运行 bot。

### Docker

```bash
# 从 GitHub Container Registry 拉取镜像
docker pull ghcr.io/x3ps/dc-notify-bot:latest

# 初始化账户（仅首次需要）
docker run --rm -v dc-notify-data:/data \
  ghcr.io/x3ps/dc-notify-bot:latest \
  dc-notify-bot -f /data init bot@example.com PASSWORD

# 运行 bot
docker run -d \
  -v dc-notify-data:/data \
  -e NOTIFY_BOT_RECIPIENTS="alice@example.com" \
  -p 8080:8080 \
  ghcr.io/x3ps/dc-notify-bot:latest
```

### 从源码构建

需要 Go 1.24+ 及 `PATH` 中存在 `deltachat-rpc-server`。

```bash
git clone https://github.com/x3ps/dc-notify-bot
cd dc-notify-bot
go build -o dc-notify-bot .
```

## 使用方法

### JSON 载荷（Slack 兼容格式）

```bash
# 向所有收件人发送简单文本消息
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello from webhook"}' \
  http://localhost:8080/webhook

# 发送给指定收件人
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello","recipient":"alice@example.com"}' \
  http://localhost:8080/webhook

# 发送给多个指定收件人
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello","recipients":["alice@example.com","bob@example.com"]}' \
  http://localhost:8080/webhook
```

### Multipart 文件上传

```bash
# 带附件的文本消息
curl -F 'text=Check this out' -F 'file=@photo.jpg' \
  http://localhost:8080/webhook

# 仅上传文件（文本默认为 "(empty notification)"）
curl -F 'file=@document.pdf' http://localhost:8080/webhook

# 向指定收件人发送文件
curl -F 'text=For you' -F 'file=@photo.jpg' -F 'recipient=alice@example.com' \
  http://localhost:8080/webhook

# 发送给多个收件人
curl -F 'text=Team update' -F 'recipient=alice@example.com' -F 'recipient=bob@example.com' \
  http://localhost:8080/webhook
```

## JSON 字段说明

| 字段 | 是否必填 | 说明 |
|------|----------|------|
| `text` | 是 | 消息文本（markdown 原样传递） |
| `recipient` | 否 | 指定收件人的邮箱地址（须存在于 `NOTIFY_BOT_RECIPIENTS` 中） |
| `recipients` | 否 | 收件人邮箱地址数组（若同时提供 `recipient`，则合并处理） |

## Multipart 字段说明

| 字段 | 是否必填 | 说明 |
|------|----------|------|
| `text` | `text` 或 `file` 二选一必填 | 消息文本 |
| `file` | `text` 或 `file` 二选一必填 | 文件附件 |
| `recipient` | 否 | 指定收件人的邮箱地址（可重复填写以指定多个收件人） |

## 错误响应

| 状态码 | 含义 |
|--------|------|
| 400 | JSON 格式无效、缺少必填字段或收件人未知 |
| 405 | 请求方法不允许 |
| 413 | 请求体过大 |
| 415 | 不支持的 Content-Type（请使用 `application/json` 或 `multipart/form-data`） |
| 500 | 所有消息发送均失败 |
| 503 | 所有收件人均处于 SecureJoin 握手待确认状态（包含 `Retry-After` 响应头） |

## 接口端点

- `POST /webhook` — 发送通知
- `GET /health` — 存活探针
- `GET /recipients` — 列出已配置的收件人及其状态

## 配置

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `NOTIFY_BOT_RECIPIENTS` | （必填） | 以逗号分隔的收件人列表：邮箱地址或 `OPENPGP4FPR:` SecureJoin URI |
| `NOTIFY_BOT_LISTEN` | `127.0.0.1:8080` | HTTP 监听地址 |
| `NOTIFY_BOT_MAX_PAYLOAD_BYTES` | `1048576`（1 MiB） | 请求体最大字节数 |

需要 `PATH` 中存在 `deltachat-rpc-server`。

## SecureJoin

SecureJoin 可建立带密钥验证的端对端加密聊天。如需从 Delta Chat 应用获取 `OPENPGP4FPR:` 邀请链接：

1. 打开 Delta Chat → **设置** → **邀请加入 Delta Chat**（或**二维码**）
2. 点击**复制链接**，获取 `OPENPGP4FPR:…` URI
3. 将该 URI 添加到 `NOTIFY_BOT_RECIPIENTS`

首次联系时，bot 会发起 SecureJoin 握手。聊天会被标记为"待确认"，直到收件人在其 Delta Chat 客户端接受请求，协议自动完成。此后，联系人通过验证，消息将进行端对端加密。

后续重启时，已验证的联系人会被立即识别（无需重新握手），聊天可直接使用。

## 开发

```bash
# 进入包含 Go 工具链、gopls 和 deltachat-rpc-server 的开发环境
nix develop

# 运行测试
go test -v ./...

# 构建
nix build

# 手动测试
dc-notify-bot init bot@example.com PASSWORD
dc-notify-bot serve &
curl -X POST http://127.0.0.1:8080/webhook \
  -H 'Content-Type: application/json' \
  -d '{"text":"hello"}'
```

## 许可证

本项目采用 **GNU Affero General Public License v3.0** 授权。
详见 [LICENSE](./LICENSE)。

## 第三方库（Go 模块）

| 库 | 版本 | 许可证 | 作者 / 版权持有者 |
|---|---|---|---|
| `github.com/chatmail/rpc-client-go/v2` | `v2.0.1` | MPL-2.0 | Chatmail contributors |
| `github.com/deltachat-bot/deltabot-cli-go/v2` | `v2.0.0-20260308000653-bc7d68bb83c1` | MPL-2.0 | DeltaChat Bot contributors |
| `github.com/spf13/cobra` | `v1.8.0` | Apache-2.0 | spf13/cobra maintainers and contributors |
| `github.com/cpuguy83/go-md2man/v2` | `v2.0.3` | MIT | Brian Goff and contributors |
| `github.com/creachadair/jrpc2` | `v1.1.2` | BSD-3-Clause | Michael J. Fromberger |
| `github.com/creachadair/mds` | `v0.8.2` | BSD-2-Clause | Michael J. Fromberger |
| `github.com/davecgh/go-spew` | `v1.1.1` | ISC | Dave Collins |
| `github.com/fortytw2/leaktest` | `v1.3.0` | BSD-3-Clause | The Go Authors |
| `github.com/google/go-cmp` | `v0.6.0` | BSD-3-Clause | The Go Authors |
| `github.com/inconshreveable/mousetrap` | `v1.1.0` | Apache-2.0 | inconshreveable contributors |
| `github.com/kr/text` | `v0.2.0` | MIT | Keith Rarick |
| `github.com/pmezard/go-difflib` | `v1.0.0` | BSD-2-Clause | Patrick Mezard |
| `github.com/russross/blackfriday/v2` | `v2.1.0` | BSD-2-Clause | Russ Ross and contributors |
| `github.com/spf13/pflag` | `v1.0.5` | BSD-3-Clause | Alex Ogier; The Go Authors |
| `github.com/stretchr/testify` | `v1.8.2` | MIT | Mat Ryer, Tyler Bunnell, and contributors |
| `go.uber.org/goleak` | `v1.2.0` | MIT | Uber Technologies, Inc. and contributors |
| `go.uber.org/multierr` | `v1.11.0` | MIT | Uber Technologies, Inc. and contributors |
| `go.uber.org/zap` | `v1.26.0` | MIT | Uber Technologies, Inc. and contributors |
| `golang.org/x/sync` | `v0.6.0` | BSD-3-Clause | The Go Authors |
| `gopkg.in/check.v1` | `v0.0.0-20161208181325-20d25e280405` | BSD-2-Clause | Gustavo Niemeyer |
| `gopkg.in/yaml.v3` | `v3.0.1` | MIT or Apache-2.0 | Kirill Simonov and contributors |

## 第三方产品与工具

| 产品 | 用途 | 许可证 | 作者 / 维护者 |
|---|---|---|---|
| `deltachat-rpc-server` | Bot 使用的运行时 RPC 后端 | MPL-2.0 | Delta Chat / Chatmail contributors |
| Go toolchain (`go`) | 构建与测试工具链 | BSD-3-Clause | The Go Authors |
| Nixpkgs (`NixOS/nixpkgs`) | Nix 构建/开发环境的包来源 | MIT | Nixpkgs contributors |
| `numtide/flake-utils` | Flake 输出辅助工具 | MIT | Numtide contributors |
| Debian `bookworm-slim` images | Docker 构建/运行时基础镜像 | 多种自由软件许可证 | Debian contributors |

备注：
- Go 模块版本来自 `go list -m all`。
- 许可证和作者信息基于上游 `LICENSE` 元数据和包清单。
