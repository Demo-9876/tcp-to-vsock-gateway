package metrics

import (
	"strings"
	"testing"
)

func TestPrometheusIncludesProofRelayAndEgressMetrics(t *testing.T) {
	m := New()
	m.IncProofRelayRequest()
	m.IncProofRelayPreflightFailure("request_too_large")
	m.IncProofRelayAbort("enclave_read")
	m.AddProofRelaySpoolBytes(1024)
	m.IncProofRelaySpoolFailure()
	m.SetEgressRouteGauges(1, 2, 3, 4)
	m.IncEgressRouteFailure("rate_limited")
	m.IncEgressProxyFailure("rejected")
	m.IncEgressLateConnection()
	m.AddEgressBytes("enclave_to_upstream", 11)
	m.AddEgressBytes("upstream_to_enclave", 13)

	got := m.Prometheus()
	for _, want := range []string{
		`ttvg_proof_relay_requests_total 1`,
		`ttvg_proof_relay_preflight_failures_total{reason="request_too_large"} 1`,
		`ttvg_proof_relay_aborts_total{reason="enclave_read"} 1`,
		`ttvg_proof_relay_spool_bytes_active 1024`,
		`ttvg_proof_relay_spool_failures_total 1`,
		`ttvg_egress_routes_cached_active 1`,
		`ttvg_egress_routes_cached_idle 2`,
		`ttvg_egress_leases_active 3`,
		`ttvg_egress_lane_ports_active 4`,
		`ttvg_egress_route_failures_total{reason="rate_limited"} 1`,
		`ttvg_egress_proxy_failures_total{reason="rejected"} 1`,
		`ttvg_egress_late_connections_total 1`,
		`ttvg_egress_bytes_enclave_to_upstream_total 11`,
		`ttvg_egress_bytes_upstream_to_enclave_total 13`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Prometheus() missing %q in:\n%s", want, got)
		}
	}
}
