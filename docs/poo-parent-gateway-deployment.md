# PoO Parent Gateway 生产部署配置文档

本文档说明如何在 Enclave 父 VM 上部署 `tcp-to-vsock-gateway`
的 PoO Parent Gateway 能力，使中转站接入后支持以下两条链路：

```text
中转站 -> PoO Parent Gateway -> Enclave / PoO runtime -> 上游模型
中转站 -> PoO Parent Gateway -> Enclave / PoO runtime -> PoO Parent Gateway egress -> 代理 -> 上游模型
```

PoO Parent Gateway 的定位是：

- Proof Relay API：接收中转站传入的 PoO frame stream 和可选 `X-PoO-Proxy-URL`。
- Egress Route / Lane：为每次请求分配 Enclave 可连接的 egress port，并维护长生命周期 route cache。
- Egress Proxy：按请求使用 direct、HTTP CONNECT、HTTPS CONNECT、SOCKS5 或 SOCKS5H 出网。
- Legacy L4 Bridge：保留原 `TCP -> AF_VSOCK` 透明转发能力，兼容不需要按请求代理的旧接入方式。

Gateway 不生成、不验证、不改写 proof；响应仍由 Enclave 按原 PoO frame
协议返回给中转站。

## 1. 部署拓扑

### 1.1 远程中转站模式

中转站部署在独立机器上，通过 VPC / 私网 LB 访问父 VM：

```text
中转站机器
  -> https://<parent_vm_private_ip>:15005/v1/proof/relay
  -> PoO Parent Gateway
  -> AF_VSOCK <enclave_cid>:5005
  -> Enclave PoO runtime
  -> AF_VSOCK lane port 18000-18999
  -> PoO Parent Gateway egress
  -> proxy/direct
  -> upstream:443
```

生产要求：

- `POO_PARENT_PROOF_RELAY_LISTEN` 绑定父 VM 私网地址或 `0.0.0.0:15005`。
- `POO_PARENT_AUTH_MODE=mtls`。
- 通过安全组、防火墙或私网 LB 限制只有中转站可访问 `15005`。
- admin listener 默认只绑定 `127.0.0.1:15007`；如需给 LB 探活开放，必须只开放到受控内网。

### 1.2 父 VM 本机中转站模式

中转站和 Gateway 部署在同一台父 VM：

```text
父 VM 本机中转站
  -> http://127.0.0.1:15005/v1/proof/relay
  -> PoO Parent Gateway
  -> AF_VSOCK <enclave_cid>:5005
  -> Enclave PoO runtime
  -> AF_VSOCK lane port 18000-18999
  -> PoO Parent Gateway egress
  -> proxy/direct
  -> upstream:443
```

生产要求：

- `POO_PARENT_PROOF_RELAY_LISTEN=127.0.0.1:15005`。
- 可以使用 `POO_PARENT_AUTH_MODE=none`，因为程序会拒绝 `none` 模式绑定非 loopback 地址。
- admin listener 继续绑定 `127.0.0.1:15007`。

## 2. 端口规划

| 端口 / 地址 | 默认值 | 方向 | 用途 |
|---|---:|---|---|
| Proof Relay API | `15005` | 中转站 -> Gateway | `POST /v1/proof/relay`，支持按请求传代理 |
| Legacy Ingress | 默认关闭 | 中转站 -> Gateway | 旧 L4 `TCP -> vsock` 透明转发 |
| Admin API | `127.0.0.1:15007` | 运维 / 探活 -> Gateway | `/healthz`、`/readyz`、`/metrics` |
| Enclave relay vsock | `5005` | Gateway -> Enclave | PoO control / relay frame stream |
| Enclave metrics vsock | `5006` | Gateway -> Enclave | readiness probe |
| Egress lane ports | `18000-18999` | Enclave -> Gateway | 单请求 egress lane，默认 `direct-vsock` |

生产默认使用：

```bash
POO_EGRESS_LANE_LISTEN_MODE=direct-vsock
```

只有本地开发或厂商 direct-vsock 不可用的临时诊断场景才使用：

```bash
POO_EGRESS_LANE_LISTEN_MODE=tcp-loopback
```

`tcp-loopback` 只能绑定 `127.0.0.1`，不能作为生产跨机器 egress lane 暴露。

## 3. 父 VM 前置检查

以下命令在 Enclave 父 VM 上执行。

确认系统架构：

```bash
uname -m
```

常见映射：

```text
x86_64 / amd64  -> linux/amd64
aarch64 / arm64 -> linux/arm64
```

确认 Enclave 已运行并记录 CID。

AWS Nitro：

```bash
sudo nitro-cli describe-enclaves
```

阿里云 Enclave：

```bash
sudo enclave-cli describe-enclaves
```

QingTian：

```bash
# 使用当前父 VM 已安装的 QingTian Enclave CLI 查询运行中的 Enclave。
# 目标是记录当前 Enclave CID。
```

