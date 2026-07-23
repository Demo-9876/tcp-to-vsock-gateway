# TCP-to-vsock Gateway 技术方案

状态：草案

本文档定义一个最精简但可满足生产部署的 L4 透明
`network-to-vsock gateway`。它用于把普通 TCP 连接桥接到 Enclave 的
AF_VSOCK relay 端口，让中转站可以通过网络访问部署在 Enclave 父 VM 上
的 `proof-of-observation` runtime。

核心定位：

```text
TCP client / relay service
  -> tcp-to-vsock-gateway
  -> AF_VSOCK <enclave_cid>:<enclave_port>
  -> proof-of-observation /attest
```

gateway 不是 proof verifier，不是 relay adapter，也不是 Enclave 生命周期
管理器。它只做一件事：在 TCP 字节流和 AF_VSOCK 字节流之间做透明转发。

## 1. 背景

`proof-of-observation` 当前已经支持三类 Enclave evidence profile：

- AWS Nitro Enclave：`TEE_PROFILE=nitro`
- 阿里云 Enclave：`TEE_PROFILE=aliyun-vtpm`
- 华为云 QingTian Enclave：`TEE_PROFILE=qingtian`

三类 Enclave 的 evidence 生成方式、PCR / measurement、证书链和用户侧
trust config 不同，但父 VM 到 Enclave 的业务 relay 入口已经收敛到同一类
字节流模型：

- Enclave runtime 在 vsock control port `5005` 上监听业务请求。
- Enclave runtime 在 vsock metrics port `5006` 上暴露运行状态。
- 中转站按 `TEE Relay Frame Protocol v1` 发送 `REQ_HEAD` / `REQ_BODY`。
- Enclave 返回 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER`。
- `RESP_TRAILER` 中携带 profile-specific proof，gateway 不需要理解其内容。

因此，只要 gateway 保持 L4 透明，不解析、不重写 frame，就可以做成一个
云厂商无关的统一版本。

## 2. 目标

gateway 必须满足：

- 接受来自可信中转站的 TCP 连接。
- 每个 TCP 连接对应打开一个 `AF_VSOCK(<enclave_cid>, <enclave_port>)`
  连接。
- 双向复制字节，直到任一侧关闭、超时或发生错误。
- 不解析、不缓存完整请求、不缓存完整响应。
- 不修改任何 relay frame、HTTP body、SSE chunk、multipart body 或 proof。
- 通过配置支持 AWS Nitro Enclave、阿里云 Enclave、华为云 QingTian
  Enclave。
- 错误时 fail closed。
- 暴露健康检查和 metrics，便于生产运维。
- 实现足够小，便于审计。

## 3. 非目标

gateway 不做以下事情：

- 不解析 `TEE Relay Frame Protocol v1`。
- 不生成、验证、缓存、转换 proof JSON。
- 不理解 `nitro` / `aliyun-vtpm` / `qingtian` evidence 内部结构。
- 不终止到上游模型服务商的 TLS。
- 不做上游 egress proxy。
- 不启动、停止、构建 Enclave。
- 不管理 PCR allowlist、release manifest 或 verifier trust config。
- v1 不内置复杂应用层鉴权。

鉴权优先交给基础设施完成，例如安全组、VPC 私网、IP allowlist、防火墙、
内网负载均衡或服务网格。当前假设中转站和 gateway 由同一家中转站管理方
控制，因此 v1 不强制内置 mTLS。后续如果要跨组织暴露 gateway，可以再增加
mTLS 或签名鉴权。

## 4. 部署拓扑

### 4.1 与中转站同父 VM 部署

这是最简单的形态：中转站和 gateway 都部署在 Enclave 父 VM 上。

```text
relay container/process
  -> TCP 127.0.0.1:15005
  -> gateway on parent VM
  -> AF_VSOCK cid=4 port=5005
  -> Enclave /attest
```

适用场景：

- 中转站跑在 Docker 容器中，容器默认 seccomp / capability 不方便直接创建
  `AF_VSOCK` socket。
- 希望把 vsock 权限收敛到一个很小的 host binary。
- 希望不同中转站实现都通过普通 TCP 接入。

### 4.2 中转站和 Enclave 分机器部署

多个中转站实例可以部署在普通机器或普通容器中，通过私网 TCP 访问部署在
Enclave 父 VM 上的 gateway。

```text
relay instance A ----\
relay instance B -----+-> TCP <gateway-private-ip>:15005
relay instance C ----/       -> gateway on Enclave parent VM
                              -> AF_VSOCK cid=4 port=5005
                              -> Enclave /attest
