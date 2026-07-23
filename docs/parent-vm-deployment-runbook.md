# 真实 Enclave 父 VM 部署 Runbook

本文档说明如何把 `tcp-to-vsock-gateway` 部署到真实 Enclave 父 VM 上，
让中转站通过私网 TCP endpoint 访问 Enclave 内的 `proof-of-observation`
runtime。

gateway 只做 L4 透明转发：

```text
relay / middlebox  ->  TCP gateway_private_ip:15005
tcp-to-vsock-gateway  ->  AF_VSOCK enclave_cid:5005
proof-of-observation runtime
```

它不会解析、验证、生成、缓存或改写 proof、HTTP、SSE、multipart 或
`TEE Relay Frame Protocol v1` frame。

## 1. 适用范围

支持的父 VM 架构：

| Enclave provider | Gateway binary |
|---|---|
| AWS Nitro Enclave | `linux/arm64` |
| 阿里云 Enclave | `linux/amd64` |
| 华为 QingTian Enclave | `linux/amd64` |

默认端口：

| Port | Direction | Purpose |
|---|---|---|
| TCP `15005` | relay -> gateway | 业务 L4 透明转发入口 |
| TCP `15006` | ops/LB -> gateway | `/healthz`、`/readyz`、`/metrics` |
| vsock `5005` | gateway -> Enclave | proof relay runtime |
| vsock `5006` | gateway -> Enclave | Enclave metrics readiness |

生产推荐：

- gateway 以 host binary + systemd 运行在父 VM 上。
- relay listener 绑定私网地址或 `0.0.0.0`，通过安全组 / 防火墙 / LB 限制来源。
- admin listener 只绑定本机或私网管理地址，不暴露公网。
- 一个 gateway 进程对应一个 Enclave CID。

## 2. 前置条件

父 VM 上必须已完成：

1. 已安装对应云厂商 Enclave CLI。
2. 已启动 `proof-of-observation` Enclave runtime。
3. Enclave 内 runtime 监听：
   - relay vsock port：`5005`
   - metrics vsock port：`5006`
4. 父 VM 安全组允许中转站或私网 LB 访问 gateway relay 端口。
5. 中转站侧已经支持把原来的 vsock dialer 切换为 TCP dialer。

先确认 Enclave 正在运行并获取 CID。

AWS Nitro Enclave：

```bash
sudo nitro-cli describe-enclaves
```

阿里云 Enclave：

```bash
sudo enclave-cli describe-enclaves
```

输出中记录 CID，例如：

```text
EnclaveCID: 4
```

华为 QingTian Enclave：

```bash
# 使用华为云 QingTian Enclave CLI 查询正在运行的 Enclave。
# 不同镜像/发行版上的命令包装可能不同，以实际安装的 CLI 文档为准。
# 目标是拿到当前 Enclave 的 CID。
```

后续示例统一使用：

```bash
export ENCLAVE_CID=4
```

## 3. 构建 Gateway Binary

可以在 CI、开发机或任意可信构建机上构建，再把二进制发布到父 VM。构建机器
不需要支持 `AF_VSOCK`。

进入仓库：

```bash
cd /path/to/tcp-to-vsock-gateway
```

运行测试：

```bash
go test ./...
go test -race ./...
go vet ./...
```

构建阿里云 / QingTian 父 VM 使用的 `linux/amd64`：

```bash
VERSION=v0.1.0
COMMIT=$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
mkdir -p dist

GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
  -o dist/tcp-to-vsock-gateway_linux_amd64 \
  ./cmd/tcp-to-vsock-gateway
```

构建 AWS Nitro 父 VM 使用的 `linux/arm64`：

```bash
VERSION=v0.1.0
COMMIT=$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
mkdir -p dist

GOOS=linux GOARCH=arm64 go build \
  -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
  -o dist/tcp-to-vsock-gateway_linux_arm64 \
  ./cmd/tcp-to-vsock-gateway
```

生成校验文件：

```bash
cd dist
sha256sum tcp-to-vsock-gateway_linux_* > SHA256SUMS
cat SHA256SUMS
```

