# tcp-to-vsock-gateway

`tcp-to-vsock-gateway` is a small L4 transparent bridge from TCP to
`AF_VSOCK`. It lets relay services connect to a `proof-of-observation` runtime
inside an Enclave through a normal private TCP endpoint.

It does not parse, verify, generate, or rewrite proof data. Each TCP connection
is mapped to one Enclave vsock connection and bytes are copied unchanged in both
directions.

Supported target binaries:

| Provider | Target binary |
|---|---|
| AWS Nitro Enclave | `linux/arm64` |
| Alibaba Cloud Enclave | `linux/amd64` |
| Huawei QingTian Enclave | `linux/amd64` |

See [docs/tcp-to-vsock-gateway-design.md](docs/tcp-to-vsock-gateway-design.md)
for the full production deployment design.

For concrete commands on a real Enclave parent VM, see
[docs/parent-vm-deployment-runbook.md](docs/parent-vm-deployment-runbook.md).

For PoO Parent Gateway deployment with per-request proxy egress, see
[docs/poo-parent-gateway-deployment.md](docs/poo-parent-gateway-deployment.md).

## Build

```bash
go test ./...

GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w" \
  ./cmd/tcp-to-vsock-gateway

GOOS=linux GOARCH=arm64 go build \
  -trimpath \
  -ldflags="-s -w" \
  ./cmd/tcp-to-vsock-gateway
```

## Run

```bash
TTVG_LISTEN_ADDR=127.0.0.1:15005 \
TTVG_VSOCK_CID=4 \
TTVG_VSOCK_PORT=5005 \
TTVG_METRICS_ADDR=127.0.0.1:15006 \
TTVG_VSOCK_METRICS_PORT=5006 \
tcp-to-vsock-gateway
```

Health endpoints:

```bash
curl http://127.0.0.1:15006/healthz
curl http://127.0.0.1:15006/readyz
curl http://127.0.0.1:15006/metrics
```

## Configuration

Required:

- `TTVG_LISTEN_ADDR`: TCP listen address, for example `127.0.0.1:15005`.
- `TTVG_VSOCK_CID`: target Enclave CID.

Important optional values:

- `TTVG_VSOCK_PORT`: target relay port, default `5005`.
- `TTVG_METRICS_ADDR`: admin HTTP address, default `127.0.0.1:15006`.
- `TTVG_VSOCK_METRICS_PORT`: Enclave metrics port for readiness, default `5006`.
- `TTVG_CONNECT_TIMEOUT`: vsock dial/probe timeout, default `5s`.
- `TTVG_IDLE_TIMEOUT`: per-direction idle timeout, default `300s`.
- `TTVG_MAX_CONNS`: max concurrent bridged connections, default `1024`.
- `TTVG_SHUTDOWN_TIMEOUT`: graceful drain timeout, default `300s`.

Validate configuration without starting listeners:

```bash
TTVG_LISTEN_ADDR=127.0.0.1:15005 \
TTVG_VSOCK_CID=4 \
tcp-to-vsock-gateway --check-config
```