```

适用场景：

- 多个中转站实例共享一个或一组 proof Enclave。
- 中转站本身不部署在 Enclave 父 VM 上。
- 希望把 proof-of-observation 的 Enclave 资源独立运维。

生产要求：

- gateway 监听地址必须是私网地址。
- 安全组 / 防火墙只允许指定中转站实例访问。
- 中转站仍必须要求用户侧 proof 验证通过，不能只信任 gateway 可达。

### 4.3 生产高可用拓扑

长期生产部署不建议把所有流量压到单个 gateway / 单个 Enclave。推荐以
`gateway + Enclave runtime + egress proxy` 为一个最小故障域，横向部署多组
节点。

```text
relay instance A ----\
relay instance B -----+-> private L4 load balancer
relay instance C ----/       -> gateway node 1 -> AF_VSOCK cid=4 port=5005 -> Enclave 1
                              -> gateway node 2 -> AF_VSOCK cid=4 port=5005 -> Enclave 2
                              -> gateway node 3 -> AF_VSOCK cid=4 port=5005 -> Enclave 3
```

生产建议：

- 每个 gateway 进程只连接本机一个 Enclave endpoint。
- 私网 L4 load balancer 以 gateway `/readyz` 作为健康检查。健康检查有两种
  部署方式：
  - 同机 node-agent / sidecar 访问 gateway 本地 `127.0.0.1:<admin_port>`，
    再把结果暴露给 LB 或编排系统。
  - gateway admin listener 绑定到父 VM 私网管理 IP，由安全组只放行 LB health
    checker 或运维监控网段访问。
- 如果采用远程 LB 直接检查 `/readyz`，不能把 `TTVG_METRICS_ADDR` 只绑定到
  `127.0.0.1`；必须绑定到明确的私网管理地址，例如
  `<parent-vm-private-ip>:15006`。
- `/readyz` 失败的节点必须自动摘除。
- 中转站侧配置多个 gateway endpoint 或使用私网 LB 地址。
- 扩容单位是 `gateway + Enclave` 节点，而不是只扩 gateway。
- 每个节点独立记录当前 Enclave CID、EIF sha256、PCR / measurement 和
  `proof-of-observation` release 版本。
- Enclave 重启后 CID 可能变化，必须更新本节点 gateway 配置并重启 gateway，
  或通过运维脚本重新渲染环境文件。
- 单节点容量以 `active connections`、vsock dial latency、Enclave
  `queue_depth` / `in_flight`、proof 生成耗时和上游响应时间共同评估。

### 4.4 出网 egress 不属于本 gateway

gateway 只处理“中转站进入 Enclave”的 ingress 路径。

Enclave 内 `/attest` 访问真实上游的 egress 路径仍由
`proof-of-observation` 部署方案负责：

- AWS Nitro：父 VM direct vsock egress proxy。
- 阿里云 Enclave：父 VM direct vsock egress proxy。
- 华为 QingTian：生产路径使用华为 `qproxy` 加 parent-local TCP mapper。

这些组件不应塞进本 gateway，否则 gateway 会变成 provider-specific，失去
统一版本的价值。

## 5. 三家 Enclave 兼容模型

只要平台在父 VM 上提供 Linux `AF_VSOCK` 到 Enclave 的连接能力，一个
gateway binary 就够。

| Provider | gateway ingress transport | Enclave relay port | 是否需要单独版本 |
|---|---|---:|---|
| AWS Nitro Enclave | AF_VSOCK | `5005` | 不需要 |
| 阿里云 Enclave | AF_VSOCK | `5005` | 不需要 |
| 华为 QingTian Enclave | AF_VSOCK | `5005` | 不需要 |

三家差异保留在部署和验证层：

| 关注点 | AWS Nitro | 阿里云 | 华为 QingTian |
|---|---|---|---|
| Enclave lifecycle CLI | `nitro-cli` | `enclave-cli` | `qt` |
| Evidence profile | `nitro` | `aliyun-vtpm` | `qingtian` |
| Verifier trust config | AWS Nitro root / PCR0 | Aliyun TPM CA / PCR allowlist | QingTian root / PCR allowlist |
| Egress path | vsock proxy | vsock proxy | qproxy + TCP mapper |
| v1 gateway target platform | `linux/arm64` | `linux/amd64` | `linux/amd64` |
| gateway 行为 | 相同 | 相同 | 相同 |

gateway 只需要运行时配置：

- TCP listen address。
- Enclave CID。
- Enclave relay port，默认 `5005`。
- Enclave metrics port，默认 `5006`。
- 超时和并发限制。

## 6. L4 转发语义

每个 TCP 连接的处理流程：

1. 接受 TCP 连接。
2. 检查全局并发连接数。
3. 在限定时间内 dial `AF_VSOCK(enclave_cid, enclave_port)`。
4. 启动两个方向的复制：
   - TCP -> vsock
   - vsock -> TCP
5. 正确处理 half-close。
6. 正常 EOF 时只关闭对端写方向，并继续保留反方向复制。
7. 任一方向发生 error、timeout、连接生命周期超限或 shutdown drain 超时后关闭
   两侧连接。
8. 两个方向都完成后释放连接资源。
9. 记录连接时长、字节数、关闭原因和错误指标。

gateway 不需要知道 frame 边界。`TEE Relay Frame Protocol v1` 本身运行在
字节流之上，gateway 只要保证字节不被修改、不被重排即可。

实现要求：

- 不读取完整 request body。
- 不读取完整 response body。
- 每个方向使用固定大小 buffer，例如 `32 KiB` 到 `128 KiB`。
- 利用 socket 自然背压：vsock 写阻塞时停止继续从 TCP 读，TCP 写阻塞时停止
  继续从 vsock 读。
- TCP -> vsock 方向读到 EOF 通常只表示中转站已发送完整请求，不能立即关闭
  vsock -> TCP 方向；Enclave 仍可能继续返回 `RESP_HEAD`、`RESP_CHUNK` 和
  `RESP_TRAILER`。
- Go 的 TCP 连接可以使用 `CloseWrite` 传播 half-close，但具体 vsock package
  或云平台内核未必可靠暴露 vsock `CloseWrite`。如果 vsock 写方向不支持
  half-close，gateway 在 TCP -> vsock 读到 EOF 后应停止该方向 copy，保留
  vsock -> TCP 方向继续读取，不得立刻 full close。
- `TEE Relay Frame Protocol v1` 使用长度前缀 frame，请求结束由 frame 长度和
  协议状态决定，不依赖 socket EOF。因此“不向 vsock 传播 CloseWrite”不会影响
  Enclave 判断 `REQ_HEAD` / `REQ_BODY` 是否完整。
- vsock -> TCP 方向读到 EOF 后，如果 TCP -> vsock 方向仍未结束，应关闭 TCP
  连接，避免中转站继续写入一个已经没有 Enclave 响应方的请求。
- 不合成协议层 `ERR` frame，因为 gateway 不知道当前是否处于合法 frame
  边界。协议错误由 Enclave 内 `/attest` 处理。

## 7. 配置项

v1 建议先支持环境变量，后续可增加 TOML / YAML 配置文件。

| 名称 | 必填 | 默认值 | 说明 |
|---|---:|---|---|
| `TTVG_LISTEN_ADDR` | 是 | 无 | TCP 监听地址，例如 `127.0.0.1:15005` 或 `0.0.0.0:15005`。 |
| `TTVG_VSOCK_CID` | 是 | 无 | Enclave CID，由对应云厂商 CLI 输出。 |
| `TTVG_VSOCK_PORT` | 否 | `5005` | Enclave relay control port。 |
| `TTVG_METRICS_ADDR` | 否 | `127.0.0.1:15006` | HTTP admin listener。为空则关闭；生产模式必须启用。 |
| `TTVG_VSOCK_METRICS_PORT` | 否 | `5006` | Enclave metrics vsock port，用于主动 readiness 检查。 |
| `TTVG_READY_CACHE_TTL` | 否 | `1s` | `/readyz` 主动 vsock probe 结果缓存时间，避免 LB 高频探测放大 Enclave 压力。 |
| `TTVG_CONNECT_TIMEOUT` | 否 | `5s` | dial Enclave vsock 的超时时间。 |
| `TTVG_IDLE_TIMEOUT` | 否 | `300s` | 桥接连接无读写活动的超时时间。 |
| `TTVG_MAX_CONN_LIFETIME` | 否 | `0` | 单连接最大生命周期。`0` 表示不限制。 |
| `TTVG_MAX_CONNS` | 否 | `1024` | 最大并发桥接连接数。 |
| `TTVG_TCP_KEEPALIVE` | 否 | `30s` | TCP keepalive 周期。 |
| `TTVG_SHUTDOWN_TIMEOUT` | 否 | `300s` | 优雅退出等待时间。生产流式请求不建议低于 `300s`。 |
| `TTVG_LOG_LEVEL` | 否 | `info` | `debug` / `info` / `warn` / `error`。 |

单机 / 同机 agent 模式示例：

```bash
TTVG_LISTEN_ADDR=127.0.0.1:15005 \
TTVG_VSOCK_CID=4 \
TTVG_VSOCK_PORT=5005 \
TTVG_METRICS_ADDR=127.0.0.1:15006 \
TTVG_VSOCK_METRICS_PORT=5006 \
TTVG_READY_CACHE_TTL=1s \
TTVG_MAX_CONNS=512 \
tcp-to-vsock-gateway
```

远程 relay / 私网 LB 模式示例：

```bash
TTVG_LISTEN_ADDR=<parent-vm-private-ip>:15005 \
TTVG_VSOCK_CID=4 \
TTVG_VSOCK_PORT=5005 \
TTVG_METRICS_ADDR=<parent-vm-private-ip>:15006 \
TTVG_VSOCK_METRICS_PORT=5006 \
TTVG_READY_CACHE_TTL=1s \
TTVG_MAX_CONNS=512 \
tcp-to-vsock-gateway
```

远程模式下，安全组必须只放行 relay 子网访问 `TTVG_LISTEN_ADDR`，只放行 LB
health checker / 监控网段访问 `TTVG_METRICS_ADDR`。

## 8. 健康检查和 metrics

admin listener 必须和 relay listener 分开，避免业务字节流端口被 HTTP 请求
污染。

生产部署必须启用 admin listener，因为 `/readyz` 和 `/metrics` 是 LB 摘除、
监控告警和滚动发布的闭环依赖。`TTVG_METRICS_ADDR` 为空只允许用于本地开发、
临时 smoke test 或没有生产流量的手工排障环境，不满足生产验收要求。

admin listener 绑定策略：

- 单机部署或同机 agent 模式：默认 `127.0.0.1:15006`。
- 私网 LB / 远程监控直连模式：绑定父 VM 私网管理 IP，例如
  `<parent-vm-private-ip>:15006`，并用安全组只放行 LB health checker 和监控
  来源。
- 禁止把 admin listener 暴露到公网。

建议提供：

- `GET /healthz`
- `GET /readyz`
- `GET /metrics`

### 8.1 `/healthz`

只检查 gateway 进程是否存活、admin listener 是否可响应，不要求 Enclave
可达。

示例响应：

```json
{"status":"ok"}
```

### 8.2 `/readyz`

当配置了 `TTVG_VSOCK_METRICS_PORT` 时，`/readyz` 应主动 dial
`AF_VSOCK(<cid>, <metrics_port>)`。如果能读到 Enclave metrics frame，返回
`200`；否则返回 `503`。

metrics probe 的读取规则必须固定，避免不同实现对“ready”的理解不一致：

```text
frame header:
  type:   1 byte
  length: 4 bytes unsigned big-endian