后续示例统一使用：

```bash
export ENCLAVE_CID=4
```

确认父 VM 可以访问模型上游和代理：

```bash
curl -I --connect-timeout 5 https://api.openai.com/ || true
curl -I --connect-timeout 5 https://api.anthropic.com/ || true
```

如果账号使用代理，先用父 VM 自身验证代理可连通：

```bash
curl -I --connect-timeout 10 \
  --proxy "http://user:pass@proxy.example.com:8080" \
  https://api.openai.com/ || true
```

代理地址是否有效由中转站和代理服务自身负责；Gateway 只解析代理 URL 并按该 URL 出网。

## 4. 在父 VM 拉取代码

安装基础工具。Ubuntu / Debian：

```bash
sudo apt-get update
sudo apt-get install -y git ca-certificates curl build-essential
```

Amazon Linux / RHEL / CentOS：

```bash
sudo yum install -y git ca-certificates curl gcc make
```

安装 Go。当前 `go.mod` 要求 Go `1.24`，父 VM 上必须使用 Go `1.24`
或更高版本：

```bash
go version
```

如果系统包版本不足，优先使用公司内部标准 Go 包或内部制品库中已校验的
Go tarball。没有内部包时，可按架构安装 Go `1.24.x`。生产环境必须先从
公司制品库、发布记录或 Go 官方下载页取得对应 tarball 的 SHA256，并替换
下面的 `GO_TARBALL_SHA256`：

```bash
set -euo pipefail

cd /tmp
GO_VERSION=1.24.0
GO_TARBALL_SHA256="<replace-with-expected-sha256>"
ARCH=$(uname -m)

case "$ARCH" in
  x86_64) GO_ARCH=amd64 ;;
  aarch64) GO_ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

GO_TARBALL="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"

if [ "$GO_TARBALL_SHA256" = "<replace-with-expected-sha256>" ]; then
  echo "GO_TARBALL_SHA256 must be set before installing Go" >&2
  exit 1
fi

curl -fLO "https://go.dev/dl/${GO_TARBALL}"
printf '%s  %s\n' "$GO_TARBALL_SHA256" "$GO_TARBALL" | sha256sum -c -

sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf "$GO_TARBALL"

echo 'export PATH=/usr/local/go/bin:$PATH' | sudo tee /etc/profile.d/go.sh >/dev/null

export PATH=/usr/local/go/bin:$PATH
go version
```

拉取当前功能分支。功能合并到 `main` 后，把 `GIT_REF` 改成 `main`：

```bash
sudo mkdir -p /opt/poo
sudo chown "$USER":"$USER" /opt/poo

cd /opt/poo
git clone https://github.com/Demo-9876/tcp-to-vsock-gateway.git
cd tcp-to-vsock-gateway

GIT_REF=docs/poo-parent-gateway-request-proxy-design
git fetch origin "$GIT_REF"
git checkout "$GIT_REF"
git pull --ff-only origin "$GIT_REF"
```

确认当前代码版本：

```bash
git rev-parse --abbrev-ref HEAD
git rev-parse --short=12 HEAD
```

## 5. 构建

在父 VM 本机直接构建：

```bash
cd /opt/poo/tcp-to-vsock-gateway

go test ./...
go vet ./...
```

构建当前父 VM 架构 binary：

```bash
VERSION=poo-parent-gateway-$(date -u +%Y%m%d%H%M%S)
COMMIT=$(git rev-parse --short=12 HEAD)
BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)

mkdir -p dist
go build \
  -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
  -o dist/tcp-to-vsock-gateway \
  ./cmd/tcp-to-vsock-gateway
```

查看版本和校验值：

```bash
./dist/tcp-to-vsock-gateway --version
sha256sum ./dist/tcp-to-vsock-gateway
```

如果在非父 VM 的构建机交叉编译，按目标架构指定：

```bash
# linux/amd64: 阿里云 Enclave、QingTian 或 x86_64 父 VM
GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
  -o dist/tcp-to-vsock-gateway_linux_amd64 \
  ./cmd/tcp-to-vsock-gateway

# linux/arm64: arm64 父 VM
GOOS=linux GOARCH=arm64 go build \
  -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
  -o dist/tcp-to-vsock-gateway_linux_arm64 \
  ./cmd/tcp-to-vsock-gateway
```

## 6. 安装目录和运行用户

以下命令在父 VM 上执行：

```bash
sudo useradd --system --no-create-home --shell /sbin/nologin poo-gateway 2>/dev/null || true

sudo mkdir -p /etc/poo-parent-gateway
sudo mkdir -p /var/lib/poo-parent-gateway/spool
sudo mkdir -p /var/log/poo-parent-gateway

sudo chown -R poo-gateway:poo-gateway /var/lib/poo-parent-gateway
sudo chown -R poo-gateway:poo-gateway /var/log/poo-parent-gateway
sudo chmod 0700 /var/lib/poo-parent-gateway/spool
```

