package egressproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Scheme       string
	Host         string
	Port         string
	Username     string
	Password     string
	RemoteDNS    bool
	RedactedKey  string
	SecretHash   string
	OriginalHost string
}

func Parse(raw string) (Config, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Config{}, nil
	}
	if strings.ContainsAny(raw, "\r\n\t") || hasControl(raw) {
		return Config{}, fmt.Errorf("proxy URL contains control characters")
	}
	if len(raw) > 8192 {
		return Config{}, fmt.Errorf("proxy URL exceeds 8192 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Config{}, err
	}
	if !u.IsAbs() {
		return Config{}, fmt.Errorf("proxy URL must be absolute")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return Config{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, fmt.Errorf("proxy URL path, query and fragment must be empty")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if host == "" || port == "" {
		return Config{}, fmt.Errorf("proxy URL must include host:port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return Config{}, fmt.Errorf("invalid proxy port: %w", err)
	}
	user := ""
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	secretHash := hashSecret(user + "\x00" + pass)
	redacted := scheme + "://" + host + ":" + port
	if user != "" || pass != "" {
		redacted = scheme + "://<redacted>@" + host + ":" + port
	}
	return Config{
		Scheme:       scheme,
		Host:         host,
		Port:         port,
		Username:     user,
		Password:     pass,
		RemoteDNS:    scheme == "socks5h",
		RedactedKey:  redacted,
		SecretHash:   secretHash,
		OriginalHost: u.Host,
	}, nil
}

func (c Config) Empty() bool {
	return c.Scheme == ""
}

func (c Config) Address() string {
	if c.Host == "" {
		return ""
	}
	return net.JoinHostPort(c.Host, c.Port)
}

type Connector struct {
	Dialer  *net.Dialer
	Timeout time.Duration
}

func (c Connector) Connect(ctx context.Context, targetHost string, targetPort uint16, proxy Config) (net.Conn, error) {
	if c.Dialer == nil {
		c.Dialer = &net.Dialer{}
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	target := net.JoinHostPort(targetHost, strconv.Itoa(int(targetPort)))
	if proxy.Empty() {
		return c.Dialer.DialContext(ctx, "tcp", target)
	}
	conn, err := c.Dialer.DialContext(ctx, "tcp", proxy.Address())
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(c.Timeout)
	_ = conn.SetDeadline(deadline)
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	if proxy.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxy.Host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = tlsConn
	}
	switch proxy.Scheme {
	case "http", "https":
		if err := httpConnect(conn, target, proxy); err != nil {
			return nil, err
		}
	case "socks5", "socks5h":
		if err := socks5Connect(ctx, conn, c.Dialer.Resolver, targetHost, targetPort, proxy); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", proxy.Scheme)
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return conn, nil
}

func httpConnect(conn net.Conn, target string, proxy Config) error {
	var req strings.Builder
	req.WriteString("CONNECT ")
	req.WriteString(target)
	req.WriteString(" HTTP/1.1\r\nHost: ")
	req.WriteString(target)
	req.WriteString("\r\n")
	if proxy.Username != "" || proxy.Password != "" {
		token := base64.StdEncoding.EncodeToString([]byte(proxy.Username + ":" + proxy.Password))
		req.WriteString("Proxy-Authorization: Basic ")
		req.WriteString(token)
		req.WriteString("\r\n")
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	if len(status) > 4096 {
		return fmt.Errorf("HTTP CONNECT status line too large")
	}
	parts := strings.Fields(status)
	if len(parts) < 2 {
		return fmt.Errorf("HTTP CONNECT malformed status line")
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("HTTP CONNECT invalid status code: %w", err)
	}
	var total int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		total += len(line)
		if total > 16*1024 {
			return fmt.Errorf("HTTP CONNECT response header too large")
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("HTTP CONNECT proxy returned %s", strings.TrimSpace(status))
	}
	if br.Buffered() != 0 {
		return fmt.Errorf("HTTP CONNECT proxy returned unexpected buffered bytes")
	}
	return nil
}

func socks5Connect(ctx context.Context, conn net.Conn, resolver *net.Resolver, targetHost string, targetPort uint16, proxy Config) error {
	methods := []byte{0x00}
	wantUserPass := proxy.Username != "" || proxy.Password != ""
	if wantUserPass {
		methods = []byte{0x02}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	var choice [2]byte
	if _, err := io.ReadFull(conn, choice[:]); err != nil {
		return err
	}
	if choice[0] != 0x05 {
		return fmt.Errorf("SOCKS5 invalid greeting version %d", choice[0])
	}
	if wantUserPass {
		if choice[1] != 0x02 {
			return fmt.Errorf("SOCKS5 proxy did not accept username/password authentication")
		}
		if err := socks5UserPass(conn, proxy.Username, proxy.Password); err != nil {
			return err
		}
	} else if choice[1] != 0x00 {
		return fmt.Errorf("SOCKS5 proxy did not accept no-auth method")
	}
	req, err := socks5Request(ctx, resolver, targetHost, targetPort, proxy.RemoteDNS)
	if err != nil {
		return err
	}
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return err
	}
	if head[0] != 0x05 {
		return fmt.Errorf("SOCKS5 invalid response version %d", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("SOCKS5 connect failed with reply code %d", head[1])
	}
	var discardLen int
	switch head[3] {
	case 0x01:
		discardLen = 4
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return err
		}
		discardLen = int(l[0])
	case 0x04:
		discardLen = 16
	default:
		return fmt.Errorf("SOCKS5 unsupported bound address type %d", head[3])
	}
	_, err = io.CopyN(io.Discard, conn, int64(discardLen+2))
	return err
}

func socks5UserPass(conn net.Conn, username, password string) error {
	if len(username) > 255 || len(password) > 255 {
		return fmt.Errorf("SOCKS5 username/password too long")
	}
	req := []byte{0x01, byte(len(username))}
	req = append(req, username...)
	req = append(req, byte(len(password)))
	req = append(req, password...)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return err
	}
	if resp[0] != 0x01 || resp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 username/password authentication failed")
	}
	return nil
}

func socks5Request(ctx context.Context, resolver *net.Resolver, targetHost string, targetPort uint16, remoteDNS bool) ([]byte, error) {
	req := []byte{0x05, 0x01, 0x00}
	if remoteDNS {
		if len(targetHost) == 0 || len(targetHost) > 255 {
			return nil, fmt.Errorf("SOCKS5 target host length out of range")
		}
		req = append(req, 0x03, byte(len(targetHost)))
		req = append(req, targetHost...)
	} else {
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		ips, err := resolver.LookupIPAddr(ctx, targetHost)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("SOCKS5 target DNS returned no addresses")
		}
		ip := ips[0].IP
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else if v6 := ip.To16(); v6 != nil {
			req = append(req, 0x04)
			req = append(req, v6...)
		} else {
			return nil, fmt.Errorf("SOCKS5 target DNS returned invalid IP")
		}
	}
	req = append(req, byte(targetPort>>8), byte(targetPort))
	return req, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
