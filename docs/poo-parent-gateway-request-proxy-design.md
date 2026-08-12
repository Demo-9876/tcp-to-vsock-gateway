# PoO Parent Gateway 按请求指定代理出网技术方案

日期：2026-07-27

状态：生产设计草案

本文描述 `tcp-to-vsock-gateway` 如何演进为 PoO Parent Gateway，使不同中转站在接入 PoO 后，仍然像原有链路一样按请求或按账号指定代理地址。目标链路是：

```text
原直连链路：
  中转站 <-> 上游模型

接入 PoO 后：
  中转站 <-> PoO(TEE) <-> 上游模型

原代理链路：
  中转站 <-> 代理 <-> 上游模型

接入 PoO 后：
  中转站 <-> PoO(TEE) <-> 代理 <-> 上游模型
```

本方案不要求 proof 正面证明“使用了哪个代理出口”。代理只作为不可信 egress 层的一部分存在；用户侧 verifier 仍然验证 TEE 内请求的上游 host/path、请求体 hash、响应体 hash、nonce、TEE evidence 和 statement signature。

## 1. 当前结论

当前 `proof-of-observation` main 已具备以下核心能力：

- Enclave 内接收 Relay Adapter 发来的 `REQ_HEAD` / `REQ_BODY`。
- `REQ_HEAD` 包含 `nonce`、`egress_port`、`upstream.host`、`upstream.path`、`upstream.method` 等字段。
- Enclave 根据 `egress_port` 连接父 VM egress 字节通道。
- Enclave 在该字节通道上对 `upstream.host` 建立 TLS，校验证书，发送真实上游 HTTP 请求。
- Enclave 流式返回响应，并在末尾输出 `RESP_TRAILER`，其中包含 `tee.proof`；失败时可能返回 Enclave 协议内的 `ERR` frame。
- `tee.proof` 签名覆盖 `upstream_host`、`upstream_path`、HTTP status、请求 hash、响应 hash 等事实。

当前不足：

- `REQ_HEAD` 不包含 `proxyURL`。
- Enclave 不执行 HTTP CONNECT 或 SOCKS5/SOCKS5H 代理握手。
- `egress_port` 当前表达的是“一个已经可用的 raw TCP tunnel”，不是“一个代理配置”。
- 父 VM 侧缺少统一的、可按请求接收 `proxyURL` 并维护 cached egress route / per-request lease 的 route manager。

因此，如果要求“像 sub2api 的 `Do(req, proxyURL, accountID, concurrency)` 一样，在发起请求时指定代理地址”，需要在父 VM 侧 PoO Parent Gateway 补一层按请求动态路由能力；但 `proof-of-observation` 的 proof wire、proof statement 和 verifier 语义不需要为了代理而扩展。

## 2. 目标和非目标

### 2.1 目标

- 支持每次上游请求携带一个可选 `proxyURL`。
- 支持无代理直连和有代理出网共存。
- 支持账号级代理场景：中转站选中账号后，把该账号绑定的 `proxyURL` 传给 PoO adapter。
- 支持常见代理协议：
  - `http://host:port`
  - `https://host:port`
  - `socks5://host:port`
  - `socks5h://host:port`
  - 带用户名密码的代理 URL
- 保持 Enclave 内 TLS 终结：上游 TLS SNI、证书校验、HTTP 请求和响应 hash 仍在 TEE 内完成。
- 保持 proof statement 不变：proof 证明访问了哪个上游 endpoint、请求/响应 bytes 是否匹配，不证明代理身份。
- 为不同中转站提供统一接入模型，而不是给每个项目实现一套私有代理协议。

### 2.2 非目标

- 不把 `proxyURL` 加入 `tee.proof`。
- 不要求 verifier 校验代理出口。
- 不在第一阶段让 Enclave 直接理解 HTTP/SOCKS 代理协议。
- 不要求 Relay Adapter 或父 VM 成为可信组件。
- 不要求隐藏代理配置不被父 VM 知道。父 VM 本来就是代理出网执行层，会持有或接收代理地址。

## 3. 总体架构

推荐把父 VM 侧能力收敛为一个部署单元：**PoO Parent Gateway**。

它可以由 `Demo-9876/tcp-to-vsock-gateway` 仓库演进而来。原因是这个仓库已经承担“中转站到 Enclave control port 的父 VM 网关”职责，继续在同一仓库内增加 egress route manager，可以让接入方只部署一个父 VM gateway binary / container / systemd service。

注意：这里说的是**同仓库、同部署单元**，不是把职责混成一团。PoO Parent Gateway 内部仍然应拆成三个模块：

- Proof Relay API：推荐默认入口。接收中转站 adapter 构造好的原 PoO frame stream 和可选 `proxyURL` metadata，内部完成 route 分配、`REQ_HEAD.egress_port` 填充/替换、Enclave 调用和 frame 透传。
- Ingress Bridge：兼容/低层入口。复用现有 `tcp-to-vsock-gateway` 能力，负责 `TCP -> AF_VSOCK <enclave_cid>:5005`，透明转发已由调用方构造好的 PoO frame bytes。
- Egress Router：内部能力。负责按请求创建 route，并在 Enclave 连接 `egress_port` 后，根据 route 建立直连或代理 tunnel。

目标链路：

```text
client
  -> OSS relay / 中转站
  -> PoO Relay Adapter
  -> PoO Parent Gateway Proof Relay API(frame stream, proxyURL metadata)
  -> PoO Parent Gateway internal route cache lookup + lane_port allocation
  -> AF_VSOCK <enclave_cid>:5005 with REQ_HEAD(egress_port, upstream_host, ...)
  -> Enclave /attest
  -> PoO Parent Gateway internal single-use lane listener on lane_port
  -> optional HTTP CONNECT / SOCKS5 proxy
  -> upstream_host:443
```

Enclave 看到的仍然是：

```text
connect_egress(egress_port)
  -> raw TCP tunnel
  -> TLS handshake with upstream_host
```

是否经过代理由父 VM egress gateway 负责，对 Enclave 透明。

部署视角可以简化为：

```text
中转站
  -> PoO Parent Gateway on Enclave parent VM
  -> TEE / Enclave runtime
  -> PoO Parent Gateway egress module
  -> proxy/direct
  -> upstream
```

这样接入方新增的 PoO 部署节点仍然主要是一组 `PoO Parent Gateway + Enclave runtime` 节点，而不是多个彼此独立的网关节点。

默认产品形态不要求中转站 adapter 在发送 `REQ_HEAD` 前显式调用 lane registration API。中转站 adapter 可以只调用一次 PoO Parent Gateway，把原 PoO frame stream 和可选 `proxyURL` metadata 交给 Gateway；`egress_port` 和 route 生命周期变成 Gateway 内部细节。

### 3.1 兼容两种中转站部署拓扑

PoO Parent Gateway 必须兼容两种部署方式：

模式 A：中转站部署在独立机器上，通过网络连接 Enclave 父 VM。

```text
client
  -> 中转站机器
  -> PoO Relay Adapter
  -> VPC / private network
  -> PoO Parent Gateway on Enclave parent VM
  -> AF_VSOCK / direct-vsock
  -> Enclave
  -> Parent Gateway egress route
  -> proxy/direct
  -> upstream
```

这种模式下，PoO Parent Gateway 的 Proof Relay API 是远程接入面，可以监听父 VM 的内网地址或受保护的服务地址。它替代原来“远程中转站直接连 tcp-to-vsock-gateway L4 listener”的默认入口；原 L4 listener 仍可作为低层兼容入口保留。

模式 B：中转站直接部署在 Enclave 父 VM 上。

```text
client
  -> 父 VM 本机中转站
  -> PoO Relay Adapter
  -> 127.0.0.1 / Unix socket
  -> PoO Parent Gateway on same parent VM
  -> AF_VSOCK / direct-vsock
  -> Enclave
  -> Parent Gateway egress route
  -> proxy/direct
  -> upstream
```

这种模式下，Proof Relay API 可以只绑定 `127.0.0.1` 或 Unix socket，减少网络暴露面。中转站和 Parent Gateway 仍是两个逻辑组件，但可以同机、同 Compose、同 systemd target 部署。

两种模式的共同约束：

- PoO Parent Gateway 必须运行在能连接 Enclave vsock/direct-vsock 的父 VM 或等价宿主环境上。
- route 创建、Enclave control connection、Enclave 连接 `egress_port`、父 VM 侧 proxy/direct 出网必须落在同一个 Parent Gateway 实例或同一个 PoO 节点内。
- 中转站可以远程部署，也可以本机部署；但不应直接管理 Enclave egress port 生命周期。
- Internal egress listeners 只服务 Enclave 到父 VM 的本地出网链路，不应暴露给中转站机器或公网。
- QingTian / Nitro / Aliyun direct-vsock 生产路径下，single-use lane listener 必须绑定在 Enclave 可达的 vsock/direct-vsock 监听面；如为了本地兼容临时使用 TCP listener，也只能绑定 loopback 并由本机 ACL 隔离，不能绑定 `0.0.0.0:<lane_port>` 或父 VM 内网地址。
- 远程模式下必须给 Proof Relay API 增加认证、访问控制和传输加密；本机模式也建议使用 least privilege 的 Unix socket 权限或 loopback ACL。

## 4. 组件职责

### 4.1 中转站

中转站继续负责：

- 鉴权、计费、账号选择、模型路由。
- 按原有逻辑决定是否使用代理。
- 从账号、渠道或请求上下文中解析 `proxyURL`。
- 调用 PoO adapter，而不是直接调用上游模型。

以 sub2api 为例，原有调用形态是：

```text
HTTPUpstream.Do(req, proxyURL, accountID, accountConcurrency)
```

接入 PoO 后，理想形态是：

```text
TEEUpstream.Do(req, proxyURL, accountID, accountConcurrency)
```

或者在原 `HTTPUpstream` 实现中按配置切到 TEE 模式，但不建议把 Enclave endpoint 伪装成普通 `proxyURL`。

### 4.2 PoO Relay Adapter

Relay Adapter 负责把中转站的普通 HTTP request 转成原 PoO Relay Frame Protocol，并交给 PoO Parent Gateway 透传到 Enclave。

新增职责：

- 接收中转站传入的 `proxyURL`。
- 根据 request URL 得到 `upstream_host` 和 `upstream_path`。
- 生成或透传 `nonce`、proof required/optional 等 PoO 调用参数。
- 按既有 PoO Relay Frame Protocol 构造 `REQ_HEAD` / `REQ_BODY`，调用 PoO Parent Gateway 的统一 frame relay API，并通过 metadata 传入可选 `proxyURL`。
- 像原来直连 Enclave 一样读取 Gateway 透传回来的 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR`，再转回客户端可消费的响应格式。

默认模式下，Relay Adapter 不需要：

- 显式调用 lane registration API。
- 获取或管理 `egress_port`。
- 自己把 `egress_port` 写入 `REQ_HEAD`。
- 维护 route TTL、端口池或节点亲和性。

低层 lane registration 不进入 v1 生产实现范围；如果后续某个调用方确实需要自己构造并发送 PoO frame，再作为单独兼容能力设计。

Relay Adapter 不负责：

- 生成 proof。
- 修改 proof。
- 在 Enclave 外终结上游 TLS。
- 对 `tee.proof` 里的 platform evidence 做 profile-specific 解析。

### 4.3 PoO Parent Gateway / tcp-to-vsock-gateway

`Demo-9876/tcp-to-vsock-gateway` 当前是连接中转站和 Enclave control port 的 L4 透明网关：

```text
Relay Adapter / 中转站
  -> TCP tcp-to-vsock-gateway
  -> AF_VSOCK <enclave_cid>:5005
  -> Enclave /attest
```

现有 ingress bridge 职责应保持不变：

- 每个 TCP 连接映射一个 Enclave vsock 连接。
- 双向复制字节。
- 不解析 `TEE Relay Frame Protocol v1`。
- 不读取、生成、验证或重写 `tee.proof`。

按第一档合并父 VM 组件方案，建议在这个仓库中新增 egress router 模块，而不是另建一个完全独立的 PoO Egress Gateway 仓库。这样有几个好处：

- 部署单元更少：一个 binary / container / systemd service 即可承载父 VM ingress 和 egress。
- 节点亲和性天然更清楚：route 创建、Enclave control connection、egress_port 数据面都在同一个 PoO Parent Gateway 节点上。
- 复用现有健康检查、metrics、并发限制、graceful shutdown 和发布流程。
- 中转站只需要配置一个 PoO Parent Gateway endpoint。

但代码结构上必须保持模块边界：

```text
cmd/tcp-to-vsock-gateway
  -> internal/bridge        # 现有 ingress: TCP -> vsock
  -> internal/proofrelay    # 新增统一调用入口: PoO frame stream + proxy metadata -> Enclave
  -> internal/egressroute   # 新增 route lifecycle: allocation / TTL / isolation
  -> internal/egressproxy   # 新增 data plane: direct / HTTP CONNECT / SOCKS5 / SOCKS5H
  -> internal/admin         # 扩展 healthz / readyz / metrics