expected:
  type == 0x20
  0 < length <= 1 MiB
  payload is UTF-8 JSON object
  JSON contains at least "n_workers", "accepted", "failed"
```

读取 header、payload 和解析 JSON 都必须受 `TTVG_CONNECT_TIMEOUT` 或独立 probe
timeout 约束。任何 dial 失败、header 不完整、type 不匹配、length 超限、payload
不是 JSON object、字段缺失都视为 not ready。

为避免私网 LB 高频健康检查造成 vsock metrics listener 压力，`/readyz` 必须
支持短 TTL 缓存：

- `TTVG_READY_CACHE_TTL` 默认 `1s`。
- TTL 内并发 `/readyz` 请求复用同一次 probe 结果。
- 同一时刻最多允许一个真实 vsock readiness probe 在运行，其它请求等待结果或
  使用未过期缓存。
- shutdown 开始后不得使用旧的 ready 缓存，必须立即返回 `503`。
- metrics 应记录真实 probe 次数、cache hit 次数、probe duration 和 probe
  error。

这能提前发现：

- Enclave CID 配错。
- Enclave 未启动。
- runtime 镜像启动失败，没有监听 `5005` / `5006`。
- Docker 或 host security policy 阻止 `AF_VSOCK`。

如果运维方显式关闭主动 readiness，`/readyz` 可以只检查本地配置和 listener
状态。但生产环境建议开启主动 readiness。

### 8.3 `/metrics`

建议使用 Prometheus text format。最小指标：

```text
ttvg_connections_active
ttvg_connections_total
ttvg_connections_rejected_total
ttvg_vsock_dial_total
ttvg_vsock_dial_errors_total
ttvg_copy_errors_total
ttvg_bytes_tcp_to_vsock_total
ttvg_bytes_vsock_to_tcp_total
ttvg_connection_duration_seconds
ttvg_readiness_probe_total
ttvg_readiness_probe_errors_total
ttvg_readiness_probe_duration_seconds
ttvg_readiness_cache_hits_total
```

metrics 禁止包含：

- request body
- response body
- authorization header
- proof trailer 原文
- nonce
- 上游 payload 数据

## 9. 日志

日志应结构化、低基数、避免敏感信息。

建议记录：

- TCP connection accepted。
- 连接因并发限制被拒绝。
- vsock dial 失败。
- 双向复制出错。
- 连接关闭，包含字节数、耗时和关闭原因。
- 进程开始退出和退出完成。

禁止记录：

- 请求体。
- 响应体。
- 完整 proof trailer。
- API key / authorization header。
- nonce，除非后续单独提供 debug build 并默认关闭。

推荐字段：

```text
conn_id
remote_addr
vsock_cid
vsock_port
duration_ms
bytes_tcp_to_vsock
bytes_vsock_to_tcp
close_reason
```

## 10. 安全模型

gateway 属于不可信父侧基础设施。只要它保持透明，它不会削弱
proof-of-observation 的完整性保证：

- gateway 不能伪造 proof，因为 proof signing key 在 Enclave 内。
- gateway 不能让任意 Enclave image 被用户信任，因为 PCR / evidence 检查在
  用户侧 verifier 中完成。
- gateway 可以丢弃、延迟、截断、拒绝流量；这是可用性风险，不是 proof
  完整性风险。

生产必需控制：

- 优先监听私网地址。
- 用安全组、防火墙、VPC 路由限制来源 IP。
- 远程模式下 gateway 不得直接公网暴露。即使业务上由同一家中转站管理，也应
  默认只允许 relay 子网、relay 安全组或固定私网 IP 访问。
- 跨 VPC、跨账号或跨组织暴露 gateway 时，TCP 侧 mTLS 应从“可选增强”升级为
  必需项。
- admin listener 默认只监听 `127.0.0.1`。
- 使用 systemd 或同等级进程管理。
- 设置保守的 `TTVG_MAX_CONNS`。
- 监控 readiness、dial error、active connection、copy error。
- 中转站仍必须把 proof 原样交给用户侧 verifier，不能因为 gateway 可达就
  跳过验证。

可选后续增强：

- TCP 侧 mTLS。
- PROXY protocol，用于保留原始 relay instance 身份。
- 按来源 IP 限制并发。
- 多租户独立 listener。

## 11. 失败行为

gateway 必须 fail closed。

| 失败场景 | 行为 |
|---|---|
| 配置无效 | 拒绝启动。 |
| TCP listen 失败 | 拒绝启动。 |
| Enclave vsock dial 超时 | 关闭 TCP 连接，增加 dial error metric。 |
| Enclave 未就绪 | `/readyz` 返回 `503`，业务连接 dial 失败。 |
| 达到最大连接数 | 拒绝新 TCP 连接。 |
| idle timeout | 关闭两侧连接。 |
| copy error | 关闭两侧连接。 |
| 进程退出 | 停止接收新连接，等待已有连接 drain 到超时。 |

中转站侧必须按请求阶段区分失败：

- TCP 连接尚未建立，或 gateway 尚未成功 dial Enclave 时，可以按普通上游
  不可用处理，例如返回 `503 tee_proof_unavailable`，或由中转站按自身幂等策略
  换另一个 gateway endpoint 重试。
- 一旦中转站已经开始写入 `REQ_HEAD` 或 `REQ_BODY`，连接中断就必须视为本次
  proof 请求失败。中转站不能在用户无感知的情况下把同一个请求重放到另一个
  Enclave 后再拼接响应，因为上游可能已经收到请求并产生部分响应。
- 已经向用户返回任何 `RESP_HEAD` / `RESP_CHUNK` 后，如果 gateway 连接中断，
  中转站必须终止本次用户响应，并明确标记 proof 失败或响应不完整，不能伪造
  `RESP_TRAILER`。

## 12. 优雅退出

收到 `SIGTERM` 或 `SIGINT` 后：

1. 停止接受新 TCP 连接。
2. readiness 置为失败。
3. 等待已有连接完成，最长 `TTVG_SHUTDOWN_TIMEOUT`。
4. 超时后关闭剩余连接。
5. 正常退出返回状态码 `0`。

这样可以支持负载均衡后的滚动发布。

shutdown 开始后的新连接处理规则：

- relay listener 必须立即停止 accept 新 TCP 连接。
- `/readyz` 必须立即返回 `503`，并绕过 readiness cache。
- 如果 accept loop 已经取到连接但 shutdown flag 已置位，必须立即关闭该连接，
  并增加 `ttvg_connections_rejected_total{reason="shutdown"}`。
- 不得在 shutdown 期间继续 dial 新的 Enclave vsock 连接。

生产滚动发布推荐顺序：

1. 先让 LB 摘除当前 gateway 节点，或把 readiness 置为失败。
2. 等待 `ttvg_connections_active` 降为 `0`。
3. 如果有长流式请求，最多等待 `TTVG_SHUTDOWN_TIMEOUT`。
4. 再重启 gateway 或重启本节点 Enclave。

默认 `TTVG_SHUTDOWN_TIMEOUT=300s` 是为了给常见 SSE/model stream 留出收尾
时间。对超长流式业务，应通过 LB drain timeout 和业务最大响应时间共同决定。

## 13. 运行形态

v1 推荐优先发布单个 Linux binary。

### 13.1 Host binary

父 VM 上优先使用 host binary：

```text
/usr/local/bin/tcp-to-vsock-gateway
/etc/tcp-to-vsock-gateway/env
systemd service
```

好处：

- 避免 Docker seccomp / capability 对 `AF_VSOCK` 的限制。
- 运行路径简单。
- 更容易和云厂商 Enclave CLI 输出的 CID 配合。

### 13.2 Container

如果必须容器化，需要确认容器运行时允许创建 `AF_VSOCK` socket。

常见要求：

- 部署在 Enclave 父 VM 的 Linux host 上。
- `--network host` 或等价网络模式。
- seccomp profile 放行 `socket(AF_VSOCK, ...)`。
- 初次验证可使用 `--privileged`，跑通后再收敛权限。

容器镜像不得包含云厂商密钥、上游 API key 或 verifier trust 私密材料。

## 14. systemd 部署草案

环境文件示例：

```env
TTVG_LISTEN_ADDR=0.0.0.0:15005
TTVG_VSOCK_CID=4
TTVG_VSOCK_PORT=5005
TTVG_METRICS_ADDR=127.0.0.1:15006
TTVG_VSOCK_METRICS_PORT=5006
TTVG_CONNECT_TIMEOUT=5s
TTVG_IDLE_TIMEOUT=300s
TTVG_READY_CACHE_TTL=1s
TTVG_SHUTDOWN_TIMEOUT=300s
TTVG_MAX_CONNS=512
TTVG_LOG_LEVEL=info
```

service 示例：

```ini
[Unit]
Description=TCP to vsock gateway for proof-of-observation
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/tcp-to-vsock-gateway/env
ExecStart=/usr/local/bin/tcp-to-vsock-gateway
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
```

systemd hardening 需要在各云父 VM 上实测。如果某个 sandbox 选项阻断
`AF_VSOCK`，只放宽对应项，并记录原因。

`TimeoutStopSec` 必须大于 `TTVG_SHUTDOWN_TIMEOUT`，否则 systemd 可能在
gateway 完成 drain 前发送强制终止信号。建议至少预留 30s 缓冲，例如
`TTVG_SHUTDOWN_TIMEOUT=300s` 时设置 `TimeoutStopSec=330s`。

## 15. 中转站接入方式

中转站接入时，只把 dialer 从 vsock 改成 TCP。

改造前：

```text
dial AF_VSOCK <enclave_cid>:5005
```

改造后：

```text
dial TCP <gateway_private_ip>:15005
```

除此之外，中转站必须保持原有 `TEE Relay Frame Protocol v1` 行为：

- 每个请求生成 fresh nonce。
- 发送 `REQ_HEAD`。
- 发送 `REQ_BODY`。
- 读取 `RESP_HEAD`。
- 流式转发所有 `RESP_CHUNK`。
- 读取 `RESP_TRAILER`。
- 把 proof 原样交给用户或 verifier。
- 保持请求体和响应体字节不被二次改写，保证 hash binding 可验证。

特别注意：不能把 gateway 或 Enclave 当成“hash-only signing service”。真实
上游请求必须仍由 Enclave 内 `/attest` 发起并观察完整响应。

连接失败和重试要求：

- dial TCP gateway 失败：中转站可以选择另一个 gateway endpoint 重试。
- 已经成功建立 gateway 连接但还没有写入任何 frame：可以安全重试。
- 已经写入任何 `REQ_HEAD` / `REQ_BODY` 字节：不得自动重放为同一次用户请求。
- 已经向用户透传任何响应字节：不得再切换 gateway，也不得拼接另一次请求的
  proof。
- 所有失败路径都必须让用户侧知道本次响应没有可验证 proof，或者直接返回
  明确错误。

## 16. 测试计划

### 16.1 单元测试

覆盖：

- 配置解析和校验。
- 非法 CID、端口、duration、并发数。
- 并发连接限制。
- copy loop 的 EOF 和 error。
- half-close 行为。
- vsock 不支持 `CloseWrite` 时的降级行为。
- metrics 计数。
- graceful shutdown 状态切换。

### 16.2 无真实 Enclave 的集成测试

通过 fake dialer / fake stream server 模拟 vsock 侧：

- echo bytes。
- 延迟响应。
- 提前关闭。
- 停止读取，测试背压。
- 发送大 payload。
- 并发高频 `/readyz` 请求，确认 TTL 内不会产生同等数量的真实 vsock probe。
- `SIGTERM` 后新连接被拒绝，已有连接可以完成；超过 shutdown timeout 的连接
  被关闭并记录 metric。

实现时应把 vsock dialer 抽象成接口，这样绝大多数测试不依赖真实
`AF_VSOCK`。

### 16.3 Linux AF_VSOCK smoke test

在支持 vsock 的 Linux 机器上：

1. 启动本地 vsock test listener。
2. 启动 gateway。
3. 通过 TCP 连接 gateway。
4. 确认双向字节未被修改。

### 16.4 真实 Enclave 验收测试

对 AWS Nitro、阿里云、华为 QingTian 分别执行：

1. 启动官方 `proof-of-observation` Enclave runtime。
2. 直连 `cid:5006` 确认 metrics 可读。
3. 启动 gateway。
4. 中转站配置 gateway TCP endpoint。
5. 发起一个非流式请求和一个流式请求。
6. 用对应 trust config 的 verifier 验证 proof。
7. 检查 gateway metrics 中连接数和字节数符合预期。

通过标准：

- 非流式验证通过。
- 流式验证通过。
- proof bytes 未被 gateway 修改。
- 错误 CID 会导致 readiness 失败和请求失败。
- 停止 Enclave 后 readiness 失败。
- LB 高频健康检查下真实 readiness probe 次数受 `TTVG_READY_CACHE_TTL` 限制。
- 滚动重启时 readiness 先失败，节点被 LB 摘除，已有长流式连接按 drain 策略
  结束。

## 17. 实现建议

v1 推荐用 Go 实现，原因：

- 容易发布单个 Linux binary。
- TCP、HTTP admin server、signal handling 成熟。
- metrics 接入简单。
- 代码量可以很小，便于审计。

特殊点是 AF_VSOCK 支持。实现时可以选用小型维护良好的 vsock package，或
封装最小 Linux syscall。vsock dialer 必须隐藏在接口后，方便测试替换。

build tag 策略：

- 真实 vsock dialer 只在 Linux 下编译，例如使用 `//go:build linux`。
- 非 Linux 平台提供 stub dialer，仅支持配置解析、`--check-config`、单元测试
  和 fake dialer 集成测试。
