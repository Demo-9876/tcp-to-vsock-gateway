package metrics

import (
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"
)

type Metrics struct {
	connectionsActive atomic.Int64
	connectionsTotal  atomic.Uint64
	rejectedLimit     atomic.Uint64
	rejectedShutdown  atomic.Uint64
	vsockDialTotal    atomic.Uint64
	vsockDialErrors   atomic.Uint64
	copyErrors        atomic.Uint64
	bytesTCPToVsock   atomic.Uint64
	bytesVsockToTCP   atomic.Uint64
	connDurationCount atomic.Uint64
	connDurationSumNS atomic.Uint64

	readinessProbeTotal    atomic.Uint64
	readinessProbeErrors   atomic.Uint64
	readinessProbeCount    atomic.Uint64
	readinessProbeSumNS    atomic.Uint64
	readinessCacheHits     atomic.Uint64
	readinessLastReady     atomic.Int64
	readinessLastProbeUnix atomic.Int64

	proofRelayRequestsTotal          atomic.Uint64
	proofRelayPreflightBadRequest    atomic.Uint64
	proofRelayPreflightForbidden     atomic.Uint64
	proofRelayPreflightRequestTooBig atomic.Uint64
	proofRelayPreflightTimeout       atomic.Uint64
	proofRelayPreflightUnavailable   atomic.Uint64
	proofRelayAbortEnclaveWrite      atomic.Uint64
	proofRelayAbortEnclaveRead       atomic.Uint64
	proofRelayAbortClientWrite       atomic.Uint64
	proofRelaySpoolBytesActive       atomic.Int64
	proofRelaySpoolFailures          atomic.Uint64

	egressRoutesCachedActive      atomic.Int64
	egressRoutesCachedIdle        atomic.Int64
	egressLeasesActive            atomic.Int64
	egressLanePortsActive         atomic.Int64
	egressRouteFailureTarget      atomic.Uint64
	egressRouteFailureRateLimited atomic.Uint64
	egressRouteFailureCapacity    atomic.Uint64
	egressRouteFailureDraining    atomic.Uint64
	egressRouteFailureListen      atomic.Uint64
	egressProxyFailureConnect     atomic.Uint64
	egressProxyFailureRejected    atomic.Uint64
	egressLateConnections         atomic.Uint64
	egressBytesEnclaveToUpstream  atomic.Uint64
	egressBytesUpstreamToEnclave  atomic.Uint64
}

func New() *Metrics {
	return &Metrics{}
}

func (m *Metrics) IncConnectionsTotal() {
	m.connectionsTotal.Add(1)
}

func (m *Metrics) IncConnectionsActive() {
	m.connectionsActive.Add(1)
}

func (m *Metrics) DecConnectionsActive() {
	m.connectionsActive.Add(-1)
}

func (m *Metrics) IncRejected(reason string) {
	switch reason {
	case "shutdown":
		m.rejectedShutdown.Add(1)
	default:
		m.rejectedLimit.Add(1)
	}
}

func (m *Metrics) IncVsockDial(ok bool) {
	m.vsockDialTotal.Add(1)
	if !ok {
		m.vsockDialErrors.Add(1)
	}
}

func (m *Metrics) IncCopyError() {
	m.copyErrors.Add(1)
}

func (m *Metrics) AddBytesTCPToVsock(n uint64) {
	m.bytesTCPToVsock.Add(n)
}

func (m *Metrics) AddBytesVsockToTCP(n uint64) {
	m.bytesVsockToTCP.Add(n)
}

func (m *Metrics) ObserveConnectionDuration(d time.Duration) {
	m.connDurationCount.Add(1)
	m.connDurationSumNS.Add(uint64(maxDuration(d, 0)))
}

func (m *Metrics) ObserveReadinessProbe(d time.Duration, ok bool) {
	m.readinessProbeTotal.Add(1)
	m.readinessProbeCount.Add(1)
	m.readinessProbeSumNS.Add(uint64(maxDuration(d, 0)))
	m.readinessLastProbeUnix.Store(time.Now().Unix())
	if ok {
		m.readinessLastReady.Store(1)
	} else {
		m.readinessProbeErrors.Add(1)
		m.readinessLastReady.Store(0)
	}
}