```

也可以在产品命名上把运行时叫做 `poo-parent-gateway`，但第一阶段代码仓库建议放在 `tcp-to-vsock-gateway` 中演进，避免多仓库、多进程、多部署模板同时出现。

运行时它包含三类监听面：

```text
Proof Relay API listener:
  中转站 adapter(远程或本机) -> PoO frame stream + proxyURL metadata -> internal route allocation -> AF_VSOCK <enclave_cid>:5005

Legacy ingress listener:
  已构造 PoO frame 的调用方 -> TCP listen addr -> AF_VSOCK <enclave_cid>:5005

Internal egress listeners:
  Enclave -> egress_port -> proxy/direct -> upstream
```

远程中转站和本机中转站的区别，只体现在 Proof Relay API listener 的绑定地址、认证方式和网络 ACL 上：

- 远程中转站：监听 VPC/private addr，v1 生产固定启用 mTLS，并限制来源安全组。
- 本机中转站：监听 `127.0.0.1` 或 Unix socket，建议用本机 socket 权限或 loopback ACL 隔离。

两种部署方式下，Parent Gateway 到 Enclave 的 AF_VSOCK/direct-vsock 连接和内部 egress listeners 都保持父 VM 本地能力，不应要求中转站机器直接访问这些本地端口。

如果中转站和 Enclave 之间已经通过 `tcp-to-vsock-gateway` 接入，推荐把接入方式升级为：

- 中转站 adapter 调用 PoO Parent Gateway 的 Proof Relay API。
- 请求 body 携带 adapter 构造好的原 PoO frame stream，transport metadata 携带可选 `proxyURL` 和审计/关联 scope。
- Parent Gateway 内部查找或创建 cached route，并为本次 lease 分配 single-use `lane_port`。
- Parent Gateway 先完整读取并校验 `REQ_HEAD + 单个 REQ_BODY + EOF`，暂存 `REQ_BODY`，只填充/替换 `REQ_HEAD.egress_port`，再把已校验的 `REQ_HEAD` / `REQ_BODY` 转发给 Enclave。
- Enclave 连接该 `egress_port`，Gateway 再按 route 直连或走指定代理。

Legacy ingress bridge 仍然不需要解析 `proxyURL`。v1 统一调用模式下，`proxyURL` 只进入 Proof Relay API，`egress_port` 只存在于 Gateway 内部。

统一调用模式也降低了多节点部署的节点亲和性风险：route 创建、Enclave control connection、egress listener 都由同一个 Parent Gateway 实例串起来，不会出现 route/lane 分配和 control connection 被负载均衡到不同节点的错配。

推荐把一个 PoO 节点定义为：

```text
PoO Parent Gateway
  - ingress bridge
  - egress route manager
  - egress proxy/tunnel data plane
  + Enclave runtime
  + local direct-vsock egress support
```

中转站侧应按 PoO 节点选择 endpoint，而不是分别随机选择 ingress gateway 和 egress gateway。

### 4.4 Egress Router 模块

Egress Router 是 PoO Parent Gateway 内的新增模块，运行在父 VM 或等价不可信基础设施上。

控制面职责：

- 接收 Proof Relay API 内部传入的 route 创建请求。
- 校验 `target_host`、`target_port`、`proxyURL` 的格式。
- 复用 route cache entry，并为每个 lease 分配一个 single-use `lane_port`。
- 维护 route 生命周期、TTL、并发计数和清理。
- 向 Proof Relay API 返回内部 `lane_port`，由 Gateway 写入 `REQ_HEAD.egress_port`。

数据面职责：

- 监听一个或多个 `egress_port`。
- 当 Enclave 连接某个 `egress_port` 时，找到对应 route。
- 如果 route 没有 `proxyURL`，直连 `target_host:target_port`。
- 如果 route 有 HTTP/HTTPS proxy，先向 proxy 建立连接，再发送 `CONNECT target_host:target_port`。
- 如果 route 有 SOCKS5/SOCKS5H proxy，执行 SOCKS5 connect 握手。
- 代理握手成功后，开始双向拷贝字节。
- 对 Enclave 暴露的是已经连到 `target_host:target_port` 的 raw TCP tunnel。

PoO Parent Gateway 不可信。它可以拒绝服务、断开连接、连错目标或篡改字节流；但只要 Enclave 仍然在 TEE 内对 `upstream_host` 建 TLS 并校验证书，错误目标和篡改都会导致 TLS 或 hash/proof 验证失败，不能生成可通过 verifier 的假 proof。

### 4.5 Enclave Relay

Enclave Relay 继续保持当前职责：

- 接收 `REQ_HEAD`。
- 连接 `egress_port`。
- 对 `upstream.host` 建 TLS。
- 校验证书和 SNI。
- 发送 HTTP 请求。
- 哈希请求体和响应体。
- 生成 `tee.proof`。

第一阶段不建议让 Enclave Relay 直接支持 `proxyURL`。这样可以避免：

- 把代理协议复杂度放进 TEE 镜像。
- 让 EIF/PCR 因代理协议支持变大、变复杂。
- 在 Enclave 内处理大量代理认证和连接池细节。
- 改动 proof statement 和 verifier。

## 5. Proof Relay API v1 与内部 route cache 生命周期

### 5.1 推荐默认模型：一次调用 Parent Gateway

Relay Adapter 不直接操作 `egress_port`。它向 PoO Parent Gateway 发起一次 proof relay 调用，传入原 PoO frame stream 和可选 `proxyURL` metadata：

第一阶段固定采用 **PoO frame relay + 可选 HTTP/Unix socket transport**。为了让 Gateway 尽量保持透明，中转站 adapter 与 Enclave 之间的响应格式不改变：Enclave 原来如何返回 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR`，Gateway 就如何把这些 frame bytes 透传回 adapter。

- 远程中转站：`POST /v1/proof/relay` over HTTPS + mTLS。HTTP 只是承载原 PoO frame stream 的外层 transport，v1 生产默认只固定 mTLS wire spec；HMAC/JWT/service token 留到后续版本扩展。
- 父 VM 本机中转站：同一个 frame relay handler 可以监听 `127.0.0.1`，也可以挂到 Unix socket。
- 请求 body：v1 body 是原 PoO Relay Frame Protocol bytes，`Content-Type: application/vnd.poo.frames`。adapter 仍按既有方式发送 `REQ_HEAD` / `REQ_BODY`，不把普通上游 HTTP request 改造成 Gateway 私有 JSON envelope。
- 代理参数：`proxy_url` 作为 frame relay transport metadata 传入，例如 HTTP header `X-PoO-Proxy-URL` 或本机调用上下文；Gateway 只解析该 URL 并在父 VM egress 数据面使用，不把它写入 PoO frame 或 proof statement。
- 响应 body：Gateway 返回 Enclave 原始 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR` frame stream，`Content-Type: application/vnd.poo.frames`。Gateway 不生成新的非流式 JSON envelope，不追加 SSE `tee.proof` event，不把 Enclave response 转成 SDK/http.Response。
- Gateway 自身错误边界：只有在请求尚未转发给 Enclave 前，Gateway 才可以返回 `application/problem+json`。一旦已经把 `REQ_HEAD` / `REQ_BODY` 转发给 Enclave，后续必须保持 Enclave frame 语义：透传 Enclave 原始 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR`，或关闭连接并记录指标，不能再插入 Gateway 私有业务响应。
- HTTP response 提交边界：Gateway 只有在完成自身前置检查、建立 Enclave control connection，并准备写出第一帧 Enclave response frame 时，才提交 `200 application/vnd.poo.frames`；在这之前不能提前 flush HTTP 200 或 response headers。
- Gateway v1 不解释 proof required / optional / disabled 业务语义，不从客户端 header 或上游 HTTP header 推导 proof mode。强 proof、兼容 fail open、最终是否把未验证响应返回给用户，都由中转站 adapter 或 verifier 按既有 PoO 接入逻辑处理。

Proof Relay API listener v1 endpoint：

```text
POST /v1/proof/relay
```

Admin listener endpoint：

```text
GET  /healthz
GET  /readyz
GET  /metrics
```

- `POST /v1/proof/relay` 只挂在 Proof Relay API listener，不挂在 admin listener。
- `/healthz`、`/readyz`、`/metrics` 只挂在 admin listener；Proof Relay API listener 不暴露 metrics。
- `POO_PARENT_ADMIN_LISTEN` 生产默认绑定 loopback。远程 LB 如果需要访问 `/readyz`，应通过受控内网或 service mesh 访问 admin listener，且 `/readyz` 只返回最小健康状态，不输出 route、proxy、tenant、account、proof 或配置详情。
- `/metrics` 只用于内部监控抓取，不应暴露到公网或中转站业务入口。

错误码映射：

| 场景 | HTTP status | `code` | `retryable` |
|---|---:|---|---|
| 请求 frame 非法或字段缺失 | `400` | `bad_request` | `false` |
| `target_host:port` 不在 allowlist | `403` | `target_not_allowed` | `false` |
| `proxy_url` scheme 或格式非法 | `400` | `invalid_proxy_url` | `false` |
| relay metadata、frame 或 request bytes 超限 | `413` | `request_too_large` | `false` |
| 认证失败 | `401` / `403` | `unauthorized` / `forbidden` | `false` |
| 并发超过 client policy 或 gateway policy | `429` | `rate_limited` | `true` |
| route / lane / 端口池耗尽 | `503` | `egress_capacity_exhausted` | `true` |
| Enclave control port 不可达 | `503` | `enclave_unavailable` | `true` |
| preflight metadata/header/request read timeout | `408` / `504` | `relay_timeout` | `true` |

上表只描述 Gateway 在把请求转发给 Enclave 之前可以直接返回的自身错误。Enclave 未连接 `egress_port` 超时、proxy/direct 连接超时、临时网络失败、proxy 认证失败、CONNECT 被拒绝或 SOCKS auth 失败都发生在 Enclave egress data plane 阶段；此时 Gateway 不再返回自身 `application/problem+json`，只能让 Enclave 产生并返回原始 `ERR` frame，或在无法得到 Enclave `ERR` 时关闭连接并记录脱敏指标。后续缺少 `RESP_TRAILER`、收到 Enclave `ERR` 或连接中断，都由 adapter 按原 PoO 接入逻辑判断是否把未验证响应交给最终用户。

frame relay metadata：

```text
Content-Type: application/vnd.poo.frames
X-PoO-Proxy-URL: socks5h://user:pass@proxy.example.com:1080
X-PoO-Tenant-ID: optional-tenant-id
X-PoO-Account-ID: optional-account-id
X-PoO-Request-ID: optional-request-id
```

Gateway 只允许把这些 metadata 用作父 VM 侧路由 scope、审计关联和 proxy egress 输入。原始上游 HTTP headers、body、响应和 proof 仍在 PoO frame protocol 内由 Enclave/adapter 处理，Gateway 不新增业务层响应格式。

中转站原有的 `accountConcurrency` 不通过 Proof Relay API metadata 下发给 Gateway。账号级并发仍由中转站自身执行；Gateway 只做自身资源保护，按 authenticated client、route key、全局 active lease、端口池和 client policy 上限控制接入。

`X-PoO-Proxy-URL` v1 wire contract：

- header 缺失或值为空字符串表示 direct route。
- 非空值必须是 RFC 3986 absolute URI 字符串，只允许 `http`、`https`、`socks5`、`socks5h` scheme。
- v1 要求非空 proxy URL 必须显式包含 `host:port`，不做 scheme 默认端口补齐；缺失端口必须返回 `invalid_proxy_url`。
- proxy username / password 中的特殊字符必须由 adapter 做 percent-encoding；Gateway parse 时只 percent-decode 一次，不能反复 decode。
- header 值禁止 CR/LF、其它控制字符和前后空白；超出实现固定上限，例如 `8192` bytes，必须返回 `invalid_proxy_url`。
- URI 的 path、query、fragment 必须为空；需要业务参数时由中转站自己映射成 proxy host / port / credential，不能透传给 Gateway。
- Gateway normalize 后再计算 route key：scheme 小写、host 小写、保留显式端口、userinfo decode 后进入 secret hash；normalized redacted proxy key 不包含明文用户名密码。