安装 binary：

```bash
cd /opt/poo/tcp-to-vsock-gateway

sudo install -o root -g root -m 0755 \
  ./dist/tcp-to-vsock-gateway \
  /usr/local/bin/tcp-to-vsock-gateway

/usr/local/bin/tcp-to-vsock-gateway --version
```

## 7. 生成远程模式 mTLS 证书

如果中转站和 Gateway 同机，且 Proof Relay API 只监听 `127.0.0.1`，可以跳过本节。

下面命令生成一套示例 CA、Gateway server cert 和中转站 client cert。生产环境建议接入公司证书签发和轮换系统，示例证书只用于说明文件格式和命令。

在安全的运维机或父 VM 上执行：

```bash
mkdir -p /tmp/poo-mtls
cd /tmp/poo-mtls

openssl genrsa -out client-ca.key 4096
openssl req -x509 -new -nodes \
  -key client-ca.key \
  -sha256 \
  -days 3650 \
  -subj "/CN=poo-parent-gateway-client-ca" \
  -out client-ca.pem

openssl genrsa -out server-key.pem 2048
openssl req -new \
  -key server-key.pem \
  -subj "/CN=poo-parent-gateway" \
  -out server.csr
```

把 `10.0.12.34` 替换成父 VM 私网 IP，把 DNS 替换成实际服务名：

```bash
cat > server-ext.cnf <<EOF
subjectAltName = IP:10.0.12.34,DNS:poo-parent-gateway.internal
extendedKeyUsage = serverAuth
EOF

openssl x509 -req \
  -in server.csr \
  -CA client-ca.pem \
  -CAkey client-ca.key \
  -CAcreateserial \
  -out server.pem \
  -days 825 \
  -sha256 \
  -extfile server-ext.cnf
```

生成中转站 client cert。推荐使用稳定 URI SAN 作为 client identity：

```bash
openssl genrsa -out sub2api-client-key.pem 2048
openssl req -new \
  -key sub2api-client-key.pem \
  -subj "/CN=sub2api-prod" \
  -out sub2api-client.csr

cat > sub2api-client-ext.cnf <<EOF
subjectAltName = URI:spiffe://poo/sub2api-prod
extendedKeyUsage = clientAuth
EOF

openssl x509 -req \
  -in sub2api-client.csr \
  -CA client-ca.pem \
  -CAkey client-ca.key \
  -CAcreateserial \
  -out sub2api-client.pem \
  -days 825 \
  -sha256 \
  -extfile sub2api-client-ext.cnf
```

安装 Gateway 侧证书：

```bash
sudo install -o root -g root -m 0644 /tmp/poo-mtls/client-ca.pem /etc/poo-parent-gateway/client-ca.pem
sudo install -o root -g root -m 0644 /tmp/poo-mtls/server.pem /etc/poo-parent-gateway/server.pem
sudo install -o root -g poo-gateway -m 0640 /tmp/poo-mtls/server-key.pem /etc/poo-parent-gateway/server-key.pem
```

把以下文件安全分发给中转站机器：

```text
client-ca.pem
sub2api-client.pem
sub2api-client-key.pem
```

## 8. 编写 Gateway 配置

### 8.1 远程中转站生产配置

把 `10.0.12.34` 替换成父 VM 私网 IP：

```bash
export ENCLAVE_CID=4
export PARENT_VM_PRIVATE_IP=10.0.12.34

sudo tee /etc/poo-parent-gateway/env >/dev/null <<EOF
POO_PARENT_PROOF_RELAY_LISTEN=${PARENT_VM_PRIVATE_IP}:15005
POO_PARENT_LEGACY_INGRESS_LISTEN=
POO_PARENT_ADMIN_LISTEN=127.0.0.1:15007
POO_PARENT_AUTH_MODE=mtls
POO_PARENT_MTLS_CA_FILE=/etc/poo-parent-gateway/client-ca.pem
POO_PARENT_MTLS_CERT_FILE=/etc/poo-parent-gateway/server.pem
POO_PARENT_MTLS_KEY_FILE=/etc/poo-parent-gateway/server-key.pem
POO_PARENT_CLIENT_POLICY_FILE=/etc/poo-parent-gateway/client-policy.yaml

POO_PARENT_VSOCK_CID=${ENCLAVE_CID}
POO_PARENT_VSOCK_PORT=5005
POO_PARENT_VSOCK_METRICS_PORT=5006
POO_PARENT_SHUTDOWN_TIMEOUT_MS=300000
POO_PARENT_LOG_LEVEL=info

POO_EGRESS_LANE_LISTEN_MODE=direct-vsock
POO_EGRESS_PORT_RANGE=18000-18999
POO_EGRESS_PORT_COOLDOWN_MS=5000
POO_EGRESS_ROUTE_IDLE_TTL_MS=300000
POO_EGRESS_LEASE_TTL_MS=30000
POO_EGRESS_DEFAULT_CONNECT_TIMEOUT_MS=10000
POO_EGRESS_MAX_ACTIVE_ROUTES=1000
POO_EGRESS_MAX_ACTIVE_LEASES=4096
POO_EGRESS_DEFAULT_ROUTE_CONCURRENCY=1
POO_EGRESS_MAX_ROUTE_CONCURRENCY=16
POO_EGRESS_ALLOWED_TARGETS=

POO_RELAY_MAX_METADATA_BYTES=16384
POO_RELAY_MAX_REQ_HEAD_BYTES=1048576
POO_RELAY_MAX_FRAME_BYTES=67108864
POO_RELAY_MAX_REQUEST_BYTES=268435456
POO_RELAY_MAX_BUFFERED_BYTES=4194304
POO_RELAY_SPOOL_DIR=/var/lib/poo-parent-gateway/spool
POO_RELAY_MAX_SPOOL_BYTES=1073741824
POO_RELAY_IO_TIMEOUT_MS=300000

TTVG_CONNECT_TIMEOUT=5s
TTVG_IDLE_TIMEOUT=300s
TTVG_READY_CACHE_TTL=1s
TTVG_MAX_CONNS=1024
TTVG_TCP_KEEPALIVE=30s
EOF
```

