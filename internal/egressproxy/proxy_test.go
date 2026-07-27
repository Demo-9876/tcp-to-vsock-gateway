package egressproxy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestParseProxyURLNormalizesAndRedacts(t *testing.T) {
	cfg, err := Parse("socks5h://user:p%40ss@Proxy.Example:1080")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Scheme != "socks5h" || cfg.Host != "proxy.example" || cfg.Port != "1080" {
		t.Fatalf("normalized proxy = %#v", cfg)
	}
	if cfg.Username != "user" || cfg.Password != "p@ss" {
		t.Fatalf("credentials = %q/%q", cfg.Username, cfg.Password)
	}
	if cfg.RedactedKey != "socks5h://<redacted>@proxy.example:1080" {
		t.Fatalf("RedactedKey = %q", cfg.RedactedKey)
	}
}

func TestParseProxyURLRejectsPathAndMissingPort(t *testing.T) {
	for _, raw := range []string{
		"http://proxy.example",
		"http://proxy.example:8080/path",
		"http://proxy.example:8080?x=1",
		"ftp://proxy.example:21",
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) expected error", raw)
		}
	}
}

func TestParseProxyURLAllowsTrailingSlash(t *testing.T) {
	cfg, err := Parse("http://Proxy.Example:8080/")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Address() != "proxy.example:8080" {
		t.Fatalf("Address() = %q", cfg.Address())
	}
}

func TestHTTPConnectSendsBasicAuth(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := server.Read(buf)
		done <- string(buf[:n])
		_, _ = server.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	}()

	err := httpConnect(client, "api.openai.com:443", Config{
		Username: "user",
		Password: "pass",
	})
	if err != nil {
		t.Fatalf("httpConnect() error = %v", err)
	}
	req := <-done
	if !contains(req, "CONNECT api.openai.com:443 HTTP/1.1") {
		t.Fatalf("CONNECT request missing target: %q", req)
	}
	if !contains(req, "Proxy-Authorization: Basic dXNlcjpwYXNz") {
		t.Fatalf("CONNECT request missing proxy auth: %q", req)
	}
}

func TestSocks5UserPassDomainConnect(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan []byte, 1)
	go func() {
		var out []byte
		buf := make([]byte, 3)
		_, _ = io.ReadFull(server, buf)
		out = append(out, buf...)
		_, _ = server.Write([]byte{0x05, 0x02})
		authHead := make([]byte, 2)
		_, _ = io.ReadFull(server, authHead)
		authRest := make([]byte, int(authHead[1])+1)
		_, _ = io.ReadFull(server, authRest)
		authPass := make([]byte, int(authRest[len(authRest)-1]))
		_, _ = io.ReadFull(server, authPass)
		out = append(out, authHead...)
		out = append(out, authRest...)
		out = append(out, authPass...)
		_, _ = server.Write([]byte{0x01, 0x00})
		req := make([]byte, 5+len("api.openai.com")+2)
		_, _ = io.ReadFull(server, req)
		out = append(out, req...)
		_, _ = server.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0})
		done <- out
	}()

	err := socks5Connect(context.Background(), client, nil, "api.openai.com", 443, Config{
		Scheme:    "socks5h",
		Username:  "u",
		Password:  "p",
		RemoteDNS: true,
	})
	if err != nil {
		t.Fatalf("socks5Connect() error = %v", err)
	}
	got := <-done
	wantPrefix := []byte{0x05, 0x01, 0x02, 0x01, 0x01, 'u', 0x01, 'p', 0x05, 0x01, 0x00, 0x03, byte(len("api.openai.com"))}
	if string(got[:len(wantPrefix)]) != string(wantPrefix) {
		t.Fatalf("SOCKS5 bytes prefix = %v, want %v", got[:len(wantPrefix)], wantPrefix)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && index(s, sub) >= 0)
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestConnectorDirectUsesTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		c, _ := ln.Accept()
		if c != nil {
			_ = c.Close()
		}
		close(done)
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	pn, _ := net.LookupPort("tcp", port)
	conn, err := Connector{Timeout: time.Second}.Connect(context.Background(), host, uint16(pn), Config{})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	_ = conn.Close()
	<-done
}