`X-PoO-Proxy-URL` 或等价 transport metadata 可能包含代理用户名密码，必须按 secret 处理。远程中转站模式下，LB、service mesh、反向代理、HTTP access log、APM 和抓包排障工具都不得记录该 header 或其等价字段；如果接入环境无法保证 header 脱敏，应改用不会被默认 access log 采集的本机调用上下文、加密 side channel 或明确关闭该字段日志后再上线。

Gateway 自身错误 schema：

```json
{
  "code": "egress_capacity_exhausted",
  "message": "human-readable safe message",
  "request_id": "req_...",
  "retryable": true
}
```

Parent Gateway 必须从 frame relay metadata 和首个 `REQ_HEAD` 拿到：

- `nonce`，仅用于日志关联和连接生命周期排查，不用于 proof 判断。
- 上游 method、host、path 等 `REQ_HEAD` 字段。
- 本次请求可选的 `proxyURL`。
- 中转站侧可选 opaque `tenant_id` / `account_id`，只用于 route key scope、审计和 metrics hash。
- 中转站侧可选 `request_id`，只用于日志关联和错误响应回填，不进入 route key。

v1 frame stream 约束：

- 请求 body 的第一个 PoO frame 必须是且只能是一个 `REQ_HEAD`。
- 当前 PoO Relay Frame Protocol v1 的请求侧 frame type 只允许 `REQ_HEAD` (`0x01`) 和 `REQ_BODY` (`0x02`)。
- 合法请求序列必须是 `REQ_HEAD` 后跟恰好一个 `REQ_BODY`；空请求体使用 zero-length `REQ_BODY` 表示。
- Gateway 必须先读到并校验完整 v1 request frame stream，也就是 `REQ_HEAD`、恰好一个 `REQ_BODY`、随后 EOF 且没有尾随字节；任何尾随字节或额外 frame 都是不合法请求。
- 第二个 `REQ_HEAD`、第二个 `REQ_BODY`、任何 `RESP_*` frame、未知 frame type、或任何会改变 upstream / egress 语义的 frame 都必须在转发前返回 `bad_request`。
- Gateway 只有在完整请求 frame stream 校验通过后，才能分配 lane、连接 Enclave control port 并转发请求；提交 HTTP 200 / Enclave response stream 前发现额外 frame 或尾随字节时返回 `400 bad_request`。
- Gateway 只允许修改首个 `REQ_HEAD.egress_port`；`REQ_HEAD.upstream.*`、headers、nonce、body frame bytes 和 frame 顺序必须保持不变。
- 如果未来协议扩展为多请求 frame 或流式上传，违规 frame 在 Gateway 已向 Enclave 或 adapter 提交流之后才出现时，Gateway 只能关闭连接并记录 `invalid_frame_sequence_after_commit`，不能插入私有错误 frame；v1 单 `REQ_BODY` 路径必须避免这种提交后才发现尾随输入的情况。

relay stream 大小和缓冲限制：

- v1 必须分别限制 metadata header 总大小、`REQ_HEAD` payload 大小、`REQ_BODY` payload 大小、单次 relay stream 累计 request bytes，以及提交 Enclave 前的 `REQ_BODY` 暂存内存 bytes。
- v1 请求 frame stream 必须在调用 Enclave 前完成大小校验；metadata、`REQ_HEAD`、`REQ_BODY`、累计 request bytes 或 spool 超限时返回 `413 request_too_large`，不得创建 lane 或连接 Enclave control port。
- v1 为了在调用 Enclave 前确认 EOF，需要暂存单个 `REQ_BODY`；实现应优先使用受 `POO_RELAY_MAX_BUFFERED_BYTES` 限制的内存缓冲，超过后使用匿名临时文件或等价 spool，并受 `POO_RELAY_MAX_REQUEST_BYTES` 和 `POO_RELAY_MAX_SPOOL_BYTES` 约束。这个暂存只服务请求预检和随后向 Enclave 写回同一个 `REQ_BODY` frame，不允许变成响应 buffer、业务重组层或长期持久化队列；超过内存阈值、request 上限或 spool 上限时必须在调用 Enclave 前拒绝本次请求。
- spool 文件必须只用于 Enclave 调用前的请求暂存，权限等价 `0600`，优先使用 unlink-on-open；如果平台不支持 unlink-on-open，必须在请求结束和进程启动时执行兜底清理。创建、写入、容量检查或预提交 unlink 失败时，必须在调用 Enclave 前 fail closed；请求结束后的 cleanup 失败只记录告警和脱敏指标，不能改写已经开始的 Enclave frame 响应。
- 对应配置项是 `POO_RELAY_MAX_METADATA_BYTES`、`POO_RELAY_MAX_REQ_HEAD_BYTES`、`POO_RELAY_MAX_FRAME_BYTES`、`POO_RELAY_MAX_REQUEST_BYTES`、`POO_RELAY_MAX_BUFFERED_BYTES`、`POO_RELAY_SPOOL_DIR` 和 `POO_RELAY_MAX_SPOOL_BYTES`；`--check-config` 必须拒绝无限制或为 0 的生产配置，并拒绝 `POO_RELAY_MAX_REQ_HEAD_BYTES` 大于 `POO_RELAY_MAX_FRAME_BYTES` 或大于 Enclave 对齐上限的配置。

Parent Gateway 内部执行：

```text
1. read and validate complete v1 request frame stream before touching Enclave:
   REQ_HEAD + single REQ_BODY + EOF/no trailing bytes

2. parse REQ_HEAD:
   target_host = api.openai.com
   target_port = 443
   upstream_path = /v1/responses

3. get or create cached route:
   proxy_url = socks5h://user:pass@proxy.example.com:1080
   route_key = client_scope + tenant_id + account_id + target_host + target_port + normalized_redacted_proxy_key + proxy_secret_hash

4. acquire a per-request route lease:
   request_id
   nonce (log/correlation only; do not validate proof)
   lease_ttl = 30000ms

5. allocate single-use lane port:
   lane_port = 18445
   ensure lane listener is already bound and accepting.

6. rewrite or fill REQ_HEAD.egress_port:
   egress_port = lane_port
   keep nonce / upstream.host / upstream.path / upstream.method / upstream.headers unchanged

7. open vsock connection to Enclave control port.
8. forward rewritten REQ_HEAD and the already validated single REQ_BODY frame.
9. wait for Enclave to connect egress_port and bind this connection to the lease.
10. egress proxy connects direct/proxy to target_host:target_port.
11. stream RESP_HEAD / RESP_CHUNK / RESP_TRAILER / ERR back to adapter.
12. release the lease when request finishes or fails.
13. close lane port and keep cached route entry until idle TTL expires or route cache is evicted.
```

这里的 `egress_port` 是 Gateway 内部细节，不暴露给中转站 adapter。

请求取消和 backpressure：

- Adapter 断开 `/v1/proof/relay` HTTP 连接时，Parent Gateway 必须关闭 Enclave control connection，并释放本次 lease。
- Proof Relay API 必须对 metadata/header 读取、首个 `REQ_HEAD`、单个 `REQ_BODY` 和 EOF confirmation 设置 read / idle timeout；超时发生在请求转发给 Enclave 前时不得创建 lane 或连接 Enclave，直接返回 `relay_timeout`；请求已经转发给 Enclave 后的响应透传 idle timeout 只能关闭连接并记录指标，不能返回 Gateway 自身错误。
- Enclave control connection 失败时，Parent Gateway 必须关闭 upstream/proxy tunnel；如果失败发生在请求转发给 Enclave 前，可以返回 Gateway 自身错误；如果失败发生在请求已经转发给 Enclave 后，只能透传 Enclave 原始 `ERR`、关闭连接并记录指标，或让 adapter/verifier 按缺少 `RESP_TRAILER` 的既有逻辑处理。
- 响应流写入 adapter 受阻时，应通过 TCP backpressure 传导到 Enclave；超过 idle timeout 后 fail closed。
- 如果响应流在 `RESP_TRAILER` 前异常结束，Gateway 不补发 `tee.error` 或其它私有 frame；它关闭连接并记录 stream ended before trailer，adapter/verifier 按既有 PoO 逻辑把本次响应视为未验证或失败。

### 5.2 非 v1 范围：显式 lane registration

v1 生产实现不暴露低层 lane registration API，也不提供低层 lane 相关外部配置。普通中转站只接入 Proof Relay API，由 Gateway 在一次调用内完成 route lookup、single-use `lane_port` 分配、`REQ_HEAD.egress_port` 填充和 Enclave 调用。

如果后续确实需要支持“调用方自行构造 PoO frame 并手动写入 `egress_port`”的高级模式，应作为独立版本重新设计认证、节点粘性、readyz、测试矩阵和运维 runbook，不能混入 v1 主路径。

### 5.3 Route cache 生命周期

本期实现 **长生命周期 route cache**。cache 复用的是完整 route key、解析后的 proxy config、容量计数、统计对象和最近使用状态；每次模型请求仍然创建独立 Enclave egress connection 和独立 upstream/proxy tunnel。

由于第一阶段 Enclave 连接 `egress_port` 时无法携带 connection token，v1 生产实现不在同一个 `egress_port` 上承载多个并发 lease。route cache 是长生命周期的，但具体给 Enclave 连接的 data-plane lane 是单请求生命周期：

```text
route cache entry(route_key, normalized proxy config)
  -> acquire lease
  -> allocate single-use lane_port
  -> write lane_port into REQ_HEAD.egress_port
  -> Enclave connects lane_port
  -> proxy/direct tunnel
  -> release lease and close lane_port
  -> keep route cache entry until idle TTL / eviction
```

这样既满足长生命周期 route cache，又避免在没有 connection token 的情况下用 FIFO 猜测并发连接归属。后续如果 Enclave 协议增加 connection token，可以把同 route key 的多个 lease 复用到同一个 listener；这不属于 v1 生产范围。

route key：

```text
client_scope + tenant_id + account_id + target_host + target_port + normalized_redacted_proxy_key + proxy_secret_hash
```

其中 `client_scope` 来自认证后的 mTLS client identity，或本机无认证模式下的固定 local scope。`tenant_id` / `account_id` 是中转站传入的可选 opaque scope，用于 route 隔离、审计和 metrics hash；Gateway v1 默认不理解也不校验其业务归属。字段缺失时必须归一化为空字符串，不能用随机值或 request_id 代替，避免同一账号/代理 route cache 被无意义打散。需要业务级 tenant/account 授权时，应作为中转站或后续可选 client policy 扩展处理，不进入 Gateway v1 主路径。

`request_id` 只用于日志关联和排障，不进入 route key。中转站只有在账号、租户或渠道确实是 route 隔离边界时才传 `tenant_id` / `account_id`；如果两个请求传入的 scope 不同，Gateway 会把它们视为不同 route key，但不把 scope 当作授权依据。

route cache entry 状态机：

```text
missing
  -> active
  -> idle
  -> draining
  -> closed
```

route cache entry 状态含义：

- `active`：至少有一个 lane 正在使用该 route cache entry 的 proxy/direct 配置。
- `idle`：无活跃连接，等待 idle TTL 或复用。
- `draining`：配置变更、驱逐或 shutdown 中，不再接收新 lease，已有连接允许完成。
- `closed`：route cache entry 已删除或不再可用，关联 lane 都已关闭，端口已归还或等待冷却。

lane 状态机：

```text
allocating_port
  -> listening
  -> connected
  -> active
  -> released
  -> closed

allocating_port/listening
  -> expired
  -> closed
```

lane 状态含义：

- `allocating_port`：正在从端口池分配单请求 lane port。
- `listening`：lane listener 已 bind/listen 成功，可以安全写入 `REQ_HEAD.egress_port`。
- `connected`：Enclave 已连接 lane port，listener 已停止 accept。
- `active`：Enclave egress connection 和 upstream/proxy connection 正在双向拷贝。
- `released`：请求完成或失败，连接关闭，端口进入冷却。
- `expired`：Enclave 未在 lease TTL 内连接 lane port。
- `closed`：lane 已彻底关闭，端口已归还或仍在冷却期。

每次请求必须创建一个短生命周期 lease：

```text
lease_id = gateway-generated opaque id
lane_id = gateway-generated opaque id
route_id
request_id (log/correlation only)
nonce (log/correlation only)
expires_at
state = pending | connected | active | released | expired
```