如果构建机是 macOS，没有 `sha256sum`，使用：

```bash
cd dist
shasum -a 256 tcp-to-vsock-gateway_linux_* > SHA256SUMS
cat SHA256SUMS
```

把对应架构的二进制和 `SHA256SUMS` 上传到父 VM。例如：

```bash
scp tcp-to-vsock-gateway_linux_amd64 SHA256SUMS user@PARENT_VM:/tmp/
```

如果环境不能使用 `scp`，也可以通过内部制品库、对象存储、ACR 附件镜像或其它
公司标准发布通道分发。关键要求是父 VM 上必须能校验 SHA256。

## 4. 在父 VM 安装 Binary

以下命令在父 VM 上执行。

创建运行用户和目录：

```bash
sudo useradd --system --no-create-home --shell /sbin/nologin tcp-vsock-gw 2>/dev/null || true
sudo mkdir -p /etc/tcp-to-vsock-gateway
sudo mkdir -p /var/log/tcp-to-vsock-gateway
sudo chown tcp-vsock-gw:tcp-vsock-gw /var/log/tcp-to-vsock-gateway
```

安装 binary。阿里云 / QingTian：

```bash
cd /tmp
sha256sum -c SHA256SUMS --ignore-missing

sudo install -o root -g root -m 0755 \
  /tmp/tcp-to-vsock-gateway_linux_amd64 \
  /usr/local/bin/tcp-to-vsock-gateway
```

AWS Nitro：

```bash
cd /tmp
sha256sum -c SHA256SUMS --ignore-missing

sudo install -o root -g root -m 0755 \
  /tmp/tcp-to-vsock-gateway_linux_arm64 \
  /usr/local/bin/tcp-to-vsock-gateway
```

确认版本：

```bash
/usr/local/bin/tcp-to-vsock-gateway --version
```

## 5. 编写 Gateway 配置

先确认本机私网 IP。如果 relay 和 gateway 不在同一台机器，推荐绑定父 VM 私网 IP：

```bash
hostname -I
```

示例配置：

```bash
sudo tee /etc/tcp-to-vsock-gateway/env >/dev/null <<EOF
TTVG_LISTEN_ADDR=0.0.0.0:15005
TTVG_VSOCK_CID=${ENCLAVE_CID}
TTVG_VSOCK_PORT=5005
TTVG_METRICS_ADDR=127.0.0.1:15006
TTVG_VSOCK_METRICS_PORT=5006
TTVG_CONNECT_TIMEOUT=5s
TTVG_IDLE_TIMEOUT=300s
TTVG_READY_CACHE_TTL=1s
TTVG_SHUTDOWN_TIMEOUT=300s
TTVG_MAX_CONNS=1024
TTVG_LOG_LEVEL=info
EOF
```

如果私网 LB 需要直接访问 `/readyz`，把 `TTVG_METRICS_ADDR` 改成父 VM 私网
IP 或 `0.0.0.0:15006`，并通过安全组限制来源：

```bash
sudo sed -i.bak 's/^TTVG_METRICS_ADDR=.*/TTVG_METRICS_ADDR=0.0.0.0:15006/' \
  /etc/tcp-to-vsock-gateway/env
```

配置检查：

```bash
set -a
. /etc/tcp-to-vsock-gateway/env
set +a

/usr/local/bin/tcp-to-vsock-gateway --check-config
```

## 6. 安装 systemd Service

写入 service：

```bash
sudo tee /etc/systemd/system/tcp-to-vsock-gateway.service >/dev/null <<'EOF'
[Unit]
Description=TCP to vsock gateway for proof-of-observation
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/tcp-to-vsock-gateway/env
ExecStart=/usr/local/bin/tcp-to-vsock-gateway
User=tcp-vsock-gw
Group=tcp-vsock-gw
Restart=always
RestartSec=2s
LimitNOFILE=65536
TimeoutStopSec=330s

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/tcp-to-vsock-gateway

[Install]
WantedBy=multi-user.target
EOF
```

