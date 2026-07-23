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