`lease_id` / `lane_id` 必须由 Gateway 生成，不能由调用方可控的 `request_id`、`tenant_id`、`account_id` 或 `nonce` 直接拼接得到。`request_id` 和 `nonce` 只作为日志关联字段，用于排障和指标 exemplar，不作为内部资源主键或唯一性依据。

约束：

- `REQ_HEAD` 写给 Enclave 前，本次 lane 必须已经处于 `listening`；route cache entry 不得处于 `draining` 或 `closed`。
- route cache entry 可以被多个请求复用，但每个 lease 必须拥有独立 lane port，每个 lane port 同一时刻只接受一个 Enclave egress connection。
- 第一阶段禁止同一个 `egress_port` 同时服务多个 pending lease；迟到的第二条连接必须被拒绝并计入 `late_egress_connection`。
- route 的 effective concurrency 限制该 route key 下活跃 lease / active lane 总数。
- 全局 `POO_EGRESS_MAX_ACTIVE_LEASES` 限制所有 route key 的活跃 lane 总数。
- route idle TTL 到期后进入 `draining -> closed`，释放 route cache entry；如仍有关联 lane，必须先关闭 lane 并归还端口。
- 端口关闭后建议设置短冷却时间，避免旧连接迟到时误接入新 route。
- route 配置变更时，新请求使用新 route；旧 route 进入 draining，直到活跃连接结束或超时。

并发策略：

- Gateway v1 不接收 per-request 并发 header，不执行中转站账号级业务并发策略；`accountConcurrency` 仍由中转站自身控制。
- Gateway 只做资源保护：route 维度限制 active lease 数，全局维度限制 active lease 总数和 lane port pool。
- route effective concurrency = `min(client_policy.max_concurrency, POO_EGRESS_MAX_ROUTE_CONCURRENCY)`；如果 client policy 未配置 `max_concurrency`，则使用 `POO_EGRESS_DEFAULT_ROUTE_CONCURRENCY`，最终仍不得超过 `POO_EGRESS_MAX_ROUTE_CONCURRENCY`。
- effective concurrency 是 acquire lease 时的 admission limit；route cache entry 只维护 active lease 计数，不保存任何业务账号并发配置。
- 超过 route effective concurrency 时返回 `429 rate_limited`；全局 lane 或端口池耗尽时返回 `503 egress_capacity_exhausted`。

lane 释放规则：

- lane listener 在 Enclave 第一条 egress connection 建立后立即停止 accept。
- Enclave 未在 `POO_EGRESS_LEASE_TTL_MS` 内连接 lane port，lease 过期，lane listener 关闭，端口进入冷却。
- Adapter 取消请求、Enclave control connection 失败、proxy 连接失败、proxy 拒绝或 upstream 连接失败时，都必须关闭 lane listener、已建立的 Enclave egress connection 和 upstream/proxy connection。
- lease release 后只释放 lane 和连接资源，不删除 route cache entry；route cache entry 在 idle TTL 或 cache eviction 时删除。

### 5.4 端口分配

Gateway 需要维护一个可配置 lane port pool。v1 中 lane port pool 分配给单请求 lane，不把同一端口长期绑定到某个 route key：

```text
POO_EGRESS_PORT_RANGE=18000-18999
POO_EGRESS_PORT_COOLDOWN_MS=5000
POO_EGRESS_ROUTE_IDLE_TTL_MS=300000
```

direct-vsock / Nitro / Aliyun / QingTian 生产场景：

- `POO_EGRESS_PORT_RANGE` 表示 Enclave 可达的 vsock/direct-vsock lane port 范围，Gateway 在这些端口上为单请求 lane 监听 Enclave egress connection。
- Enclave 连接 parent CID 的 `egress_port`，该 `egress_port` 对应本次请求分配到的 lane port。
- lane port pool 可以较动态。
- 新增上游或新增代理通常不需要重构 EIF。
- 如果为了本机兼容使用 TCP lane listener，`--check-config` 必须拒绝绑定非 loopback 地址；v1 生产 QingTian 路径固定使用 direct-vsock，不把 TCP lane listener 作为生产默认形态。

端口池容量建议：

```text
required_ports >= peak_active_leases + cold_ports + surge_buffer
cold_ports ~= requests_per_second * POO_EGRESS_PORT_COOLDOWN_MS / 1000
surge_buffer 建议至少为 peak_active_leases 的 20%
```

端口耗尽必须 fail closed，返回 `egress_capacity_exhausted`，不能复用 active lane 或把请求回退到直连。

## 6. 代理协议处理

### 6.1 无代理直连

route:

```json
{
  "target_host": "api.anthropic.com",
  "target_port": 443,
  "proxy_url": ""
}
```

Gateway 数据面：

```text
Enclave -> egress_port -> Gateway -> TCP api.anthropic.com:443
```

### 6.2 HTTP/HTTPS CONNECT 代理

route:

```json
{
  "target_host": "api.openai.com",
  "target_port": 443,
  "proxy_url": "http://user:pass@proxy.example.com:8080"
}
```

Gateway 数据面：

```text
Gateway -> TCP proxy.example.com:8080
Gateway -> CONNECT api.openai.com:443 HTTP/1.1
Proxy   -> 200 Connection Established
Gateway <-> Enclave: raw tunnel bytes
Enclave -> TLS handshake SNI api.openai.com
```

对于 `https://` proxy，Gateway 先与代理建立 TLS，再在 TLS 内发送 CONNECT。

认证要求：

- proxy URL 包含 `username:password@` 时，HTTP CONNECT 必须发送 `Proxy-Authorization: Basic ...`。
- 日志和 metrics 不得输出原始用户名、密码或完整 proxy URL。
- HTTP proxy 返回非 2xx 时，route lease 失败，本次 Gateway 调用 fail closed。
- `https://` proxy transport 必须校验代理服务器证书。第一阶段可使用系统 CA；如果允许跳过校验，必须是显式 debug 配置且生产默认禁用。

### 6.3 SOCKS5/SOCKS5H 代理

route:

```json
{
  "target_host": "api.openai.com",
  "target_port": 443,
  "proxy_url": "socks5h://user:pass@proxy.example.com:1080"
}
```

Gateway 数据面：

```text
Gateway -> SOCKS5 greeting/auth
Gateway -> SOCKS5 CONNECT api.openai.com:443 for socks5h://
Gateway -> SOCKS5 CONNECT <checked_ip>:443 for socks5://
Proxy   -> success
Gateway <-> Enclave: raw tunnel bytes
Enclave -> TLS handshake SNI api.openai.com
```

`socks5://` 和 `socks5h://` 不做静默互相改写：

- `socks5h://` 使用 domain name 形式把上游 host 交给代理解析。
- `socks5://` 由 Gateway 在通过 target allowlist 和 DNS 安全检查后解析上游 host，并在 SOCKS5 CONNECT 中发送校验后的 IP；TLS SNI 和证书校验仍使用原始 `target_host`。

认证要求：

- proxy URL 包含用户名密码时，必须执行 SOCKS5 username/password authentication。
- proxy URL 不含用户名密码时，可以使用 no-auth method。
- 如果代理不接受所需认证方式，route lease 失败，本次 Gateway 调用 fail closed。

## 7. 与 proof 语义的关系

本方案不扩展 proof statement。

proof 继续证明：

- `nonce` 是本次 verifier challenge。
- `upstream_host` 是 Enclave 内 TLS 连接和签名覆盖的上游 host。
- `upstream_path` / `http_method` / `http_status` 是签名覆盖的路由事实。
- `request_body_sha256` 匹配用户实际请求 body。
- `response_body_sha256` 匹配用户实际收到的响应 body。
- signing key 绑定到 TEE evidence 和受信 measurement。

proof 不证明：

- 使用了哪个代理。
- 代理出口 IP 是多少。
- 父 VM gateway 是否按中转站期望的代理执行。
- 代理服务商是否可信。

这符合本次需求：只需要像 sub2api 一样在发起请求时指定代理地址，不要求用户侧 verifier 证明该代理地址。

## 8. 中转站接入模式

### 8.1 sub2api

sub2api 当前已经有集中式上游接口：

```text
Do(req, proxyURL, accountID, accountConcurrency)
DoWithTLS(req, proxyURL, accountID, accountConcurrency, profile)
```

推荐新增一个 TEE-aware upstream 实现：

```text
TEEHTTPUpstream.Do(req, proxyURL, accountID, accountConcurrency)
```

调用流程：

```text
1. 原服务层选择 account。
2. 读取 account.Proxy.URL() 得到 proxyURL。
3. 基于原始上游 HTTP request 构造 PoO `REQ_HEAD` / `REQ_BODY` frame。
4. TEEHTTPUpstream 调用 PoO Parent Gateway frame relay API:
   frame stream = REQ_HEAD / REQ_BODY
   metadata.proxyURL = account.Proxy.URL()
   metadata.accountID = account.ID
5. Parent Gateway 内部查找或创建 cached route，创建本次 lease，并调用 Enclave。
6. TEEHTTPUpstream 读取 Gateway 透传回来的 RESP_HEAD / RESP_CHUNK / RESP_TRAILER / ERR，并按原有 PoO adapter 逻辑转成中转站内部响应对象。
```

`accountConcurrency` 继续由 sub2api 原有账号调度和限流逻辑执行，不作为 Proof Relay API metadata 传给 Gateway。

如果 `proxyURL` 为空，Parent Gateway 内部查找或创建 direct cached route。

### 8.2 new-api

new-api 当前的 proxy 是 channel 级配置，普通路径会通过 `GetHttpClientWithProxy(info.ChannelSetting.Proxy)` 创建 HTTP client。

PoO 接入后应避免只设置 channel proxy。推荐方式：

- 在 proof adapter 中读取 `info.ChannelSetting.Proxy`。
- 调用 PoO Parent Gateway Proof Relay API，并把 channel proxy 作为 `proxy_url` 传入。
- 由 Parent Gateway 内部查找或创建 cached route、创建 lease、填充 `REQ_HEAD.egress_port` 并调用 Enclave。
- 保持用户侧 proof transport：继续使用 Enclave 原始 `RESP_TRAILER` 中的 `tee.proof`，Gateway 不新增响应封装。

### 8.3 claude-relay-service

claude-relay-service 当前通过 `ProxyHelper.createProxyAgent(account.proxy)` 给 axios/fetch 注入 agent。

PoO 接入后：

- 原 `proxyAgent` 不再直接用于访问上游模型。
- account proxy 应传给 PoO adapter。
- PoO adapter 调用 PoO Parent Gateway Proof Relay API。
- Parent Gateway 内部查找或创建 direct/proxy cached route，创建 lease 并调用 Enclave。
- Enclave 仍然对 Anthropic/OpenAI/Gemini 等上游 host 终结 TLS。

### 8.4 CLIProxyAPI

CLIProxyAPI 具有 `proxy-url`、auth 级 `ProxyURL`、ProviderExecutor 和 interceptor 扩展点。

PoO 接入可做成插件或内置 executor：

