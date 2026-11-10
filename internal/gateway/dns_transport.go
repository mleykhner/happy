package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
)

type dnsDial func(context.Context, string, string) (net.Conn, error)

// Bootstrap only upstream hostnames on the host, never client query names.
func dnsTarget(ctx context.Context, u DNSUpstream) (string, string, error) {
	host, port, err := u.endpoint()
	if err != nil {
		return "", "", err
	}
	if u.IP != "" {
		host = u.IP
	} else if net.ParseIP(host) == nil {
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil {
			return "", "", e
		}
		if len(ips) == 0 {
			return "", "", errors.New("DNS bootstrap: нет адресов")
		}
		host = ips[0].IP.String()
	}
	name, _, _ := u.endpoint()
	if u.TLSName != "" {
		name = u.TLSName
	}
	return net.JoinHostPort(host, port), name, nil
}

type dnsExchangeOptions struct {
	HTTPClient *http.Client
	RootCAs    *x509.CertPool
}

func exchangeDNS(ctx context.Context, u DNSUpstream, q *dns.Msg, dial dnsDial, options ...dnsExchangeOptions) (*dns.Msg, error) {
	var opts dnsExchangeOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if u.Protocol == "doh" {
		var client *http.Client
		if opts.HTTPClient != nil {
			client = opts.HTTPClient
		} else {
			client = newDNSHTTPClient(u, dial)
			defer client.CloseIdleConnections()
		}
		wire, e := q.Pack()
		if e != nil {
			return nil, e
		}
		req, e := http.NewRequestWithContext(ctx, "POST", u.Address, bytes.NewReader(wire))
		if e != nil {
			return nil, e
		}
		req.Header.Set("Content-Type", "application/dns-message")
		req.Header.Set("Accept", "application/dns-message")

		resp, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || strings.Split(resp.Header.Get("Content-Type"), ";")[0] != "application/dns-message" {
			return nil, errors.New("неверный ответ DoH")
		}
		wire, e = io.ReadAll(io.LimitReader(resp.Body, 65536))
		if e != nil {
			return nil, e
		}
		a := new(dns.Msg)
		if e = a.Unpack(wire); e != nil {
			return nil, e
		}
		return checkDNSReply(q, a)
	}
	target, name, err := dnsTarget(ctx, u)
	if err != nil {
		return nil, err
	}
	network := "tcp"
	if u.Protocol == "udp" {
		network = "udp"
	}
	conn, err := dial(ctx, network, target)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	if u.Protocol == "dot" {
		secure := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name, RootCAs: opts.RootCAs})
		if err = secure.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = secure
	}
	var a *dns.Msg
	if network == "udp" {
		wire, e := q.Pack()
		if e != nil {
			return nil, e
		}
		if _, e = conn.Write(wire); e != nil {
			return nil, e
		}
		buf := make([]byte, 65535)
		n, e := conn.Read(buf)
		if e != nil {
			return nil, e
		}
		a = new(dns.Msg)
		if e = a.Unpack(buf[:n]); e != nil {
			return nil, e
		}
	} else {
		client := &dns.Conn{Conn: conn}
		if err = client.WriteMsg(q); err != nil {
			return nil, err
		}
		a, err = client.ReadMsg()
		if err != nil {
			return nil, err
		}
	}
	if network == "udp" && a.Truncated {
		if _, e := checkDNSReply(q, a); e != nil {
			return nil, e
		}
		u.Protocol = "tcp"
		return exchangeDNS(ctx, u, q, dial)
	}

	return checkDNSReply(q, a)
}
func checkDNSReply(q, a *dns.Msg) (*dns.Msg, error) {
	if !a.Response || a.Id != q.Id || len(a.Question) != len(q.Question) {
		return nil, errors.New("ответ DNS не соответствует запросу")
	}
	for i, v := range q.Question {
		if !strings.EqualFold(v.Name, a.Question[i].Name) || v.Qtype != a.Question[i].Qtype || v.Qclass != a.Question[i].Qclass {
			return nil, errors.New("неверный вопрос в ответе DNS")
		}
	}
	if a.Rcode == dns.RcodeServerFailure || a.Rcode == dns.RcodeRefused {
		return nil, errors.New("DNS сервер временно недоступен")
	}
	return a, nil
}
func hostDNSDial(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func (x *Xray) dnsDial(ctx context.Context, network, address string) (net.Conn, error) {
	x.mu.Lock()
	port, secret, running := x.dnsSocksPort, x.probeSecret, x.running()
	x.mu.Unlock()
	if !running || port == 0 {
		return nil, errors.New("Xray недоступен")
	}
	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	if network == "tcp" {
		d, err := proxy.SOCKS5("tcp", endpoint, &proxy.Auth{User: "happy", Password: secret}, &net.Dialer{})
		if err != nil {
			return nil, err
		}
		return d.(proxy.ContextDialer).DialContext(ctx, "tcp", address)
	}
	return socksDNSUDP(ctx, endpoint, secret, address)
}

type socksDNSConn struct {
	net.Conn
	control net.Conn
	header  []byte
}

func (c *socksDNSConn) Close() error { return errors.Join(c.Conn.Close(), c.control.Close()) }
func (c *socksDNSConn) Write(b []byte) (int, error) {
	_, err := c.Conn.Write(append(append([]byte{}, c.header...), b...))
	if err != nil {
		return 0, err
	}
	return len(b), nil
}
func (c *socksDNSConn) Read(b []byte) (int, error) {
	buf := make([]byte, 65535)
	n, err := c.Conn.Read(buf)
	if err != nil {
		return 0, err
	}
	if n < 4 || buf[0] != 0 || buf[1] != 0 || buf[2] != 0 {
		return 0, errors.New("некорректный SOCKS UDP пакет")
	}
	offset := 4
	switch buf[3] {
	case 1:
		offset += 4
	case 4:
		offset += 16
	case 3:
		if n < 5 {
			return 0, io.ErrUnexpectedEOF
		}
		offset += 1 + int(buf[4])
	default:
		return 0, errors.New("неизвестный SOCKS адрес")
	}
	offset += 2
	if n < offset {
		return 0, io.ErrUnexpectedEOF
	}
	return copy(b, buf[offset:n]), nil
}
func socksDNSUDP(ctx context.Context, endpoint, secret, address string) (net.Conn, error) {
	control, err := hostDNSDial(ctx, "tcp", endpoint)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			control.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	_ = control.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { control.Close() })
	defer stop()
	if _, err = control.Write([]byte{5, 1, 2}); err != nil {
		return nil, err
	}
	response := make([]byte, 2)
	if _, err = io.ReadFull(control, response); err != nil {
		return nil, err
	}
	if response[0] != 5 || response[1] != 2 {
		return nil, errors.New("SOCKS auth не поддерживается")
	}
	auth := append([]byte{1, 5}, []byte("happy")...)
	auth = append(auth, byte(len(secret)))
	auth = append(auth, secret...)
	if _, err = control.Write(auth); err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(control, response); err != nil {
		return nil, err
	}
	if response[1] != 0 {
		return nil, errors.New("SOCKS auth отклонён")
	}
	if _, err = control.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return nil, err
	}
	h := make([]byte, 4)
	if _, err = io.ReadFull(control, h); err != nil {
		return nil, err
	}
	if h[0] != 5 || h[1] != 0 {
		return nil, errors.New("SOCKS UDP недоступен")
	}
	size := 4
	if h[3] == 4 {
		size = 16
	} else if h[3] != 1 {
		return nil, errors.New("неверный адрес SOCKS relay")
	}
	relay := make([]byte, size+2)
	if _, err = io.ReadFull(control, relay); err != nil {
		return nil, err
	}
	ip := net.IP(relay[:size])
	if ip.IsUnspecified() {
		ip = net.ParseIP("127.0.0.1")
	}
	relayAddress := net.JoinHostPort(ip.String(), strconv.Itoa(int(binary.BigEndian.Uint16(relay[size:]))))
	conn, err := hostDNSDial(ctx, "udp", relayAddress)
	if err != nil {
		return nil, err
	}
	host, p, _ := net.SplitHostPort(address)
	targetIP := net.ParseIP(host)
	header := []byte{0, 0, 0, 1}
	if v4 := targetIP.To4(); v4 != nil {
		header = append(header, v4...)
	} else {
		header[3] = 4
		header = append(header, targetIP.To16()...)
	}
	port, _ := strconv.Atoi(p)
	header = binary.BigEndian.AppendUint16(header, uint16(port))
	success = true
	return &socksDNSConn{Conn: conn, control: control, header: header}, nil
}

func newDNSHTTPClient(u DNSUpstream, dial dnsDial) *http.Client {
	name, _, _ := u.endpoint()
	if u.TLSName != "" {
		name = u.TLSName
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		target, _, err := dnsTarget(ctx, u)
		if err != nil {
			return nil, err
		}
		return dial(ctx, "tcp", target)
	}, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name}, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 2 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 8, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 32}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