`POO_EGRESS_ALLOWED_TARGETS=` 为空时，程序使用内置主流模型厂商 `host:port`
默认集合。查看内置集合：

```bash
/usr/local/bin/tcp-to-vsock-gateway --print-default-targets
```

如果生产只允许少数上游，可显式收窄：

```bash
sudo sed -i.bak \
  's#^POO_EGRESS_ALLOWED_TARGETS=.*#POO_EGRESS_ALLOWED_TARGETS=api.openai.com:443,api.anthropic.com:443#' \
  /etc/poo-parent-gateway/env
```

### 8.2 client policy

远程 mTLS 模式必须配置 client policy：

```bash
sudo tee /etc/poo-parent-gateway/client-policy.yaml >/dev/null <<'EOF'
clients:
  - san_uri: spiffe://poo/sub2api-prod
    allowed_targets:
      - api.openai.com:443
      - api.anthropic.com:443
      - api.deepseek.com:443
      - api.moonshot.cn:443
    max_concurrency: 4
EOF

sudo chmod 0644 /etc/poo-parent-gateway/client-policy.yaml
```

说明：

- `san_uri` 必须和中转站 client certificate 的 URI SAN 一致。
- `allowed_targets` 是该 client 的进一步收窄；不能放大全局 `POO_EGRESS_ALLOWED_TARGETS`。
- `max_concurrency` 是单个 route key 的并发上限，最终仍受 `POO_EGRESS_MAX_ROUTE_CONCURRENCY` 限制。
- 如果不想按 client 单独收窄目标，可以省略 `allowed_targets` 或配置为空列表。

### 8.3 父 VM 本机中转站配置

同机模式可以不启用 mTLS：

```bash
export ENCLAVE_CID=4

sudo tee /etc/poo-parent-gateway/env >/dev/null <<EOF
POO_PARENT_PROOF_RELAY_LISTEN=127.0.0.1:15005
POO_PARENT_LEGACY_INGRESS_LISTEN=
POO_PARENT_ADMIN_LISTEN=127.0.0.1:15007
POO_PARENT_AUTH_MODE=none
POO_PARENT_CLIENT_POLICY_FILE=

POO_PARENT_VSOCK_CID=${ENCLAVE_CID}
POO_PARENT_VSOCK_PORT=5005
POO_PARENT_VSOCK_METRICS_PORT=5006
POO_PARENT_SHUTDOWN_TIMEOUT_MS=300000
POO_PARENT_LOG_LEVEL=info

POO_EGRESS_LANE_LISTEN_MODE=direct-vsock
POO_EGRESS_PORT_RANGE=18000-18999
POO_EGRESS_PORT_COOLDOWN_MS=5000
POO_EGRESS_ROUTE_IDLE_TTL_MS=300000
POO_EGRESS_LEASE_TTL_MS=30000
POO_EGRESS_DEFAULT_CONNECT_TIMEOUT_MS=10000
POO_EGRESS_MAX_ACTIVE_ROUTES=1000
POO_EGRESS_MAX_ACTIVE_LEASES=4096
POO_EGRESS_DEFAULT_ROUTE_CONCURRENCY=1
POO_EGRESS_MAX_ROUTE_CONCURRENCY=16
POO_EGRESS_ALLOWED_TARGETS=

POO_RELAY_MAX_METADATA_BYTES=16384
POO_RELAY_MAX_REQ_HEAD_BYTES=1048576
POO_RELAY_MAX_FRAME_BYTES=67108864
POO_RELAY_MAX_REQUEST_BYTES=268435456
POO_RELAY_MAX_BUFFERED_BYTES=4194304
POO_RELAY_SPOOL_DIR=/var/lib/poo-parent-gateway/spool
POO_RELAY_MAX_SPOOL_BYTES=1073741824
POO_RELAY_IO_TIMEOUT_MS=300000

TTVG_CONNECT_TIMEOUT=5s
TTVG_IDLE_TIMEOUT=300s
TTVG_READY_CACHE_TTL=1s
TTVG_MAX_CONNS=1024
TTVG_TCP_KEEPALIVE=30s
EOF
```