- 非 Linux stub dialer 被业务路径调用时必须返回明确错误，例如
  `vsock dialer is only available on linux`。
- CI 应在 macOS / Linux 都能跑不依赖真实 AF_VSOCK 的测试；真实 AF_VSOCK smoke
  test 只在支持 vsock 的 Linux runner 或父 VM 上运行。

建议后续代码结构：

```text
cmd/tcp-to-vsock-gateway/
internal/config/
internal/bridge/
internal/vsockdial/
internal/admin/
internal/metrics/
```

本仓库不应复制或 vendor `proof-of-observation` 的 verifier 逻辑。

## 18. 发布产物和生产验收

v1 发布至少包含：

- `linux/arm64` binary：用于 AWS Nitro Enclave 父 VM。
- `linux/amd64` binary：用于阿里云 Enclave 和华为 QingTian Enclave 父 VM。
- `SHA256SUMS`。
- 版本信息：`tcp-to-vsock-gateway --version` 输出 git commit、构建时间、
  Go 版本和目标平台。
- systemd unit 示例。
- env 文件示例。
- 最小安装说明。
- CHANGELOG。

v1 支持矩阵：

| Provider | Required gateway artifact |
|---|---|
| AWS Nitro Enclave | `tcp-to-vsock-gateway_linux_arm64` |
| 阿里云 Enclave | `tcp-to-vsock-gateway_linux_amd64` |
| 华为 QingTian Enclave | `tcp-to-vsock-gateway_linux_amd64` |