- executor 读取 auth-specific `ProxyURL`。
- 调用 PoO Parent Gateway Proof Relay API。
- Parent Gateway 内部将请求交给 Enclave。
- 按原 PoO frame stream 读取 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR`，再由 adapter/executor 按既有逻辑输出 raw body 和 proof。

## 9. 配置建议

默认主流模型目标集合：

`TEE_PROOF_ALLOWED_TARGETS` 和 `POO_EGRESS_ALLOWED_TARGETS` 都以 `host:port` 为单位。v1 应内置一组主流大模型厂商默认目标，配置为空时使用该默认集合；生产部署可以按实际接入渠道收窄或扩展，但不能使用通配域名或裸 host 隐式放行所有端口。

默认集合如下：

```text
api.openai.com:443
api.anthropic.com:443
generativelanguage.googleapis.com:443
aiplatform.googleapis.com:443
api.mistral.ai:443
api.cohere.ai:443
api.together.xyz:443
openrouter.ai:443
api.deepseek.com:443
api.x.ai:443
api.groq.com:443
api.perplexity.ai:443
api.fireworks.ai:443
api.replicate.com:443
api.novita.ai:443
api.sambanova.ai:443
api.ai21.com:443
api.cerebras.ai:443
api-inference.huggingface.co:443
router.huggingface.co:443
dashscope.aliyuncs.com:443
ark.cn-beijing.volces.com:443
ark.cn-shanghai.volces.com:443
open.bigmodel.cn:443
api.moonshot.cn:443
api.minimax.chat:443
api.stepfun.com:443
api.baichuan-ai.com:443
api.lingyiwanwu.com:443
api.siliconflow.cn:443
hunyuan.tencentcloudapi.com:443
qianfan.baidubce.com:443
```

如果 Azure OpenAI、私有云网关、区域化模型 endpoint 或企业自建 proxy-to-model endpoint 使用租户专属域名，必须按实际 `host:443` 显式加入配置或 client policy；默认集合不使用 `*.domain` 通配。非 443 上游需要 PoO frame / proof statement 扩展后再支持，不在 v1 生产范围。

默认集合是 Gateway 与 adapter 的内置兼容契约，必须版本化维护：

- 每次新增、删除或替换默认目标都必须写入 release notes，说明变更原因、影响范围和建议升级动作。
- 删除默认目标必须有兼容窗口或明确版本策略，不能在 patch 版本中静默删除仍可能被生产中转站使用的目标。
- Gateway 与中转站 adapter 必须提供 `--print-default-targets` 或等价命令，打印当前二进制内置默认集合，便于部署前比对两侧版本是否一致。
- 显式配置 `TEE_PROOF_ALLOWED_TARGETS` / `POO_EGRESS_ALLOWED_TARGETS` 会覆盖内置默认集合；生产环境建议把实际启用集合纳入配置审计和变更记录。

PoO 通用配置：

```env
TEE_PROOF_ENABLED=1
TEE_PROOF_PARENT_GATEWAY=http://127.0.0.1:15005
TEE_PROOF_ALLOWED_TARGETS=
TEE_PROOF_NONCE_HEADER=X-TEE-Nonce
TEE_PROOF_REQUIRED_HEADER=X-TEE-Proof
```

PoO Parent Gateway 配置：

```env
POO_PARENT_PROOF_RELAY_LISTEN=127.0.0.1:15005
POO_PARENT_LEGACY_INGRESS_LISTEN=127.0.0.1:15006
POO_PARENT_ADMIN_LISTEN=127.0.0.1:15007
POO_PARENT_AUTH_MODE=none
POO_PARENT_CLIENT_POLICY_FILE=
POO_PARENT_SHUTDOWN_TIMEOUT_MS=300000
POO_PARENT_VSOCK_CID=4
POO_PARENT_VSOCK_PORT=5005
POO_PARENT_VSOCK_METRICS_PORT=5006
POO_EGRESS_PORT_RANGE=18000-18999
POO_EGRESS_ROUTE_IDLE_TTL_MS=300000
POO_EGRESS_LEASE_TTL_MS=30000
POO_EGRESS_PORT_COOLDOWN_MS=5000
POO_EGRESS_PROXY_SCHEMES=http,https,socks5,socks5h
POO_EGRESS_DEFAULT_CONNECT_TIMEOUT_MS=10000
POO_EGRESS_IDLE_TIMEOUT_MS=300000
POO_EGRESS_MAX_ACTIVE_ROUTES=1000
POO_EGRESS_MAX_ACTIVE_LEASES=4096
POO_EGRESS_DEFAULT_ROUTE_CONCURRENCY=1
POO_EGRESS_MAX_ROUTE_CONCURRENCY=16
POO_EGRESS_ALLOWED_TARGETS=
POO_EGRESS_TARGET_DNS_CACHE_TTL_MS=60000
POO_RELAY_MAX_METADATA_BYTES=16384
POO_RELAY_MAX_REQ_HEAD_BYTES=1048576
POO_RELAY_MAX_FRAME_BYTES=67108864
POO_RELAY_MAX_REQUEST_BYTES=268435456
POO_RELAY_MAX_BUFFERED_BYTES=4194304
POO_RELAY_SPOOL_DIR=/var/lib/poo-parent-gateway/spool
POO_RELAY_MAX_SPOOL_BYTES=1073741824
POO_RELAY_IO_TIMEOUT_MS=300000
```

relay size limit 默认值说明：

- `POO_RELAY_MAX_METADATA_BYTES=16384`：限制 relay transport metadata 和 HTTP header 总量，避免代理凭据或业务 header 异常放大。
- `POO_RELAY_MAX_REQ_HEAD_BYTES=1048576`：限制 `REQ_HEAD` payload，必须小于等于 Enclave 侧 `MAX_REQ_HEAD`；当前 PoO Enclave 对齐值是 `1 MiB`。
- `POO_RELAY_MAX_FRAME_BYTES=67108864`：限制 `REQ_BODY` payload；当前 v1 请求体是单个 `REQ_BODY`，该值必须大于正常模型请求体峰值。
- `POO_RELAY_MAX_REQUEST_BYTES=268435456`：限制单次 relay stream 累计 request bytes，至少统计 `REQ_HEAD` 与 `REQ_BODY` payload，并建议把 frame header / length prefix 等协议开销一并计入实现口径。
- `POO_RELAY_MAX_BUFFERED_BYTES=4194304`：限制单请求 `REQ_BODY` preflight 内存暂存阈值；超过后必须进入 spool 或 fail closed，不能无限制占用内存。响应侧仍按 Enclave frame stream 透明透传，不使用这个配置做响应聚合。
- `POO_RELAY_SPOOL_DIR=/var/lib/poo-parent-gateway/spool`：限制 `REQ_BODY` preflight spool 目录。生产部署必须放在受控本地磁盘或 tmpfs，目录权限仅允许 Gateway 运行用户访问，不能与 access log、admin dump 或长期业务数据目录混用。
- `POO_RELAY_MAX_SPOOL_BYTES=1073741824`：限制全局 request spool 已用容量；容量不足、无法创建安全临时文件、写入失败或预提交 unlink 失败时 fail closed，提交前返回 `413 request_too_large` 或 `503 egress_capacity_exhausted`，不得继续调用 Enclave。请求结束后的 cleanup 失败不能改写已经开始的响应，只能记录告警、计数并依赖后台/启动清理兜底。
- `POO_RELAY_IO_TIMEOUT_MS=300000`：限制 relay transport metadata/header、首个 `REQ_HEAD`、单个 `REQ_BODY`、EOF confirmation 和响应透传的读写 idle 时间。请求转发给 Enclave 前的 timeout 可以返回 `relay_timeout`；请求转发给 Enclave 后的响应透传 timeout 只能关闭连接并记录指标，避免慢请求长期占用 lease / lane / Enclave control connection。
- 生产环境可以按业务模型请求大小和长连接响应时间调整这些值，但不能设置为 `0`、负数或 unlimited；`--check-config` 必须拒绝 `POO_RELAY_MAX_REQ_HEAD_BYTES > POO_RELAY_MAX_FRAME_BYTES`、大于 Enclave 对齐上限、spool 目录为空、spool 目录不可写或权限过宽的配置。

认证配置约束：

- `POO_PARENT_AUTH_MODE=none` 仅允许 `POO_PARENT_PROOF_RELAY_LISTEN` 绑定 `127.0.0.1`、`::1` 或使用 Unix socket。
- 如果 Proof Relay API 绑定非 loopback 地址，`--check-config` 必须拒绝 `POO_PARENT_AUTH_MODE=none`。
- `POO_PARENT_ADMIN_LISTEN` 生产默认绑定 loopback；如果绑定非 loopback 地址，必须由部署层安全组、防火墙或 service mesh 限制来源，且不能把 `/metrics` 暴露到公网或中转站业务入口。
- 远程中转站 v1 生产部署必须使用 `mtls`，并配置安全组 / 防火墙 / service mesh 来源限制和 `POO_PARENT_CLIENT_POLICY_FILE`。Gateway v1 不提供单独的来源 CIDR 环境变量；来源过滤由基础设施层负责。`hmac`、`jwt` 或 service token 不进入 v1，除非另行补充 header、key id、时间戳、replay window 和轮换策略。

`POO_PARENT_CLIENT_POLICY_FILE` 绑定 mTLS client identity 和可用目标、并发上限。生产实现必须优先使用证书 SAN 中的稳定 identity，例如 URI SAN / SPIFFE ID；`subject` 字符串只能作为兼容字段使用，不能作为新生产接入的首选 identity。策略语义如下：

```yaml
clients:
  - san_uri: "spiffe://example/prod/sub2api"
    allowed_targets: ["api.openai.com:443", "api.anthropic.com:443"]
    max_concurrency: 16

  # 兼容旧证书时才使用 subject；匹配前必须 canonicalize。
  - subject: "CN=sub2api-prod,O=example"
    allowed_targets: ["api.openai.com:443"]
    max_concurrency: 8
```

Parent Gateway 必须先从 mTLS client certificate 得到 authenticated client identity，再计算 `client_scope`：

- identity 提取优先级：`san_uri` / SPIFFE ID > `san_dns` > canonicalized `subject`。
- 同一优先级如果提取出多个能命中 policy 的 identity，必须拒绝连接并记录 `ambiguous_client_identity`，不能由实现随意选择第一条或最后一条。
- `--check-config` 必须拒绝空 identity、重复 identity、同一 identity 多条冲突 policy，以及无法 canonicalize 的 `subject`。
- effective global allowlist = 显式配置的 `POO_EGRESS_ALLOWED_TARGETS`；如果 `POO_EGRESS_ALLOWED_TARGETS` 为空，则使用内置默认主流模型目标集合。
- effective target allowlist = effective global allowlist 与 mTLS client policy `allowed_targets` 的交集；如果该 client policy 未配置 `allowed_targets`，则 effective target allowlist 等于 effective global allowlist。
- client policy 不能放大 effective global allowlist。
- 请求中的 `tenant_id` / `account_id` 作为可选 opaque scope 进入 route key、审计日志和 metrics hash；字段缺失时归一化为空，Gateway 默认不按其业务含义授权。
- client policy 中的 `max_concurrency` 仍受 `POO_EGRESS_MAX_ROUTE_CONCURRENCY` 裁剪，不能放大全局 Gateway 上限。
- 本机 `POO_PARENT_AUTH_MODE=none` 模式如果没有配置 client policy，Gateway 使用固定 `client_scope=local`。

端口迁移约定：

| Listener | 现有 `tcp-to-vsock-gateway` | PoO Parent Gateway v1 |
|---|---:|---:|
| Proof Relay API | 无 | `15005` |
| Legacy L4 ingress | `TTVG_LISTEN_ADDR=15005` | `15006` |
| Admin health/metrics | `TTVG_METRICS_ADDR=15006` | `15007` |
| Enclave relay vsock | `TTVG_VSOCK_PORT=5005` | `POO_PARENT_VSOCK_PORT=5005` |
| Enclave metrics vsock | `TTVG_VSOCK_METRICS_PORT=5006` | `POO_PARENT_VSOCK_METRICS_PORT=5006` |

兼容策略：

- 如果只启用 legacy L4 bridge，可以继续使用现有 `TTVG_*` 配置。
- 如果启用 Proof Relay API，必须使用 `POO_PARENT_*` 配置，并按上表避免 listener 冲突。
- 第一阶段可以让 `TTVG_*` 作为 legacy alias 读取，但日志和 `--check-config` 应提示迁移到 `POO_PARENT_*`。

如果中转站部署在独立机器上，Proof Relay API 可以绑定父 VM 内网地址：

```env
POO_PARENT_PROOF_RELAY_LISTEN=10.0.12.34:15005
POO_PARENT_AUTH_MODE=mtls
POO_PARENT_MTLS_CA_FILE=/etc/poo-parent-gateway/client-ca.pem
POO_PARENT_MTLS_CERT_FILE=/etc/poo-parent-gateway/server.pem
POO_PARENT_MTLS_KEY_FILE=/etc/poo-parent-gateway/server-key.pem
POO_PARENT_CLIENT_POLICY_FILE=/etc/poo-parent-gateway/client-policy.yaml
POO_EGRESS_ALLOWED_TARGETS=api.openai.com:443,api.anthropic.com:443
```

如果中转站部署在父 VM 本机，优先使用 loopback 或 Unix socket：

```env
POO_PARENT_PROOF_RELAY_LISTEN=127.0.0.1:15005
# 或：
POO_PARENT_PROOF_RELAY_UNIX_SOCKET=/run/poo-parent-gateway/proof-relay.sock
```

中转站侧配置应区分：

- `proxyURL`：账号或渠道原本绑定的上游代理。
- `TEE_PROOF_PARENT_GATEWAY`：PoO Parent Gateway 的 Proof Relay API endpoint，用于一次性提交 PoO frame stream 和可选 `proxyURL` metadata。
- `TEE_PROOF_ALLOWED_TARGETS`：中转站 adapter 侧允许访问的上游 `host:port` 列表，只作为前置校验；Parent Gateway 仍以 `POO_EGRESS_ALLOWED_TARGETS` 和 client policy 为最终准入。

中转站远程部署时，`TEE_PROOF_PARENT_GATEWAY` 指向父 VM 的内网 Proof Relay API 地址；中转站本机部署时，指向 `127.0.0.1` 或 Unix socket。两种情况下都不需要把 Enclave CID、内部 lane port 或内部 `egress_port` 暴露给中转站业务代码。

不要把 Enclave endpoint 填进 `proxyURL`，也不要把 `proxyURL` 当作 PoO control endpoint。

## 10. 安全和隔离要求

### 10.0 Proof Relay API 接入面保护

远程中转站模式下，Proof Relay API 是跨机器服务间接口，必须作为控制面保护：

- 只监听内网地址或受控 service mesh 地址，不直接暴露公网。
- v1 生产固定使用 mTLS；其它认证方式需要另行补充协议和轮换策略后才能启用。
- 按中转站实例、VPC、安全组、防火墙或 service mesh policy 限制来源；来源过滤属于基础设施层能力，不由 Gateway v1 单独配置 CIDR allowlist。
- 请求日志必须脱敏 Authorization、API key、`X-PoO-Proxy-URL`、proxy username/password。
- 对请求体大小、并发、超时、目标 host 和 proxy scheme 做硬限制。

本机中转站模式下，仍建议：

- 只监听 `127.0.0.1` 或 Unix socket。
- Unix socket 使用专用用户/用户组权限。
- 不把 internal egress listener 或 lane port 暴露到非本机网络。

### 10.1 代理 URL 校验

Gateway 必须 fail fast：

- 空 `proxyURL` 表示直连。
- 非空 `proxyURL` 必须 parse 成允许的 scheme。
- 不支持的 scheme 必须拒绝，不能回退直连。
- 日志必须脱敏用户名密码。
- `socks5://` 和 `socks5h://` 保持调用方输入语义；不支持的模式必须 fail fast，不能静默改写或回退直连。
- proxy URL 必须显式包含端口；Gateway 不做 scheme 默认端口补齐。
- proxy URL normalize 后才能进入 route key：scheme 小写、host 小写、保留显式端口、path/query/fragment 必须为空。
- route key 不保存原始 `proxyURL`，只保存 normalized redacted proxy key 和 secret hash；完整用户名密码只保存在内存中的 route secret 对象。