如果某个云父 VM 的 systemd hardening 阻断 `AF_VSOCK`，先查看日志，再只放宽
对应选项。快速定位时可以临时注释 hardening 项，确认后再最小化收敛。

启动服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable tcp-to-vsock-gateway
sudo systemctl restart tcp-to-vsock-gateway
sudo systemctl status tcp-to-vsock-gateway --no-pager
```

查看日志：

```bash
sudo journalctl -u tcp-to-vsock-gateway -n 100 --no-pager
```

## 7. 父 VM 本机验证

检查监听端口：

```bash
ss -lntp | grep -E '15005|15006' || true
```

本地 health check：

```bash
curl -i http://127.0.0.1:15006/healthz
curl -i http://127.0.0.1:15006/readyz
curl -s http://127.0.0.1:15006/metrics | head -50
```

期望：

```text
/healthz -> 200
/readyz  -> 200
```

`/readyz` 返回 `503` 时，优先检查：

1. `TTVG_VSOCK_CID` 是否等于当前运行 Enclave CID。
2. Enclave runtime 是否监听 metrics vsock port `5006`。
3. Enclave 是否已经完成启动。
4. 父 VM 是否允许创建 `AF_VSOCK` socket。

如果之前已经有 `socat-vsock` 或厂商工具，可以先直连 metrics port 交叉验证。
命令格式以实际工具为准，例如：

```bash
timeout 5 sudo /path/to/socat-vsock -u \
  vsock-connect:${ENCLAVE_CID}:5006 \
  - \
  | xxd -g1 -c16
```

能看到 `0x20` 开头的 metrics frame，说明 Enclave metrics port 可达。

## 8. 中转站配置

中转站不再直接 dial Enclave vsock，而是 dial gateway TCP endpoint。

同父 VM 部署时：

```text
127.0.0.1:15005
```

跨机器部署时：

```text
<gateway_parent_vm_private_ip>:15005
```

中转站侧必须保持原有 `TEE Relay Frame Protocol v1`：

- 每次用户请求生成 fresh nonce。
- 发送完整 `REQ_HEAD` / `REQ_BODY`。
- 读取 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER`。
- 把 proof 原样返回给用户或 verifier。
- 已经写入任何请求 frame 后，不得无感重放到另一个 gateway。
- 已经向用户透传任何响应字节后，不得拼接另一次请求的 proof。

如果中转站使用环境变量配置 gateway，示例：

```bash
TEE_PROOF_ENABLED=true
TEE_PROOF_REQUIRE=true
TEE_PROOF_TRANSPORT=tcp
TEE_PROOF_GATEWAY_ADDR=<gateway_parent_vm_private_ip>:15005
TEE_PROOF_TIMEOUT_SECONDS=300
```

实际变量名以中转站实现为准。核心是把原来的：

```text
AF_VSOCK <cid>:5005
```

替换成：

```text
TCP <gateway_private_ip>:15005
```

## 9. 端到端 Proof 验证

从调用方或测试机发起非流式请求：

```bash
curl -i -X POST "http://<relay_host>:<relay_port>/v1/chat/completions" \
  -H "Authorization: Bearer <relay_api_key>" \
  -H "Content-Type: application/json" \
  -H "X-TEE-Proof: required" \
  -d '{
    "model": "<model>",
    "messages": [
      {
        "role": "user",
        "content": "请用一句话介绍你自己。"
      }
    ],
    "stream": false
  }'
```

保存完整响应，然后使用 `proof-of-observation` verifier 按对应 evidence profile
验证。不同中转站保存响应的方式不同，核心要求是保留完整 proof payload，
不能只保存模型文本。

流式请求也必须验证：

```bash
curl -N -i -X POST "http://<relay_host>:<relay_port>/v1/chat/completions" \
  -H "Authorization: Bearer <relay_api_key>" \
  -H "Content-Type: application/json" \
  -H "X-TEE-Proof: required" \
  -d '{
    "model": "<model>",
    "messages": [
      {
        "role": "user",
        "content": "请分三点说明今天的测试目标。"
      }
    ],
    "stream": true
  }'
```

验收标准：

