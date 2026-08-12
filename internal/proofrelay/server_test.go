package proofrelay

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/clientpolicy"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/protocol"
)

func TestRewriteEgressPortPreservesOtherFields(t *testing.T) {
	head := []byte(`{"nonce":"n","egress_port":1,"upstream":{"host":"api.openai.com","path":"/v1/responses","method":"POST"},"extra":{"x":1}}`)
	out, err := rewriteEgressPort(head, 18445)
	if err != nil {
		t.Fatalf("rewriteEgressPort() error = %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("rewritten JSON invalid: %v", err)
	}
	if obj["egress_port"].(float64) != 18445 {
		t.Fatalf("egress_port = %v", obj["egress_port"])
	}
	if obj["nonce"].(string) != "n" {
		t.Fatalf("nonce changed")
	}
	if _, ok := obj["extra"].(map[string]any); !ok {
		t.Fatalf("extra field lost: %#v", obj)
	}
}

func TestReadRequestRejectsTrailingFrameBeforeRouteAllocation(t *testing.T) {
	s := New(testConfig(), nil, nil, nil)
	var body bytes.Buffer
	writeFrame(t, &body, protocol.REQHead, []byte(`{"nonce":"n","egress_port":1,"upstream":{"host":"api.openai.com","path":"/v1/responses","method":"POST"}}`))
	writeFrame(t, &body, protocol.REQBody, []byte(`{}`))
	writeFrame(t, &body, protocol.REQBody, []byte(`extra`))
	_, err := s.readRequest(&body)
	if err == nil {
		t.Fatal("readRequest() expected error")
	}
}

func TestHandleRelayRejectsBadFrameWithoutDialingEnclave(t *testing.T) {
	s := New(testConfig(), panicDialer{}, nil, nil)
	var body bytes.Buffer
	writeFrame(t, &body, protocol.REQBody, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/proof/relay", &body)
	req.Header.Set("Content-Type", ContentTypeFrames)
	rr := httptest.NewRecorder()

	s.handleRelay(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestParseHeadRejectsNon443Target(t *testing.T) {
	_, _, _, err := parseHead([]byte(`{"upstream":{"host":"api.openai.com:8443"}}`))
	if err == nil {
		t.Fatal("parseHead() expected error")
	}
}

func TestMapPreflightErrorMapsTimeout(t *testing.T) {
	status, code := mapPreflightError(fmt.Errorf("read REQ_HEAD: %w", timeoutErr{}))
	if status != http.StatusRequestTimeout || code != "relay_timeout" {
		t.Fatalf("mapPreflightError() = %d/%s, want 408/relay_timeout", status, code)
	}
}

func TestClientPolicyMatchesCanonicalSubject(t *testing.T) {
	policies := &clientpolicy.PolicySet{Clients: []clientpolicy.ClientPolicy{{
		Subject: "O=example,CN=sub2api-prod",
	}}}
	if err := policies.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	s := New(Config{ClientPolicies: policies}, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/proof/relay", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{
		Subject: pkix.Name{
			CommonName:   "sub2api-prod",
			Organization: []string{"example"},
		},
	}}}

	id, _, err := s.clientPolicy(req)
	if err != nil {
		t.Fatalf("clientPolicy() error = %v", err)
	}
	if id != "CN=sub2api-prod,O=example" {
		t.Fatalf("identity = %q, want canonical subject", id)
	}
}

func testConfig() Config {
	return Config{
		Addr:              "127.0.0.1:0",
		VsockCID:          4,
		VsockPort:         5005,
		MaxMetadataBytes:  16 * 1024,
		MaxReqHeadBytes:   1024 * 1024,
		MaxFrameBytes:     64 * 1024 * 1024,
		MaxRequestBytes:   256 * 1024 * 1024,
		MaxBufferedBytes:  4 * 1024 * 1024,
		SpoolDir:          "",
		MaxSpoolBytes:     1024 * 1024 * 1024,
		IOTimeout:         time.Second,
		ReadHeaderTimeout: time.Second,
		ShutdownTimeout:   time.Second,
	}
}

func writeFrame(t *testing.T, b *bytes.Buffer, typ byte, payload []byte) {
	t.Helper()
	if err := protocol.WriteFrame(b, typ, payload); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}
}

type panicDialer struct{}

func (panicDialer) Dial(context.Context, uint32, uint32) (net.Conn, error) {
	panic("dialer must not be called")
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