### 8.4 兼容旧 L4 透明网关

如仍需保留旧中转站直接发送 PoO frame 到 TCP listener 的链路，可额外开启：

```bash
sudo sed -i.bak \
  's#^POO_PARENT_LEGACY_INGRESS_LISTEN=.*#POO_PARENT_LEGACY_INGRESS_LISTEN=127.0.0.1:15006#' \
  /etc/poo-parent-gateway/env
```

旧 L4 listener 不支持按请求传入 `proxyURL`；需要账号级代理或请求级代理时，必须接入 Proof Relay API。

## 9. 配置校验

加载环境变量并检查。远程 mTLS 模式下，server key 通常只允许 root 或
`poo-gateway` 组读取，因此建议用 root 执行 `--check-config`：

```bash
sudo bash -lc 'set -a; . /etc/poo-parent-gateway/env; set +a; /usr/local/bin/tcp-to-vsock-gateway --check-config'
```

期望输出：

```text
configuration ok
```

常见校验失败：

```text
POO_PARENT_AUTH_MODE=none requires POO_PARENT_PROOF_RELAY_LISTEN to bind loopback
```

说明远程模式没有开启 mTLS。处理方式是配置 `POO_PARENT_AUTH_MODE=mtls` 和证书文件，或把 listener 改成 `127.0.0.1:15005`。

```text
POO_EGRESS_ALLOWED_TARGETS item "xxx" must be host:port
```

说明 allowlist 必须写成 `host:port`，v1 只支持 `:443`。

## 10. systemd 部署

写入 service：

```bash
sudo tee /etc/systemd/system/poo-parent-gateway.service >/dev/null <<'EOF'
[Unit]
Description=PoO Parent Gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/poo-parent-gateway/env
ExecStart=/usr/local/bin/tcp-to-vsock-gateway
User=poo-gateway
Group=poo-gateway
Restart=always
RestartSec=2s
LimitNOFILE=65536
TimeoutStopSec=330s

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/poo-parent-gateway /var/log/poo-parent-gateway

[Install]
WantedBy=multi-user.target
EOF
```

如果某个父 VM 发行版的 systemd hardening 阻断 `AF_VSOCK` 或读取证书文件，先查看日志，再最小化放宽对应项：

```bash
sudo journalctl -u poo-parent-gateway -n 100 --no-pager
```

启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable poo-parent-gateway
sudo systemctl restart poo-parent-gateway
sudo systemctl status poo-parent-gateway --no-pager
```

查看日志：

```bash
sudo journalctl -u poo-parent-gateway -n 100 --no-pager
sudo journalctl -u poo-parent-gateway -f
```

## 11. 父 VM 本机验证

检查监听：

```bash
sudo ss -lntp | grep -E '15005|15007|15006|18000|18999' || true
```

`18000-18999` 是按请求临时 lane，不一定长期出现在监听列表中。

检查健康状态：

```bash
curl -i http://127.0.0.1:15007/healthz
curl -i http://127.0.0.1:15007/readyz
curl -s http://127.0.0.1:15007/metrics | head -80
```

期望：

```text
/healthz -> 200
/readyz  -> 200
```

查看关键指标：

```bash
curl -s http://127.0.0.1:15007/metrics | grep -E \
  'ttvg_proof_relay|ttvg_egress|ttvg_vsock|ttvg_readiness|ttvg_connections'
```

## 12. 中转站接入配置

中转站 adapter 调用 Proof Relay API：

```text
POST /v1/proof/relay
Content-Type: application/vnd.poo.frames
X-PoO-Proxy-URL: socks5h://user:pass@proxy.example.com:1080
X-PoO-Tenant-ID: optional-tenant-id
X-PoO-Account-ID: optional-account-id
X-PoO-Request-ID: optional-request-id

