# sms-receiver

[![CI](https://github.com/iliyian/sms-receiver/actions/workflows/ci.yml/badge.svg)](https://github.com/iliyian/sms-receiver/actions/workflows/ci.yml)
[![License: MPL 2.0](https://img.shields.io/badge/License-MPL_2.0-brightgreen.svg)](https://opensource.org/licenses/MPL-2.0)

接收 [SmsForwarder (pppscn)](https://github.com/pppscn/SmsForwarder) 通过 Webhook 推送的短信，解析后持久化到 SQLite，并提供带鉴权的查询接口。

- 默认监听 `127.0.0.1:8044`
- 同时兼容 SmsForwarder 的三种报文：默认 urlencoded 表单、自定义 JSON 模板、任意纯文本
- 原始报文完整入库；重复推送自动去重
- 共享 token 鉴权，可选校验 SmsForwarder 自带的 HMAC `sign`
- 单二进制 + 单文件数据库，无外部服务依赖

## 快速开始

要求 Go 1.27+。

```bash
git clone https://github.com/iliyian/sms-receiver.git
cd sms-receiver
export WEBHOOK_TOKEN="$(openssl rand -hex 32)"
go build -trimpath -o bin/sms-receiver .
./bin/sms-receiver
```

也可以直接安装到 `$GOBIN`：

```bash
go install github.com/iliyian/sms-receiver@latest
```

默认数据库在 `./data/sms.db`，监听 `127.0.0.1:8044`。自测：

```bash
curl -s http://127.0.0.1:8044/healthz
# {"status":"ok"}

curl -s -X POST http://127.0.0.1:8044/webhook \
  -H "Authorization: Bearer $WEBHOOK_TOKEN" \
  --data-urlencode 'from=10086' \
  --data-urlencode 'content=测试短信 123456' \
  --data-urlencode "timestamp=$(date +%s%3N)"
# {"id":1,"status":"ok"}

curl -s "http://127.0.0.1:8044/api/sms?token=$WEBHOOK_TOKEN&limit=10"
```

监听在 `127.0.0.1` 时手机无法直接访问，需要反向代理（Caddy/Nginx）或内网穿透，见下文。若只在局域网内直连，可设置 `LISTEN_ADDR=0.0.0.0:8044`。

## SmsForwarder 配置

在 App 中：**发送通道 → 添加 → Webhook**。

### 方式 A：默认表单（最简单）

- 请求方式：`POST`
- WebServer：`https://你的域名/webhook`
- 请求头：`Authorization: Bearer <WEBHOOK_TOKEN>`
- webParams：**留空**

App 会以 `application/x-www-form-urlencoded` 提交 `from`、`content`、`timestamp`（毫秒），设置 secret 后还会带上 `sign`。服务端可直接解析，但拿不到设备名、卡槽、短信接收时间等扩展字段。

### 方式 B：自定义 JSON 模板（推荐）

- 请求方式：`POST`
- WebServer：`https://你的域名/webhook`
- 请求头：
  ```
  Authorization: Bearer <WEBHOOK_TOKEN>
  Content-Type: application/json; charset=utf-8
  ```
- webParams：

```json
{
  "from": "[from]",
  "content": "[content]",
  "org_content": "[org_content]",
  "timestamp": "[timestamp]",
  "receive_time": "[receive_time]",
  "device_mark": "[device_mark]",
  "card_slot": "[card_slot]"
}
```

以上标签来自 SmsForwarder 官方文档（v2.4.3+）。如果 App 里配置了 `secret`，再加 `"sign": "[sign]"`，并在服务端设置相同的 `SMSF_SECRET`。

> 自定义模板以 `{` 开头时，App 会自动以 JSON 提交；否则按表单提交。两种格式服务端都支持。

### 无法设置请求头时

把 token 放进 URL：`https://你的域名/webhook?token=<WEBHOOK_TOKEN>`。查询参数中的 token 可能出现在反向代理访问日志里，优先使用请求头。

### 反向代理（Caddy 示例）

```caddyfile
sms.example.com {
    reverse_proxy 127.0.0.1:8044
}
```

Caddy 会自动申请 HTTPS 证书。也可以用 Nginx、Cloudflare Tunnel、frp 等，只要把公网请求转到 `127.0.0.1:8044`。

## HTTP API

除 `/healthz` 外，所有接口都需要鉴权，三选一：

- `Authorization: Bearer <token>`
- `X-Webhook-Token: <token>`
- `?token=<token>`

### `POST /webhook`

接收 SmsForwarder 推送。成功返回 `200`：

```json
{"id":1,"status":"ok"}
```

重复推送（相同设备/号码/内容/时间戳）返回：

```json
{"status":"duplicate"}
```

重复请求仍返回 2xx，避免 App 反复重试。请求体上限 1 MiB。去重依据设备、号码、内容、时间戳等字段；若报文里既没有 `timestamp` 也没有 `receive_time`，则在 5 分钟窗口内按相同内容去重。

### `GET /api/sms`

按 id 倒序分页查询。

| 参数 | 说明 |
| --- | --- |
| `limit` | 每页条数，1–500，默认 50 |
| `before_id` | 只返回 id 小于该值的记录（下一页用） |
| `from` | 按来源号码精确匹配 |
| `device` | 按设备标识精确匹配 |
| `q` | 在短信内容和来源号码中模糊搜索，最长 200 字符 |
| `since` | 服务端接收时间下限，RFC3339 或 Unix 时间戳 |
| `until` | 服务端接收时间上限，RFC3339 或 Unix 时间戳 |

响应示例：

```json
{
  "items": [
    {
      "id": 2,
      "received_at": "2026-10-29T08:54:38.922Z",
      "sent_at": "2024-04-05T12:34:38Z",
      "sender": "10086",
      "content": "流量提醒",
      "device": "Pixel 8",
      "card_slot": "卡1",
      "receive_time": "2024-04-05 20:34:38",
      "timestamp_raw": "1712345678902",
      "client_ip": "203.0.113.10"
    }
  ],
  "count": 1
}
```

`received_at` 是服务端收到请求的时间；`sent_at` 优先取 `receive_time`，取不到再用毫秒 `timestamp` 归一化。`count` 是本页条数，不是总条数；`next_before_id` 仅在当前页已满时返回，用于下一页。

### `GET /api/sms/{id}`

返回单条记录，额外包含 `raw_body`（原始请求体）和 `content_type`。不存在返回 `404`。

### `GET /healthz`

无需鉴权，检查进程和数据库是否可用。

## 配置项

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `WEBHOOK_TOKEN` | 无，**必填** | 共享 token，为空时进程拒绝启动 |
| `LISTEN_ADDR` | `127.0.0.1:8044` | HTTP 监听地址 |
| `DB_PATH` | `data/sms.db` | SQLite 文件路径，目录自动创建 |
| `SMSF_SECRET` | 空 | 非空时校验 SmsForwarder 的 `sign`，需与 App 中 secret 一致 |
| `SMSF_SIGN_SKEW` | `1h` | 签名时间戳允许的偏差 |
| `TRUST_PROXY` | `false` | 为 `true` 时从 `X-Forwarded-For` 取客户端 IP |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `TZ` | 系统时区 | 解析不带时区的 `receive_time` 时使用，如 `Asia/Shanghai` |

关于签名：SmsForwarder 的 `sign` 是 `base64(HMAC-SHA256(timestamp+"\n"+secret))`，只覆盖时间戳，用于证明持有 secret 并防重放，**不覆盖短信内容**。共享 token 仍是必需的访问控制。

## 数据与备份

表结构：

```sql
CREATE TABLE sms (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  dedupe_key    TEXT NOT NULL UNIQUE,
  received_ms   INTEGER NOT NULL,   -- 服务端接收时间
  sent_at       TEXT,               -- 手机侧时间（归一化为 UTC RFC3339）
  sender        TEXT,               -- 来源号码/包名
  content       TEXT,               -- 短信内容
  org_content   TEXT,               -- 原始内容（模板加工前）
  device        TEXT,
  card_slot     TEXT,
  title         TEXT,
  app_version   TEXT,
  receive_time  TEXT,               -- 手机上报的原始接收时间
  timestamp_raw TEXT,               -- 手机上报的原始毫秒时间戳
  client_ip     TEXT,
  content_type  TEXT,
  raw_body      TEXT NOT NULL       -- 完整原始报文
);
```

数据库以 WAL 模式运行，文件权限为 `0600`，所在目录首次创建时权限为 `0700`。备份必须在运行中安全进行：

```bash
sqlite3 data/sms.db ".backup '/backup/sms-$(date +%F).db'"
```

不要直接 `cp` 正在写入的 `.db` 文件（需要连同 `-wal`/`-shm` 一起处理）。服务端不会自动删除旧数据，需要时自行按 `received_ms` 清理。

## Docker

```bash
docker build -t sms-receiver .
docker run -d --name sms-receiver \
  --restart unless-stopped \
  -e WEBHOOK_TOKEN="$(openssl rand -hex 32)" \
  -e LISTEN_ADDR=:8044 \
  -e TZ=Asia/Shanghai \
  -p 127.0.0.1:8044:8044 \
  -v sms-data:/data \
  sms-receiver
```

容器内必须监听 `:8044`，宿主机端口再绑定到 `127.0.0.1`，否则 Docker 端口映射无法访问。使用 named volume 时 `/data` 权限由镜像内非 root 用户持有；改用 bind mount 时需先 `chown` 给容器内的 `app` 用户。镜像健康检查调用 `/healthz`。

## systemd

参考 `deploy/sms-receiver.service` 和 `deploy/env.example`：

```bash
sudo useradd --system --home /var/lib/sms-receiver --shell /usr/sbin/nologin sms-receiver
sudo install -d -o sms-receiver -g sms-receiver -m 700 /var/lib/sms-receiver
sudo install -m 755 bin/sms-receiver /usr/local/bin/sms-receiver
sudo install -d -m 700 /etc/sms-receiver
sudo install -m 600 deploy/env.example /etc/sms-receiver/env   # 然后填入 token
sudo install -m 644 deploy/sms-receiver.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sms-receiver
```

## 安全说明

- 短信内容常含验证码、银行通知等敏感信息。不要把 `/api/sms` 无鉴权暴露到公网；公网入口务必启用 HTTPS。
- `WEBHOOK_TOKEN` 只从环境变量读取，不会写入日志。日志只记录请求路径、状态码、id 和内容长度，不记录短信正文、号码和 token。
- `sign` 不能替代 token；它只证明请求方知道 secret，且不覆盖正文。
- 查询参数里的 token 会进入代理日志，能用请求头就不要用 URL 参数。
- 数据库目录应只允许运行用户访问；收到数据库文件后请按 `0600` 处理。

## 开发

```bash
make test      # go test ./...
make vet       # go vet ./...
make build     # 生成 bin/sms-receiver
make run       # 本地启动（需先设置 WEBHOOK_TOKEN）
```

CI 在每次 push 和 PR 时运行 `gofmt` 检查、`go vet` 与 `go test`。

## 许可证

本项目以 [Mozilla Public License 2.0](LICENSE) 发布。