- 非流式 proof verifier 通过。
- 流式 proof verifier 通过。
- gateway `/metrics` 中连接数和字节数增长合理。
- 日志中没有请求体、响应体、Authorization、nonce 或 proof trailer 原文。

## 10. 故障注入检查

错误 CID：

```bash
sudo cp /etc/tcp-to-vsock-gateway/env /etc/tcp-to-vsock-gateway/env.bak
sudo sed -i 's/^TTVG_VSOCK_CID=.*/TTVG_VSOCK_CID=999999/' /etc/tcp-to-vsock-gateway/env
sudo systemctl restart tcp-to-vsock-gateway
curl -i http://127.0.0.1:15006/readyz
sudo mv /etc/tcp-to-vsock-gateway/env.bak /etc/tcp-to-vsock-gateway/env
sudo systemctl restart tcp-to-vsock-gateway
```

期望 `/readyz` 返回 `503`。

停止 Enclave：

```bash
# 使用对应云厂商 CLI 停止 Enclave。
# 停止后 gateway /readyz 应返回 503，业务 proof 请求应失败为明确错误。
```

重启 gateway：

```bash
sudo systemctl restart tcp-to-vsock-gateway
curl -i http://127.0.0.1:15006/readyz
```

## 11. 滚动发布

推荐顺序：

1. 让 LB 摘除当前 gateway 节点，或确认 `/readyz` 已失败。
2. 等待 `ttvg_connections_active` 降到 `0`。
3. 重启 gateway 或替换 binary。
4. `/readyz` 恢复 `200` 后再加入 LB。

观察 active connection：

```bash
curl -s http://127.0.0.1:15006/metrics | grep '^ttvg_connections_active'
```

替换 binary：

```bash
sudo systemctl stop tcp-to-vsock-gateway
sudo install -o root -g root -m 0755 \
  /tmp/tcp-to-vsock-gateway_linux_amd64 \
  /usr/local/bin/tcp-to-vsock-gateway
sudo systemctl start tcp-to-vsock-gateway
```

AWS Nitro 使用 `tcp-to-vsock-gateway_linux_arm64`。

## 12. 常见问题

### `/readyz` 返回 503

检查：

```bash
sudo journalctl -u tcp-to-vsock-gateway -n 100 --no-pager
grep '^TTVG_' /etc/tcp-to-vsock-gateway/env
```

常见原因：

- CID 变了，但 env 里仍是旧 CID。
- Enclave runtime 未启动完成。
- metrics vsock port 不是 `5006`。
- 父 VM systemd sandbox 或容器 runtime 阻断 `AF_VSOCK`。

### 中转站请求超时

检查：

```bash
curl -i http://127.0.0.1:15006/readyz
curl -s http://127.0.0.1:15006/metrics | grep -E 'dial|copy|active|bytes'
sudo journalctl -u tcp-to-vsock-gateway -n 100 --no-pager
```

常见原因：

- 中转站连错 gateway IP 或端口。
- 安全组未放行中转站到 TCP `15005`。
- Enclave 内 runtime relay port `5005` 未监听。
- 长流式请求超过中转站或 LB timeout。

### `operation not permitted`

通常是父 VM 权限或容器 runtime 限制 `AF_VSOCK`。生产推荐 host binary。
如果必须容器化，需要确认 seccomp/capability/network mode 放行
`socket(AF_VSOCK, ...)`。

## 13. 上线检查清单

- `tcp-to-vsock-gateway --version` 输出符合发布版本。
- `sha256sum -c SHA256SUMS --ignore-missing` 通过。
- `--check-config` 通过。
- `systemctl status tcp-to-vsock-gateway` 为 running。
- `/healthz` 返回 `200`。
- `/readyz` 返回 `200`。
- 非流式 proof verifier 通过。
- 流式 proof verifier 通过。
- 停止 Enclave 后 `/readyz` 返回 `503`。
- 错误 CID 会导致请求失败而不是返回无 proof 响应。
- 中转站失败路径会明确暴露本次 proof 不可用。
- 日志和 metrics 不包含请求体、响应体、Authorization、nonce 或 proof trailer。