<原 PoO Relay Frame Protocol bytes: REQ_HEAD + REQ_BODY>
```

`X-PoO-Proxy-URL` 为空或缺失表示直连。支持：

```text
http://host:port
https://host:port
socks5://host:port
socks5h://host:port
http://user:pass@host:port
socks5h://user:pass@host:port
```

中转站必须把 `X-PoO-Proxy-URL` 当作 secret 处理，禁止写入 access log、APM、自定义 debug log 或错误响应。

本机中转站示例：

```bash
curl -sS \
  -X POST http://127.0.0.1:15005/v1/proof/relay \
  -H 'Content-Type: application/vnd.poo.frames' \
  -H 'X-PoO-Request-ID: smoke-local-001' \
  -H 'X-PoO-Proxy-URL: socks5h://user:pass@proxy.example.com:1080' \
  --data-binary @/tmp/poo-request.frames \
  --dump-header /tmp/poo-response.headers \
  --output /tmp/poo-response.frames
```

远程 mTLS 示例：

```bash
curl -sS \
  --cacert /etc/poo-client/client-ca.pem \
  --cert /etc/poo-client/sub2api-client.pem \
  --key /etc/poo-client/sub2api-client-key.pem \
  -X POST https://10.0.12.34:15005/v1/proof/relay \
  -H 'Content-Type: application/vnd.poo.frames' \
  -H 'X-PoO-Request-ID: smoke-remote-001' \
  -H 'X-PoO-Proxy-URL: http://user:pass@proxy.example.com:8080' \
  --data-binary @/tmp/poo-request.frames \
  --dump-header /tmp/poo-response.headers \
  --output /tmp/poo-response.frames
```

`/tmp/poo-request.frames` 必须由中转站 adapter 按原 PoO Relay Frame Protocol
生成，body 中只包含 `REQ_HEAD + REQ_BODY` 两帧。Gateway 会在内部替换
`REQ_HEAD.egress_port`，然后把 frame stream 传给 Enclave。

## 13. 端到端验收

在中转站侧发起一次直连请求：

```bash
unset POO_TEST_PROXY_URL
curl -i -X POST "http://<relay_host>:<relay_port>/v1/chat/completions" \
  -H "Authorization: Bearer <relay_api_key>" \
  -H "Content-Type: application/json" \
  -H "X-TEE-Proof: required" \
  -d '{
    "model": "<model>",
    "messages": [{"role": "user", "content": "用一句话回答：PoO 测试是否开始？"}],
    "stream": false
  }'
```

再发起一次代理请求。具体开关以中转站实现为准，核心是 adapter 最终要把账号绑定代理传给 `X-PoO-Proxy-URL`：

```bash
export POO_TEST_PROXY_URL='socks5h://user:pass@proxy.example.com:1080'

curl -i -X POST "http://<relay_host>:<relay_port>/v1/chat/completions" \
  -H "Authorization: Bearer <relay_api_key>" \
  -H "Content-Type: application/json" \
  -H "X-TEE-Proof: required" \
  -d '{
    "model": "<model>",
    "messages": [{"role": "user", "content": "用一句话回答：代理链路是否开始？"}],
    "stream": false
  }'
```

验收标准：

- 直连请求返回业务响应，并能拿到 Enclave 生成的 proof。
- 代理请求返回业务响应，并能拿到 Enclave 生成的 proof。
- proof verifier 对直连和代理请求均验证通过。
- Gateway `/metrics` 中 `ttvg_proof_relay_requests_total` 增长。
- 代理请求期间 `ttvg_egress_leases_active`、`ttvg_egress_lane_ports_active` 短暂增长。
- `ttvg_egress_proxy_failures_total{reason="connect"}` 没有异常增长。
- 日志不包含 `Authorization`、请求体、响应体、nonce、proof trailer、完整 proxy URL、proxy username 或 proxy password。

## 14. 运行期观测

查看服务：

```bash
sudo systemctl status poo-parent-gateway --no-pager
sudo journalctl -u poo-parent-gateway -n 200 --no-pager
```

查看 readiness：

```bash
curl -i http://127.0.0.1:15007/readyz
```

查看核心指标：

```bash
curl -s http://127.0.0.1:15007/metrics | grep -E \
  'ttvg_proof_relay_requests_total|ttvg_proof_relay_preflight_failures_total|ttvg_proof_relay_aborts_total|ttvg_egress_routes_cached_active|ttvg_egress_routes_cached_idle|ttvg_egress_leases_active|ttvg_egress_lane_ports_active|ttvg_egress_route_failures_total|ttvg_egress_proxy_failures_total|ttvg_egress_late_connections_total|ttvg_vsock_dial_errors_total|ttvg_readiness_last_ready'
```

指标含义：

| 指标 | 含义 |
|---|---|
| `ttvg_proof_relay_requests_total` | Proof Relay API 请求数 |
| `ttvg_proof_relay_preflight_failures_total` | 调用 Enclave 前失败的请求 |
| `ttvg_proof_relay_aborts_total` | 调用 Enclave 后读写中断 |
| `ttvg_egress_routes_cached_active` | route cache 中有活跃 lease 的 route |
| `ttvg_egress_routes_cached_idle` | route cache 中等待 idle TTL 淘汰的 route |
| `ttvg_egress_leases_active` | 当前活跃请求 lease |
| `ttvg_egress_lane_ports_active` | 当前活跃 lane listener |
| `ttvg_egress_route_failures_total` | route 分配失败 |
| `ttvg_egress_proxy_failures_total` | 代理连接或握手失败 |
| `ttvg_egress_late_connections_total` | lease 释放后 Enclave 才连接 lane |
| `ttvg_readiness_last_ready` | 最近一次 readiness 结果 |

## 15. 滚动发布

单节点发布：

```bash
set -euo pipefail