func (m *Metrics) IncReadinessCacheHit() {
	m.readinessCacheHits.Add(1)
}

func (m *Metrics) IncProofRelayRequest() {
	if m == nil {
		return
	}
	m.proofRelayRequestsTotal.Add(1)
}

func (m *Metrics) IncProofRelayPreflightFailure(reason string) {
	if m == nil {
		return
	}
	switch reason {
	case "forbidden":
		m.proofRelayPreflightForbidden.Add(1)
	case "request_too_large":
		m.proofRelayPreflightRequestTooBig.Add(1)
	case "relay_timeout":
		m.proofRelayPreflightTimeout.Add(1)
	case "unavailable":
		m.proofRelayPreflightUnavailable.Add(1)
	default:
		m.proofRelayPreflightBadRequest.Add(1)
	}
}

func (m *Metrics) IncProofRelayAbort(reason string) {
	if m == nil {
		return
	}
	switch reason {
	case "enclave_write":
		m.proofRelayAbortEnclaveWrite.Add(1)
	case "enclave_read":
		m.proofRelayAbortEnclaveRead.Add(1)
	default:
		m.proofRelayAbortClientWrite.Add(1)
	}
}

func (m *Metrics) AddProofRelaySpoolBytes(delta int64) {
	if m == nil || delta == 0 {
		return
	}
	m.proofRelaySpoolBytesActive.Add(delta)
}

func (m *Metrics) IncProofRelaySpoolFailure() {
	if m == nil {
		return
	}
	m.proofRelaySpoolFailures.Add(1)
}

func (m *Metrics) SetEgressRouteGauges(activeRoutes, idleRoutes, activeLeases, activeLanePorts int) {
	if m == nil {
		return
	}
	m.egressRoutesCachedActive.Store(int64(activeRoutes))
	m.egressRoutesCachedIdle.Store(int64(idleRoutes))
	m.egressLeasesActive.Store(int64(activeLeases))
	m.egressLanePortsActive.Store(int64(activeLanePorts))
}

func (m *Metrics) IncEgressRouteFailure(reason string) {
	if m == nil {
		return
	}
	switch reason {
	case "target_not_allowed":
		m.egressRouteFailureTarget.Add(1)
	case "rate_limited":
		m.egressRouteFailureRateLimited.Add(1)
	case "draining":
		m.egressRouteFailureDraining.Add(1)
	case "listen_failed":
		m.egressRouteFailureListen.Add(1)
	default:
		m.egressRouteFailureCapacity.Add(1)
	}
}

func (m *Metrics) IncEgressProxyFailure(reason string) {
	if m == nil {
		return
	}
	if reason == "rejected" {
		m.egressProxyFailureRejected.Add(1)
		return
	}
	m.egressProxyFailureConnect.Add(1)
}

func (m *Metrics) IncEgressLateConnection() {
	if m == nil {
		return
	}
	m.egressLateConnections.Add(1)
}

func (m *Metrics) AddEgressBytes(direction string, n uint64) {
	if m == nil || n == 0 {
		return
	}
	if direction == "upstream_to_enclave" {
		m.egressBytesUpstreamToEnclave.Add(n)
		return
	}
	m.egressBytesEnclaveToUpstream.Add(n)
}