发布时 `SHA256SUMS` 必须同时覆盖以上两个目标平台产物。后续如需支持其它父 VM
架构，应先更新该矩阵、补齐 smoke test，再发布对应 binary。

建议二进制支持：

```bash
tcp-to-vsock-gateway --version
tcp-to-vsock-gateway --check-config
```

生产上线前验收：

- `--check-config` 通过。
- `/healthz` 返回 `200`。
- `/readyz` 在 Enclave 正常时返回 `200`，停止 Enclave 后返回 `503`。
- 生产环境 admin listener 已启用；如果由 LB 直接访问 `/readyz`，listener 已
  绑定到私网管理地址且安全组限制来源。
- 非流式 proof 请求通过 verifier。
- 流式 proof 请求通过 verifier。
- LB 能根据 `/readyz` 摘除故障节点。
- `SIGTERM` 后不再接新连接，已有连接按 drain 策略结束。
- metrics 能观察 active connection、dial error、copy error 和字节数。
- 日志中没有请求体、响应体、Authorization、nonce 或 proof trailer 原文。

## 19. 待确认问题

开发前建议确认：

- 需要支持的父 VM Linux 发行版范围。
- 首个版本是否必须支持 Docker，还是 host binary 优先。
- v1 是否需要 mTLS，还是安全组 / IP allowlist 足够。
- 单 gateway 预期最大并发请求数。
- 是否需要一个进程支持多个 Enclave CID / listener。

推荐 v1 选择：一个 gateway 进程对应一个 Enclave endpoint。这样故障域、
配置和 metrics 都更简单。

## 20. 总结

如果 gateway 坚持只做：

```text
TCP byte stream <-> AF_VSOCK byte stream
```

那么 AWS Nitro Enclave、阿里云 Enclave、华为 QingTian Enclave 可以共用
同一个 gateway 版本。

云厂商差异应该保留在：

- Enclave runtime image。
- Enclave lifecycle 脚本。
- Enclave egress proxy 部署。
- verifier trust config。

这些差异不应该进入 gateway。