cd /opt/poo/tcp-to-vsock-gateway
git fetch origin
git checkout docs/poo-parent-gateway-request-proxy-design
git pull --ff-only origin docs/poo-parent-gateway-request-proxy-design

go test ./...

VERSION=poo-parent-gateway-$(date -u +%Y%m%d%H%M%S)
COMMIT=$(git rev-parse --short=12 HEAD)
BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go build \
  -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
  -o dist/tcp-to-vsock-gateway.new \
  ./cmd/tcp-to-vsock-gateway

sudo bash -lc 'set -a; . /etc/poo-parent-gateway/env; set +a; /opt/poo/tcp-to-vsock-gateway/dist/tcp-to-vsock-gateway.new --check-config'

sudo systemctl stop poo-parent-gateway
sudo cp /usr/local/bin/tcp-to-vsock-gateway \
  /usr/local/bin/tcp-to-vsock-gateway.bak.$(date -u +%Y%m%d%H%M%S)

sudo install -o root -g root -m 0755 \
  dist/tcp-to-vsock-gateway.new \
  /usr/local/bin/tcp-to-vsock-gateway

sudo systemctl start poo-parent-gateway
curl -i http://127.0.0.1:15007/readyz
```

多节点发布：

1. 从 LB 或中转站路由中摘除 1 台父 VM。
2. 等待当前节点活跃请求下降。
3. 替换 binary 并重启 Gateway。
4. `/readyz` 返回 `200` 后发起直连和代理 smoke test。
5. 把节点加回 LB。
6. 继续下一台。

等待活跃请求：

```bash
watch -n 1 \
  "curl -s http://127.0.0.1:15007/metrics | grep -E 'ttvg_egress_leases_active|ttvg_egress_lane_ports_active|ttvg_connections_active'"
```

## 16. 回滚

发布前备份旧 binary：

```bash
sudo cp /usr/local/bin/tcp-to-vsock-gateway \
  /usr/local/bin/tcp-to-vsock-gateway.bak.$(date -u +%Y%m%d%H%M%S)
```

回滚到上一版：

```bash
sudo systemctl stop poo-parent-gateway
sudo cp /usr/local/bin/tcp-to-vsock-gateway.bak.<timestamp> \
  /usr/local/bin/tcp-to-vsock-gateway
sudo chmod 0755 /usr/local/bin/tcp-to-vsock-gateway
sudo systemctl start poo-parent-gateway
curl -i http://127.0.0.1:15007/readyz
```

如果需要临时禁用按请求代理能力，只保留旧 L4 bridge，必须按中转站部署拓扑选择
legacy listener 绑定地址。legacy L4 bridge 不支持 `X-PoO-Proxy-URL`，因此只能恢复无代理
或调用方已自行管理 egress port 的旧链路。

父 VM 本机中转站模式：

```bash
set -euo pipefail

sudo cp /etc/poo-parent-gateway/env /etc/poo-parent-gateway/env.bak.$(date -u +%Y%m%d%H%M%S)
sudo sed -i \
  -e 's#^POO_PARENT_PROOF_RELAY_LISTEN=.*#POO_PARENT_PROOF_RELAY_LISTEN=#' \
  -e 's#^POO_PARENT_AUTH_MODE=.*#POO_PARENT_AUTH_MODE=none#' \
  -e 's#^POO_PARENT_LEGACY_INGRESS_LISTEN=.*#POO_PARENT_LEGACY_INGRESS_LISTEN=127.0.0.1:15006#' \
  /etc/poo-parent-gateway/env

sudo bash -lc 'set -a; . /etc/poo-parent-gateway/env; set +a; /usr/local/bin/tcp-to-vsock-gateway --check-config'
sudo systemctl restart poo-parent-gateway
```

远程中转站模式：

```bash
set -euo pipefail

export PARENT_VM_PRIVATE_IP=10.0.12.34

sudo cp /etc/poo-parent-gateway/env /etc/poo-parent-gateway/env.bak.$(date -u +%Y%m%d%H%M%S)
sudo sed -i \
  -e 's#^POO_PARENT_PROOF_RELAY_LISTEN=.*#POO_PARENT_PROOF_RELAY_LISTEN=#' \
  -e 's#^POO_PARENT_AUTH_MODE=.*#POO_PARENT_AUTH_MODE=none#' \
  -e "s#^POO_PARENT_LEGACY_INGRESS_LISTEN=.*#POO_PARENT_LEGACY_INGRESS_LISTEN=${PARENT_VM_PRIVATE_IP}:15006#" \
  /etc/poo-parent-gateway/env