func (m *Metrics) Prometheus() string {
	var b strings.Builder
	line := func(name string, value any) {
		fmt.Fprintf(&b, "%s %v\n", name, value)
	}
	line(`ttvg_connections_active`, m.connectionsActive.Load())
	line(`ttvg_connections_total`, m.connectionsTotal.Load())
	line(`ttvg_connections_rejected_total{reason="limit"}`, m.rejectedLimit.Load())
	line(`ttvg_connections_rejected_total{reason="shutdown"}`, m.rejectedShutdown.Load())
	line(`ttvg_vsock_dial_total`, m.vsockDialTotal.Load())
	line(`ttvg_vsock_dial_errors_total`, m.vsockDialErrors.Load())
	line(`ttvg_copy_errors_total`, m.copyErrors.Load())
	line(`ttvg_bytes_tcp_to_vsock_total`, m.bytesTCPToVsock.Load())
	line(`ttvg_bytes_vsock_to_tcp_total`, m.bytesVsockToTCP.Load())
	line(`ttvg_connection_duration_seconds_count`, m.connDurationCount.Load())
	line(`ttvg_connection_duration_seconds_sum`, seconds(m.connDurationSumNS.Load()))
	line(`ttvg_readiness_probe_total`, m.readinessProbeTotal.Load())
	line(`ttvg_readiness_probe_errors_total`, m.readinessProbeErrors.Load())
	line(`ttvg_readiness_probe_duration_seconds_count`, m.readinessProbeCount.Load())
	line(`ttvg_readiness_probe_duration_seconds_sum`, seconds(m.readinessProbeSumNS.Load()))
	line(`ttvg_readiness_cache_hits_total`, m.readinessCacheHits.Load())
	line(`ttvg_readiness_last_ready`, m.readinessLastReady.Load())
	line(`ttvg_readiness_last_probe_unixtime`, m.readinessLastProbeUnix.Load())
	line(`ttvg_proof_relay_requests_total`, m.proofRelayRequestsTotal.Load())
	line(`ttvg_proof_relay_preflight_failures_total{reason="bad_request"}`, m.proofRelayPreflightBadRequest.Load())
	line(`ttvg_proof_relay_preflight_failures_total{reason="forbidden"}`, m.proofRelayPreflightForbidden.Load())
	line(`ttvg_proof_relay_preflight_failures_total{reason="request_too_large"}`, m.proofRelayPreflightRequestTooBig.Load())
	line(`ttvg_proof_relay_preflight_failures_total{reason="relay_timeout"}`, m.proofRelayPreflightTimeout.Load())
	line(`ttvg_proof_relay_preflight_failures_total{reason="unavailable"}`, m.proofRelayPreflightUnavailable.Load())
	line(`ttvg_proof_relay_aborts_total{reason="enclave_write"}`, m.proofRelayAbortEnclaveWrite.Load())
	line(`ttvg_proof_relay_aborts_total{reason="enclave_read"}`, m.proofRelayAbortEnclaveRead.Load())
	line(`ttvg_proof_relay_aborts_total{reason="client_write"}`, m.proofRelayAbortClientWrite.Load())
	line(`ttvg_proof_relay_spool_bytes_active`, m.proofRelaySpoolBytesActive.Load())
	line(`ttvg_proof_relay_spool_failures_total`, m.proofRelaySpoolFailures.Load())
	line(`ttvg_egress_routes_cached_active`, m.egressRoutesCachedActive.Load())
	line(`ttvg_egress_routes_cached_idle`, m.egressRoutesCachedIdle.Load())
	line(`ttvg_egress_leases_active`, m.egressLeasesActive.Load())
	line(`ttvg_egress_lane_ports_active`, m.egressLanePortsActive.Load())
	line(`ttvg_egress_route_failures_total{reason="target_not_allowed"}`, m.egressRouteFailureTarget.Load())
	line(`ttvg_egress_route_failures_total{reason="rate_limited"}`, m.egressRouteFailureRateLimited.Load())
	line(`ttvg_egress_route_failures_total{reason="capacity_exhausted"}`, m.egressRouteFailureCapacity.Load())
	line(`ttvg_egress_route_failures_total{reason="draining"}`, m.egressRouteFailureDraining.Load())
	line(`ttvg_egress_route_failures_total{reason="listen_failed"}`, m.egressRouteFailureListen.Load())
	line(`ttvg_egress_proxy_failures_total{reason="connect"}`, m.egressProxyFailureConnect.Load())
	line(`ttvg_egress_proxy_failures_total{reason="rejected"}`, m.egressProxyFailureRejected.Load())
	line(`ttvg_egress_late_connections_total`, m.egressLateConnections.Load())
	line(`ttvg_egress_bytes_enclave_to_upstream_total`, m.egressBytesEnclaveToUpstream.Load())
	line(`ttvg_egress_bytes_upstream_to_enclave_total`, m.egressBytesUpstreamToEnclave.Load())
	return b.String()
}

func seconds(ns uint64) string {
	v := float64(ns) / float64(time.Second)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	return fmt.Sprintf("%.9f", v)
}

func maxDuration(d, min time.Duration) time.Duration {
	if d < min {
		return min
	}
	return d
}