代理凭据生命周期：

- 请求日志、access log、metrics label、admin API、panic log 都不得输出完整 proxy URL、username、password、Authorization 或 API key。
- metrics label 只能使用 `proxy_scheme`、`target_host_port`、`client_scope_hash`、`tenant_id_hash`、`account_id_hash` 等低基数字段；禁止把完整 `proxy_url`、request path、API key 放入 label。
- route cache 中的 proxy secret 只允许内存保存，不落盘，不进入 config dump。
- proxy 凭据变更时，normalized secret hash 改变，必须生成新 route cache entry；旧 entry 进入 draining，已有 lane 完成后释放。
- 生产默认禁用 core dump；debug 日志必须有独立开关，且生产默认关闭。

### 10.2 SSRF 防护

中转站和 Gateway 都应有 allowlist，但最终准入必须由 Parent Gateway 自己强制执行：

- `TEE_PROOF_ALLOWED_TARGETS` 是中转站 adapter 侧的前置校验配置，用于尽早拒绝明显不允许的请求；配置为空时使用默认主流模型目标集合。
- `POO_EGRESS_ALLOWED_TARGETS` 是 Parent Gateway 侧的强制校验配置。effective global allowlist = 显式配置的 `POO_EGRESS_ALLOWED_TARGETS`；如果 `POO_EGRESS_ALLOWED_TARGETS` 为空，则使用内置默认主流模型目标集合。远程中转站传入的请求即使已通过 adapter 校验，也必须再次命中 Gateway allowlist。
- effective target allowlist = effective global allowlist 与 mTLS client policy `allowed_targets` 的交集；如果该 client policy 未配置 `allowed_targets`，则 effective target allowlist 等于 effective global allowlist。
- client policy 不能放大 effective global allowlist。
- `--check-config` 必须拒绝 effective target allowlist 为空的生产配置。
- `target_host:target_port` 必须在 effective target allowlist 内。
- allowlist 以 `host:port` 为单位，例如 `api.openai.com:443`，不允许只写裸 host 后隐式放行所有端口。
- v1 按既有 PoO 语义由 Enclave 对 `REQ_HEAD.upstream.host` 建 TLS；Gateway 从 `REQ_HEAD.upstream.host` 得到 `target_host`，`target_port` 固定派生为 `443`，再进入 allowlist 校验。
- v1 的 `REQ_HEAD.upstream.host` 必须是 hostname、IP literal 或 bracketed IPv6 literal，不接受 `host:port` authority；`api.example.com:8443` 这类非 443 上游不进入本期生产支持范围。
- 如果后续需要支持非 443 模型 endpoint，必须先让 PoO frame / proof statement 明确携带并签名覆盖 `upstream_port`，再更新 adapter、Gateway、verifier 和 fixture，不能只在 Gateway 内部把 tunnel 连到其它端口。
- 禁止通过 `target_host` 访问内网地址、link-local、metadata endpoint。
- allowlist 校验前必须做 canonicalization：host 小写、去除尾部点、IDNA 转 ASCII、IPv6 使用 bracket 规范化、默认端口补齐。
- `target_host` 为 IP literal 时必须显式出现在 allowlist，且默认拒绝 loopback、RFC1918、link-local、multicast、metadata endpoint 地址段。
- `target_host` 为域名时，Gateway 必须在本地解析路径建立 TCP 连接前解析 DNS，并拒绝解析到 loopback、RFC1918、link-local、multicast 或 metadata endpoint 地址段的结果；多个 A/AAAA 结果只要有一个落入禁止网段，v1 生产固定整体拒绝，不提供宽松模式开关。
- 本地解析路径包括 direct route 和 `socks5://`。这些路径必须使用同一次校验后的 IP 地址发起 TCP 连接或 SOCKS5 CONNECT，不能校验后再用原始域名触发第二次解析；TLS SNI 和证书校验仍使用原始 `target_host`。
- HTTP CONNECT proxy 和 `socks5h://` 会把 `target_host:443` 交给代理侧解析；Gateway 在这些路径上只能强制 effective target allowlist、host canonicalization 和本地可做的前置检查，代理侧 DNS 结果由中转站选择的代理承担，不进入 proof 语义。
- target DNS 检查结果只能短 TTL 缓存，TTL 不得超过 DNS response TTL 和 `POO_EGRESS_TARGET_DNS_CACHE_TTL_MS` 的较小值。

Gateway 不对中转站传入的 `proxy_url` 做额外 allowlist、DNS 解析或内网地址判断。代理地址的选择、有效性和业务风险由中转站自身负责；Gateway 只负责解析 URL、规范化 route key、脱敏日志，并在代理连接或握手失败时关闭本次调用；只有错误发生在请求转发给 Enclave 前，才可以返回 Gateway 自身错误。

### 10.3 Route 隔离

- cached route 必须绑定完整 route key：`client_scope`、可选 opaque `tenant_id` / `account_id`、`target_host:target_port`、normalized redacted proxy key 和 proxy secret hash。
- 每次请求必须创建 lease，lease 过期后必须拒绝或关闭迟到连接。
- route cache entry active 后不允许被不同 route key 覆盖。
- lane 释放时关闭 lane listener、进入端口冷却期，然后归还端口。
- 不同账号的 route 不得共享错误的 proxyURL；如果账号代理是隔离边界，`account_id` 必须进入 route key。

### 10.4 失败策略

Gateway 只对自身负责的连接和出网失败 fail closed，但错误返回形式必须遵守 frame 边界：

- route 创建失败。
- proxyURL 非法。
- proxy 连接超时、临时网络失败、proxy 认证失败或 CONNECT / SOCKS auth 被拒绝。
- Enclave 连接 egress_port 超时。

如果错误发生在请求转发给 Enclave 之前，Gateway 可以返回自身 `application/problem+json`。如果错误发生在请求已经转发给 Enclave 之后，Gateway 不再生成自身 JSON 错误；它只能透传 Enclave 原始 `ERR`、关闭连接并记录指标，或让 adapter/verifier 按缺少 `RESP_TRAILER` 的既有逻辑处理。

如果 Enclave 未返回 `RESP_TRAILER`，或 adapter / 用户侧 verifier 校验 proof 失败，Gateway 不改写响应，adapter/verifier 必须按既有强 proof 语义把本次调用视为未验证或失败。

职责边界：

- Parent Gateway 是不可信父 VM 组件，默认只负责透传 Enclave 原始 response frame，包括 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR`，不作为最终信任判断方。
- Parent Gateway 不做最终 proof 语义判断，不校验 evidence、measurement、signature、request/response hash，也不解释 Enclave `ERR` 的业务语义；它只维护连接生命周期、记录是否看到正常 stream 结束，并透传 Enclave 原始 frame。
- Relay Adapter 或用户侧 verifier 才执行最终 proof verification，包括 evidence、measurement、signature、request/response hash。
- 兼容模式可允许 fail open，但必须由 adapter 在自身对外响应协议、日志或指标中明确标记 proof unavailable。

### 10.5 生产运行、重启和滚动发布

PoO Parent Gateway 的生产部署单位是：

```text
one PoO node = one Parent Gateway instance + one Enclave runtime + local lane port pool
```

生产约束：

- 一个 `/v1/proof/relay` 请求的 route cache lookup、lane port 分配、Enclave control connection、Enclave egress connection 和 proxy/direct tunnel 必须全部在同一个 PoO node 内完成。
- 远程中转站可以通过私网 LB 访问多个 PoO node，但 LB 只能按请求分发到某一个 Parent Gateway 实例；in-flight 请求不能跨节点迁移。
- route cache 是内存态。Gateway 重启后不重建旧 route cache，不恢复旧 lane，所有 in-flight 请求都按连接中断处理；新请求重新创建 route cache entry。
- Gateway 收到 SIGTERM 或进入发布 drain 时，必须立即让 `/readyz` 返回 `503`，停止接收新的 Proof Relay API 请求和新的 route lease。
- drain 期间已建立的 lease / lane / Enclave control connection 允许在 `POO_PARENT_SHUTDOWN_TIMEOUT_MS` 内完成；超时后强制关闭，并由 adapter/verifier 按既有 PoO 逻辑视为 proof unavailable / request failed。
- drain 期间 route cache entry 不再接收新 lease；active lane 结束后关闭，idle entry 直接释放。
- 发布流程必须先让 LB 摘除节点，再等待 `poo_egress_active_leases` 和 `poo_relay_inflight_requests` 降到 0，再替换 binary / container，最后等 `/readyz` 恢复 200 后重新加入 LB。

`/readyz` 语义：

- Enclave control vsock port 可达，且可以完成轻量连接或版本探测。
- Proof Relay API listener 已启动。
- lane port pool 可分配至少一个 lane port。
- 进程未处于 draining / shutdown。

Enclave metrics vsock port 不作为 `/readyz` 的摘流条件；metrics 采集失败应通过内部监控告警暴露，不能让非关键观测面短暂异常导致可服务节点被 LB 摘除。

生产 metrics 至少包括：

```text
poo_relay_inflight_requests
poo_relay_requests_total{result}
poo_egress_route_cache_entries{state}
poo_egress_active_leases
poo_egress_lane_allocations_total{result}
poo_egress_lane_late_connections_total
poo_egress_proxy_connect_total{scheme,result,retryable}
poo_egress_bytes_total{direction}
poo_relay_spool_bytes
poo_relay_spool_files
poo_relay_spool_failures_total{reason}
poo_relay_stream_ended_before_trailer_total
poo_relay_aborted_total{stage}
poo_ready_probe_total{result}
```

指标 label 必须控制基数，禁止直接包含 `proxy_url`、request path、Authorization、API key、nonce、proof 内容或 spool 文件路径。

## 11. 平台差异

### 11.1 Nitro / Aliyun direct-vsock

适合动态 route：

```text
Enclave -> parent CID:<egress_port> -> Gateway route -> proxy/direct -> upstream
```

新增代理或新增同类 HTTPS 上游通常只需要改父 VM gateway 配置和中转站配置，不需要重构 EIF。

### 11.2 QingTian direct-vsock

本期 QingTian 生产方案固定采用 direct-vsock egress，采用同 Nitro/Aliyun 类似的动态 route 模式。

新增上游：

- 更新 Gateway 侧 `POO_EGRESS_ALLOWED_TARGETS`，并同步更新中转站 adapter 侧 `TEE_PROOF_ALLOWED_TARGETS`，两者都以 `host:port` 为单位。
- Gateway 无需为每个 host 固定 mapper。
- 中转站按请求调用 Proof Relay API。
- 不需要为新增代理或新增上游重构 EIF。

### 11.3 QingTian qproxy

qproxy 不进入本期生产支持范围，只保留为历史验证素材和后续兼容研究方向。原因是 qproxy 通常需要在 EIF 或 qproxy config 中预先声明端口；按请求动态 lane port 会放大端口池规划、发布和 PCR 变更成本。

历史 qproxy 链路如下：

```text
Enclave -> 127.0.0.1:<egress_port> inside enclave
  -> qproxy enclave
  -> qproxy host
  -> parent local <egress_port>
  -> PoO Parent Gateway egress route
  -> proxy/direct
  -> upstream