sudo bash -lc 'set -a; . /etc/poo-parent-gateway/env; set +a; /usr/local/bin/tcp-to-vsock-gateway --check-config'
sudo systemctl restart poo-parent-gateway
sudo ss -lntp | grep ':15006'
curl -i http://127.0.0.1:15007/readyz
```

远程模式执行前，需要同步调整安全组、防火墙、私网 LB 或中转站配置，使中转站能访问
`<parent_vm_private_ip>:15006`。如果不希望 legacy listener 使用新端口，也可以改用原
Proof Relay API 的 `15005`，但必须先确认该端口不会同时被其它 listener 占用。

## 17. 常见故障

### `/readyz` 返回 503

检查：

```bash
grep -E 'POO_PARENT_VSOCK_CID|POO_PARENT_VSOCK_METRICS_PORT' /etc/poo-parent-gateway/env
sudo journalctl -u poo-parent-gateway -n 100 --no-pager
curl -s http://127.0.0.1:15007/metrics | grep -E 'ttvg_readiness|ttvg_vsock'
```

常见原因：

- Enclave CID 已变化，但配置仍是旧 CID。
- Enclave runtime 未启动完成。
- Enclave metrics vsock port 不是 `5006`。
- systemd hardening 或容器 runtime 阻断 `AF_VSOCK`。

### 中转站收到 `invalid_proxy_url`

检查中转站传入的代理 URL：

```text
必须是 absolute URL
必须包含 host:port
scheme 必须是 http、https、socks5、socks5h
不能包含 path、query、fragment
不能包含控制字符
```

正确示例：

```text
http://user:pass@proxy.example.com:8080
socks5h://user:pass@proxy.example.com:1080
```

错误示例：

```text
proxy.example.com:8080
http://proxy.example.com:8080/path
socks5://proxy.example.com
```

### 中转站收到 `target_not_allowed`

查看默认 allowlist：

```bash
/usr/local/bin/tcp-to-vsock-gateway --print-default-targets
```

检查全局配置和 client policy：

```bash
grep '^POO_EGRESS_ALLOWED_TARGETS=' /etc/poo-parent-gateway/env
cat /etc/poo-parent-gateway/client-policy.yaml
```

目标必须以 `host:port` 配置，v1 只支持 `:443`。

### 代理请求超时或失败

在父 VM 上验证代理本身：

```bash
curl -I --connect-timeout 10 \
  --proxy "http://user:pass@proxy.example.com:8080" \
  https://api.openai.com/ || true
```

查看 Gateway 指标：

```bash
curl -s http://127.0.0.1:15007/metrics | grep -E \
  'ttvg_egress_proxy_failures_total|ttvg_egress_route_failures_total|ttvg_egress_late_connections_total'
```

常见原因：

- 代理地址、账号或密码错误。
- 代理不支持 CONNECT 到目标 `host:443`。
- 父 VM 到代理的安全组 / 路由不通。
- `POO_EGRESS_LEASE_TTL_MS` 太短，Enclave 尚未连接 lane lease 已过期。

### `operation not permitted`

通常是父 VM 权限或容器 runtime 限制 `AF_VSOCK`。生产推荐 host binary +
systemd 部署。若必须容器化，需要显式验证容器允许创建 `socket(AF_VSOCK, ...)`。

## 18. 上线检查清单

- 已记录 Enclave CID，且 `POO_PARENT_VSOCK_CID` 与当前 CID 一致。
- `go test ./...` 通过。
- `/usr/local/bin/tcp-to-vsock-gateway --version` 输出符合本次发布 commit。
- `sudo bash -lc 'set -a; . /etc/poo-parent-gateway/env; set +a; /usr/local/bin/tcp-to-vsock-gateway --check-config'` 输出 `configuration ok`。
- `systemctl status poo-parent-gateway` 为 running。
- `/healthz` 返回 `200`。
- `/readyz` 返回 `200`。
- 本机模式 Proof Relay API 只绑定 `127.0.0.1`。
- 远程模式 Proof Relay API 使用 `POO_PARENT_AUTH_MODE=mtls`。
- 远程模式安全组 / 防火墙只允许中转站访问 `15005`。
- admin listener 未暴露公网。
- 直连请求 proof verifier 通过。
- 代理请求 proof verifier 通过。
- Gateway 日志不包含请求体、响应体、Authorization、nonce、proof trailer 或完整 proxy URL。
- 中转站日志不包含 `X-PoO-Proxy-URL` 原文、proxy username 或 proxy password。
- `ttvg_egress_proxy_failures_total`、`ttvg_egress_route_failures_total` 无异常增长。