```

如果未来重新引入 qproxy 生产路径，必须另行补充端口池容量公式、qproxy 配置模板、EIF/PCR 变更流程、端口耗尽告警和灰度发布策略。在这些内容落地前，qproxy 不能作为本方案 v1 的生产部署方式。

## 12. 开发阶段划分

### 12.0 与 `feature/qingtian-enclave-proxy-egress` 分支的关系

`feature/qingtian-enclave-proxy-egress` 分支已经验证过一条 QingTian 代理出网链路：

```text
Enclave /attest
  -> qproxy enclave
  -> qproxy host
  -> 父 VM 本地正向代理
  -> upstream model HTTPS endpoint
```

该分支的主要实现点在 `enclave/src/main.rs`：

- 新增 `ForwardProxyProtocol`，支持 HTTP CONNECT 和 SOCKS5。
- 新增 `parse_forward_proxy_url()`，从 `POO_UPSTREAM_PROXY_URL` / `POO_FORWARD_PROXY_URL` / `POO_FORWARD_PROXY_PROTOCOL` 读取代理配置。
- 新增 `connect_upstream_socket()`，先连接 `egress_port` 或代理端口，再执行 HTTP CONNECT / SOCKS5 握手。
- `handle()` 从原来的 `connect_egress(head.egress_port)` 改为 `connect_upstream_socket(head.egress_port, &head.upstream.host, 443)`。
- `deploy/qingtian-runtime` 支持把 `QINGTIAN_UPSTREAM_PROXY_URL` 构建进 runtime image。
- `docs/qingtian-forward-proxy-e2e-runbook.md` 验证固定代理端口 `127.0.0.1:${PROXY_PORT}` 可用。

它可以复用的部分：

- HTTP CONNECT 握手逻辑。
- SOCKS5 domain connect 握手逻辑。
- 代理协议 parser 的一部分测试用例。
- QingTian qproxy 启动、日志和 runbook 经验。
- “Enclave 仍对真实 upstream host 建 TLS，代理只提供 tunnel”这一事实验证。

它不能直接满足本方案的地方：

- 代理地址来自 Enclave 环境变量或 build arg，不是来自每次中转站请求。
- `ReqHead` 里没有 `proxy_url` 字段。
- `ForwardProxyConfig` 只记录 `protocol` 和 `port`，没有完整 proxy host、用户名、密码。
- `parse_forward_proxy_url()` 当前解析 URL 后实际只取 authority 里的端口，proxy host 被 qproxy/父 VM 本地映射隐含掉了。
- SOCKS5 只支持 no-auth；sub2api/new-api 常见账号代理需要用户名密码。
- `https://` proxy transport 明确不支持。
- qproxy 模式仍依赖预留端口，不能让 Enclave 按任意 proxy host/port 动态建新出口。
- 如果把 `proxy_url` 放进 `REQ_HEAD`，PoO wire protocol、所有 adapter、测试 fixture 和兼容性都要一起改。

因此它更适合作为 **协议握手和 QingTian qproxy 验证素材**，不适合作为当前默认架构的主体。当前推荐仍然是把动态 proxyURL 处理放到 PoO Parent Gateway：中转站请求时传入完整 `proxyURL`，Parent Gateway 内部创建 route 并在父 VM 侧执行 direct / HTTP CONNECT / SOCKS5 / SOCKS5H。

如果选择沿用旧分支做 Enclave 内动态代理，至少需要：

- 给 `REQ_HEAD` 增加可选 `proxy_url` 或 `forward_proxy` 字段。
- 将 `connect_upstream_socket()` 改为接收每次请求的 proxy config，而不是读取环境变量。
- 扩展 proxy URL parser，保存并校验 scheme、host、port、username、password。
- 实现 HTTP CONNECT `Proxy-Authorization`。
- 实现 SOCKS5 username/password auth。
- 明确 direct-vsock 下 proxy host 如何路由到父 VM，以及端口池不足时如何 fail closed。qproxy 不进入本期生产范围。
- 更新 verifier/proof 文档，说明 `proxy_url` 不进入 proof statement，只影响 Enclave egress tunnel 选择。

这条路径能少写一部分握手代码，但会把代理协议复杂度放进 TEE 镜像，并引入 PCR/EIF 变更、凭据进入 Enclave、协议兼容面扩大等成本。除非后续明确要求“proxyURL 必须由 Enclave 自己解析和握手”，否则第一阶段不建议走这条路。

### 阶段 0：设计和 conformance fixture

- 固化本文档。
- 固定第一阶段 Proof Relay API 形态：HTTP `POST /v1/proof/relay` 承载原 PoO frame stream，支持 loopback TCP、远程 HTTP(S)、Unix socket transport。
- 固定 Proof Relay API v1 frame relay contract：请求/响应 body 都是 `application/vnd.poo.frames`，Gateway 自身只在请求转发给 Enclave 前返回 `application/problem+json` 错误。
- 定义长生命周期 route cache + 单请求 lane port + per-request lease lifecycle。
- 设计 proxyURL parse / redact / normalize 规则。
- 给 direct、HTTP CONNECT、HTTPS CONNECT、SOCKS5、SOCKS5H 链路写 fixture。

### 阶段 1：PoO Parent Gateway 统一调用入口和 egress 模块

- 在 `tcp-to-vsock-gateway` 仓库中新增 Proof Relay API、egress route manager 和 egress proxy/tunnel 模块。
- 扩展现有 gateway 配置，增加 proof relay listener、legacy ingress listener、admin listener 和 egress port range。
- 支持 Parent Gateway 内部长生命周期 route cache、单请求 lane port、idle TTL、lease TTL、端口池和端口冷却。
- 支持读取并校验完整 v1 request frame stream，暂存单个 `REQ_BODY`，再替换/填充 `egress_port` 并调用 Enclave。
- 支持 request cancellation、backpressure 和 Enclave 连接 egress port 超时处理。
- 支持 direct route。
- 支持 HTTP CONNECT proxy，包括 Basic `Proxy-Authorization`。
- 支持 HTTPS CONNECT proxy，包括代理 TLS 证书校验。
- 支持 SOCKS5 / SOCKS5H proxy，包括 no-auth 和 username/password auth。
- 支持生产级 metrics、日志脱敏、route secret 内存生命周期和 shutdown drain。

### 阶段 2：参考 Relay Adapter

- 在现有示例 adapter 中增加 `proxyURL` 输入。
- 调用 PoO Parent Gateway Proof Relay API。
- 覆盖 Enclave 原始 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR` frame transport。
- adapter 继续按自身对外协议处理客户端侧 `X-TEE-Proof: required` 等强 proof 策略；Gateway 不解释该字段。
- v1 请求侧为完成 EOF preflight，会 bounded buffer/spool 单个 `REQ_BODY`；响应侧仍保持透明流式透传，不额外 buffer、不剥离、不重组。

### 阶段 3：中转站接入样例

- sub2api：基于 `HTTPUpstream` 增加 TEE mode 或 `TEEHTTPUpstream`。
- new-api：在 proof adapter 中读取 channel proxy。
- claude-relay-service：把 account proxy 从 axios agent 改为传给 PoO adapter。
- CLIProxyAPI：用 plugin executor 或内置 executor 接入。

### 阶段 4：生产运行能力

- systemd / Docker 部署模板。
- QingTian direct-vsock 部署模板。
- LB `/readyz` 摘流、SIGTERM drain、滚动发布 runbook。
- route metrics：
  - active routes
  - idle cached routes
  - active leases
  - active lane ports
  - route creation failures
  - proxy connect failures
  - proxy rejected failures
  - late egress connections
  - egress bytes
  - stream ended before trailer / relay aborted
- 压测和端口池容量建议。

## 13. 验证计划

### 13.1 单元测试

- `proxyURL` parse：
  - 空字符串为 direct。
  - `http/https/socks5/socks5h` 成功。
  - `socks5` 和 `socks5h` 保持输入语义；不支持的模式明确拒绝。
  - 用户名密码解析、脱敏和 percent-decoding 边界。
  - `X-PoO-Proxy-URL` header 缺失或空值为 direct；含 CR/LF、控制字符、前后空白、超出固定长度上限、非 absolute URI、缺失显式端口、path/query/fragment 非空时返回 `invalid_proxy_url`。
  - proxy username / password 只 percent-decode 一次；双重编码不会被反复 decode 成新的特殊字符。
  - 非法 scheme fail fast。
  - Gateway request log、access log、metrics、admin 输出都不得包含 `X-PoO-Proxy-URL` 原值、proxy username、proxy password 或完整 proxy URL。
- Proof Relay API schema：
  - 首个 `REQ_HEAD` frame 缺失或字段错误返回 `bad_request`。
  - v1 请求序列只接受 `REQ_HEAD` (`0x01`) 后跟恰好一个 `REQ_BODY` (`0x02`)；空 body 使用 zero-length `REQ_BODY`。
  - 首个 `REQ_HEAD` 后再次出现 `REQ_HEAD`、第二个 `REQ_BODY`、`RESP_*`、未知控制 frame、或会改变 upstream / egress 语义的 frame 时，在调用 Enclave 前返回 `bad_request`。
  - `REQ_BODY` 后出现尾随字节时，在调用 Enclave 前返回 `bad_request`，且不得创建 lane 或连接 Enclave control port。
  - `REQ_BODY` 后出现额外 frame 时，在调用 Enclave 前返回 `bad_request`，且不得创建 lane 或连接 Enclave control port。
  - `REQ_HEAD.upstream.host` 为空或不能 canonicalize 时返回 `bad_request`。
  - `REQ_HEAD.upstream.host` 包含 `host:port` authority 或非 443 端口意图时返回 `bad_request`；v1 只派生 `target_port=443`。
  - Gateway 不接受也不解释 proof mode；上游 HTTP header 或客户端侧 `X-TEE-Proof` 不影响 Gateway 行为。
  - Gateway 替换/填充 `REQ_HEAD.egress_port` 后，其它 `REQ_HEAD` 字段保持不变。
  - `POO_RELAY_MAX_METADATA_BYTES`、`POO_RELAY_MAX_REQ_HEAD_BYTES`、`POO_RELAY_MAX_FRAME_BYTES`、`POO_RELAY_MAX_REQUEST_BYTES`、`POO_RELAY_MAX_BUFFERED_BYTES` 或 `POO_RELAY_MAX_SPOOL_BYTES` 超限时，在调用 Enclave 前返回 `413 request_too_large`，且不得创建 lane 或连接 Enclave control port。
  - `REQ_BODY` 大于 `POO_RELAY_MAX_BUFFERED_BYTES` 但未超过 `POO_RELAY_MAX_FRAME_BYTES` / `POO_RELAY_MAX_REQUEST_BYTES` 时进入 spool；spool 文件权限、unlink-on-open / cleanup 和敏感信息不落日志都必须覆盖。
  - spool 使用量、文件数和失败原因必须有脱敏 metrics；metrics label 不得包含 spool 文件路径、proxy URL、request path、nonce 或 proof 内容。
  - `POO_RELAY_IO_TIMEOUT_MS` 覆盖 metadata/header、首个 `REQ_HEAD`、单个 `REQ_BODY`、EOF confirmation 和响应透传 idle timeout；请求转发给 Enclave 前超时不得创建 lane 或连接 Enclave，并返回 `relay_timeout`；请求转发给 Enclave 后的响应透传 idle timeout 只能关闭连接并记录指标。
  - Enclave 返回 `ERR` frame 时，Gateway 原样透传，不改写为 Gateway 自身 `application/problem+json`。
- client policy：
  - mTLS client `san_uri` / SPIFFE ID 优先映射到 allowed targets 和 max concurrency。
  - 同一优先级多个 identity 命中 policy 时拒绝连接并记录 `ambiguous_client_identity`。
  - 旧证书 `subject` 匹配前必须 canonicalize。
  - 空 identity、重复 identity、同一 identity 多条冲突 policy 和无法 canonicalize 的 `subject` 都会让 `--check-config` 失败。
  - 本机 `AUTH_MODE=none` 且未配置 client policy 时使用 `client_scope=local`。
  - route key 使用 authenticated `client_scope`，并把请求中的 `tenant_id` / `account_id` 作为可选 opaque scope；字段缺失时归一化为空，不用 `request_id` 代替。
  - client policy `allowed_targets` 不能放大 effective global allowlist；`POO_EGRESS_ALLOWED_TARGETS` 为空时，effective global allowlist 是内置默认主流模型目标集合。
- route manager：
  - 分配端口。
  - cached route key 相同则复用 route cache entry。
  - 每个 lease 分配独立 lane port。
  - 同一个 lane port 不得绑定多个 pending lease。
  - cached route key 不同则不得复用 route cache entry。
  - lease TTL 过期释放。
  - route idle TTL 过期释放。
  - draining route 不接收新 lease。
  - 端口冷却期间不得复用给新 route。
  - active route cache entry 不被不同 key 覆盖。
  - single-use lane 第一条 Enclave egress connection 绑定成功后立即停止 accept。
  - 同一个 `lane_id` / `egress_port` 的第二条 Enclave egress connection 必须被拒绝并记录 late/duplicate 指标。
  - lane lease 过期后到达的 Enclave egress connection 必须被拒绝。
  - 已完成 lane port 必须进入 cooldown，cooldown 结束前不得重新分配。
  - route effective concurrency 被 client policy 和 gateway policy 裁剪。
  - client policy `max_concurrency=4`、`POO_EGRESS_MAX_ROUTE_CONCURRENCY=16` 时，route effective concurrency 为 `4`。
  - client policy 未配置 `max_concurrency`、`POO_EGRESS_DEFAULT_ROUTE_CONCURRENCY=1`、`POO_EGRESS_MAX_ROUTE_CONCURRENCY=16` 时，route effective concurrency 为 `1`。
  - 中转站账号级 `accountConcurrency` 不传给 Gateway；route cache entry 不保存业务账号并发配置。
  - route effective concurrency 超限返回 `rate_limited`。
  - 全局 lane 或端口池耗尽返回 `egress_capacity_exhausted`。
- 安全配置：
  - `POO_PARENT_AUTH_MODE=none` 绑定非 loopback 地址时 `--check-config` 失败。
  - 远程 `POO_PARENT_AUTH_MODE=mtls` 但未配置 `POO_PARENT_CLIENT_POLICY_FILE` 时 `--check-config` 失败。
  - `TEE_PROOF_ALLOWED_TARGETS` 和 `POO_EGRESS_ALLOWED_TARGETS` 为空时使用默认主流模型目标集合。
  - 生产启用 Proof Relay API 但 effective target allowlist 为空时 `--check-config` 失败。
  - relay stream 大小、内部缓冲限制、`POO_RELAY_MAX_SPOOL_BYTES` 和 `POO_RELAY_IO_TIMEOUT_MS` 为 0、负数或无限制时，生产 `--check-config` 失败。
  - `POO_RELAY_SPOOL_DIR` 为空、不可写、权限过宽或与 access log / admin dump / 长期业务数据目录混用时，生产 `--check-config` 失败。
  - `POO_RELAY_MAX_REQ_HEAD_BYTES > POO_RELAY_MAX_FRAME_BYTES` 或大于 Enclave 对齐上限时，生产 `--check-config` 失败。
  - Proof Relay API listener 只暴露 `POST /v1/proof/relay`；admin listener 暴露 `/healthz`、`/readyz`、`/metrics`；Proof Relay API listener 不暴露 `/metrics`。
  - `/readyz` 只返回最小健康状态，不泄漏 route、proxy、tenant、account、proof 或配置详情。
  - 本地解析路径的 target DNS 解析到内网、link-local、metadata 或 multicast 地址时拒绝。
  - direct route 和 `socks5://` 使用校验后的 IP 建连或发起 SOCKS5 CONNECT，TLS SNI 仍使用原始 host。
  - HTTP CONNECT proxy 和 `socks5h://` 路径不声称 Gateway 能约束代理侧 DNS 结果；测试只验证 Gateway 发送 allowlist 内的 `target_host:443`。

### 13.2 集成测试

- direct route：Enclave -> Gateway -> mock upstream。
- HTTP proxy route：Enclave -> Gateway -> mock HTTP CONNECT proxy -> mock upstream。
- HTTPS proxy route：Enclave -> Gateway -> mock HTTPS CONNECT proxy -> mock upstream。
- SOCKS5 route：Enclave -> Gateway -> mock SOCKS5 proxy -> mock upstream，Gateway 本地解析并发送校验后的 IP。
- SOCKS5H route：Enclave -> Gateway -> mock SOCKS5 proxy -> mock upstream。
- HTTP CONNECT 和 SOCKS5H route：mock proxy 看到的 CONNECT target 必须是 canonicalized `target_host:443`，Gateway 不把 proxy 侧 DNS 结果写入 proof 或 metrics label。
- HTTP proxy Basic 认证成功和失败。
- SOCKS5 username/password 认证成功和失败。
- proxy 临时网络失败发生在 Enclave egress data plane 阶段时，Gateway 不返回自身 `502`；测试应验证 Enclave 原始 `ERR` 被透传，或连接被关闭且记录 `proxy_connect_failed` 脱敏指标。
- proxy 认证失败、CONNECT 被拒绝或 SOCKS auth 失败发生在 Enclave egress data plane 阶段时，Gateway 不返回自身 `502`；测试应验证 Enclave 原始 `ERR` 被透传，或连接被关闭且记录 `proxy_rejected` 脱敏指标。
- 代理连错上游：Enclave TLS 校验失败或 proof 验证失败。
- Enclave 未连接 egress port：lease 超时发生在 Enclave egress data plane 阶段时，Gateway 不返回自身 `504`；测试应验证 Enclave 原始 `ERR` 被透传，或连接被关闭且记录 `egress_connect_timeout` 脱敏指标，route 保持可复用或按策略释放。
- single-use lane 重放：同一 `egress_port` 的第二条连接、lease 过期后的迟到连接、请求完成后的重复连接都必须被拒绝。
- 端口池耗尽：请求 fail closed，不复用错误 route。
- request cancellation：adapter 断开后释放 lease 并关闭 control/upstream 连接。
- relay timeout：metadata/header、`REQ_HEAD`、`REQ_BODY`、EOF confirmation 超时时不得创建 lane 或连接 Enclave，并返回 `relay_timeout`；响应透传 idle 超时后释放已有 lease / lane 并关闭连接，且不得返回 Gateway 自身 `application/problem+json`。
- spool preflight failure：spool 容量不足、目录不可写、临时文件创建失败、写入失败或预提交 unlink 失败时 fail closed，不调用 Enclave，并记录脱敏指标；错误响应、日志和 metrics label 都不得暴露 spool 文件路径或请求内容。
- spool cleanup failure：请求结束后的 cleanup 失败不能改写已经开始的 Enclave frame 响应，只记录告警和脱敏指标，并依赖后台/启动清理兜底。
- late egress connection：lane 已绑定或已释放后再次连接时拒绝并记录指标。
- SIGTERM drain：`/readyz` 返回 503，新请求拒绝，已有 lease 在 shutdown timeout 内完成。
- `/readyz` 在 Enclave control vsock 不可达时返回 503。
- mTLS 远程接入：无客户端证书、错误 CA、过期证书都被拒绝。
- frame 透传一致性：mock Enclave 返回的 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR` bytes 与 adapter 收到的 bytes 一致，Gateway 不插入私有响应 envelope。
- admin endpoint 暴露面：远程 Proof Relay API listener 访问 `/metrics` 返回 404/405；受控 admin listener 可以抓取 `/metrics`，LB 访问 `/readyz` 只能得到最小健康信息。

### 13.3 端到端测试

- 远程中转站模式：
  - adapter 通过 VPC/private addr 调用 Proof Relay API。
  - 未授权调用被拒绝。
  - mTLS client A 只能访问自身 policy 允许的 target；`tenant_id` / `account_id` 作为可选 opaque scope 进入 route key 和审计日志，不触发业务授权错误。
- 父 VM 本机中转站模式：
  - adapter 通过 `127.0.0.1` 或 Unix socket 调用 Proof Relay API。
- 非流式和流式响应：
  - adapter 按原 PoO frame protocol 接收 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR`。
  - verifier 使用 Enclave 原始 `RESP_TRAILER` 中的 proof 校验 request/response hash。
- sub2api 风格账号代理：
  - 账号 A 使用 proxy A。
  - 账号 B 使用 proxy B。
  - route 不串用。
  - 相同账号/上游/proxy 的 route cache 被复用。

## 14. 兼容性说明

Gateway 不改变 Enclave 到 adapter 的响应协议。接入前 adapter 如何消费 Enclave 的 `RESP_HEAD` / `RESP_CHUNK` / `RESP_TRAILER` / `ERR`，接入 Parent Gateway 后仍然如何消费；Gateway 只在父 VM 侧完成 lane 分配和 proxy/direct egress。

因此，客户端最终看到的响应格式仍由中转站 adapter 决定：

- 普通 JSON-only 客户端仍由 adapter 返回原业务 JSON。
- proof 仍来自 Enclave 原始 `RESP_TRAILER`，adapter 可以继续按既有方式放入响应 header、日志审计 sidecar、单独 proof 查询接口或其它 proof transport。
- 流式响应是否把 proof 缺失前已经透传的内容交给最终用户，由业务方/中转站 adapter 自行决定；Gateway 不额外 buffer、不剥离、不重组响应。

这与按请求代理出网是两个独立问题。代理出网能力只决定 Enclave 到上游的 egress 路径；proof transport 决定用户如何拿到并验证 `tee.proof`。

## 15. 开放问题

- 是否提供官方 sub2api/new-api adapter patch，还是只提供 PoO SDK 和文档？
- 后续是否在 Enclave egress connection 增加 connection token，以允许同 route key 多 lease 复用同一个长期 listener？

## 16. 推荐结论

第一阶段推荐实现：

```text
中转站 adapter 接收 proxyURL
  -> PoO Parent Gateway Proof Relay API 一次性接收 PoO frame stream + proxyURL metadata
  -> Parent Gateway 内部查找或创建 cached route，并为本次请求分配单请求 lane_port
  -> Parent Gateway 为本次请求创建短生命周期 lease
  -> Parent Gateway 只填充/替换 REQ_HEAD.egress_port，并透传 REQ_HEAD / REQ_BODY 到 Enclave
  -> Enclave 使用 REQ_HEAD.egress_port 连接父 VM egress tunnel
  -> Parent Gateway 根据内部 route 直连或走指定代理
  -> Enclave 对 upstream_host 建 TLS 并生成原有 tee.proof
```

这意味着“中转站 adapter 在发送 PoO `REQ_HEAD` 前先注册 single-use lane”可以省略，并且推荐省略。`egress_port` 不应成为普通中转站 adapter 的外部接口；它应是 PoO Parent Gateway 内部协调 Enclave egress 的实现细节。

该方案兼容两种中转站部署方式：中转站可以远程部署在独立机器上，通过 VPC/private network 调用父 VM 上的 Proof Relay API；也可以直接部署在父 VM 本机，通过 `127.0.0.1` 或 Unix socket 调用同一个 API。两种方式共享同一套 Parent Gateway 内部 route / vsock / egress 逻辑。

代价是 PoO Parent Gateway 必须从纯 L4 透明网关升级为带一个 PoO-aware Proof Relay API 的父 VM 组件。现有 L4 ingress bridge 仍可保留，但 v1 只承诺原有无动态 proxy 的兼容透传；它不能独立完成“分配 lane + 指定 proxy + 调用 Enclave”的统一调用。

这条路径满足“像 sub2api 一样发起请求时指定代理地址”，同时把中转站接入面控制成一次调用。PoO proof statement、verifier 和 Enclave 内核心证明语义可以保持不变。
