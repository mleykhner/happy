package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func dnsAnswer(q *dns.Msg) *dns.Msg {
	a := new(dns.Msg)
	a.SetReply(q)
	a.RecursionAvailable = true
	a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.123")}}
	return a
}
func dnsFixture(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		tcp.Close()
		t.Fatal(err)
	}
	for _, s := range []*dns.Server{{Listener: tcp, Handler: handler}, {PacketConn: udp, Handler: handler}} {
		ready := make(chan struct{})
		s.NotifyStartedFunc = func() { close(ready) }
		go s.ActivateAndServe()
		<-ready
		t.Cleanup(func() { s.Shutdown() })
	}
	return tcp.Addr().String()
}
func dnsQuery(t *testing.T, address, network string) *dns.Msg {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("music.example.", dns.TypeA)
	a, _, err := (&dns.Client{Net: network, Timeout: 8 * time.Second}).ExchangeContext(t.Context(), q, address)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestDNSUDPTruncationRetriesTCP(t *testing.T) {
	var udp, tcp atomic.Int32
	address := dnsFixture(t, func(w dns.ResponseWriter, q *dns.Msg) {
		a := dnsAnswer(q)
		if _, ok := w.RemoteAddr().(*net.UDPAddr); ok {
			udp.Add(1)
			a.Answer = nil
			a.Truncated = true
		} else {
			tcp.Add(1)
		}
		w.WriteMsg(a)
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	a, err := exchangeDNS(ctx, DNSUpstream{Protocol: "udp", Address: address}, q, hostDNSDial)
	if err != nil || len(a.Answer) != 1 || udp.Load() != 1 || tcp.Load() != 1 {
		t.Fatalf("UDP/TCP fallback failed: %v", err)
	}
}
func TestDNSProxyFailoverCacheAndRecovery(t *testing.T) {
	var online atomic.Bool
	online.Store(true)
	var primaryCalls, fallbackCalls atomic.Int32
	primary := dnsFixture(t, func(w dns.ResponseWriter, q *dns.Msg) { primaryCalls.Add(1); w.WriteMsg(dnsAnswer(q)) })
	fallback := dnsFixture(t, func(w dns.ResponseWriter, q *dns.Msg) { fallbackCalls.Add(1); w.WriteMsg(dnsAnswer(q)) })
	p := newDNSProxy(hostDNSDial, func() string {
		if online.Load() {
			return "generation1"
		}
		return ""
	})
	t.Cleanup(p.Close)
	settings := defaultDNSSettings()
	settings.Enabled = true
	settings.Listen = "127.0.0.1"
	settings.Port = freeDNSPort(t)
	settings.Primary = DNSUpstream{Protocol: "udp", Address: primary}
	settings.Fallback = DNSUpstream{Protocol: "tcp", Address: fallback}
	if err := p.Apply(settings); err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort(settings.Listen, strconv.Itoa(settings.Port))
	for _, network := range []string{"udp", "tcp"} {
		a := dnsQuery(t, address, network)
		if len(a.Answer) != 1 {
			t.Fatal("missing primary answer")
		}
	}
	if p.Status().CacheHits != 1 || fallbackCalls.Load() != 0 {
		t.Fatal("cache or primary isolation failed")
	}
	online.Store(false)
	if a := dnsQuery(t, address, "udp"); a.Rcode != dns.RcodeServerFailure {
		t.Fatal("cache answered while disconnected")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatal("block mode leaked to fallback")
	}
	settings.FailureMode = "fallback"
	if err := p.Apply(settings); err != nil {
		t.Fatal(err)
	}
	if a := dnsQuery(t, address, "tcp"); len(a.Answer) != 1 || p.Status().Route != "fallback" {
		t.Fatal("fallback unavailable")
	}
	online.Store(true)
	if a := dnsQuery(t, address, "udp"); len(a.Answer) != 1 || p.Status().Route != "primary" {
		t.Fatal("did not recover primary")
	}
	if fallbackCalls.Load() != 1 {
		t.Fatal("unexpected direct queries")
	}
}
func TestDNSPrimaryFailureUsesFallbackAndRecovers(t *testing.T) {
	var failed atomic.Bool
	primary := dnsFixture(t, func(w dns.ResponseWriter, q *dns.Msg) {
		a := dnsAnswer(q)
		if failed.Load() {
			a.Answer = nil
			a.Rcode = dns.RcodeServerFailure
		}
		w.WriteMsg(a)
	})
	fallback := dnsFixture(t, func(w dns.ResponseWriter, q *dns.Msg) { w.WriteMsg(dnsAnswer(q)) })
	p := newDNSProxy(hostDNSDial, func() string { return "live" })
	t.Cleanup(p.Close)
	settings := defaultDNSSettings()
	settings.Enabled = true
	settings.Listen = "127.0.0.1"
	settings.Port = freeDNSPort(t)
	settings.CacheSize = 0
	settings.FailureMode = "fallback"
	settings.Primary = DNSUpstream{Protocol: "tcp", Address: primary}
	settings.Fallback = DNSUpstream{Protocol: "udp", Address: fallback}
	if err := p.Apply(settings); err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort(settings.Listen, strconv.Itoa(settings.Port))
	dnsQuery(t, address, "udp")
	failed.Store(true)
	dnsQuery(t, address, "udp")
	if p.Status().Route != "fallback" {
		t.Fatal("failed primary did not trigger fallback")
	}
	failed.Store(false)
	r := p.current.Load()
	r.mu.Lock()
	r.nextProbe = time.Time{}
	r.mu.Unlock()
	if !p.checkPrimary(t.Context(), p.current.Load()) {
		t.Fatal("primary health did not recover")
	}
	dnsQuery(t, address, "udp")
	if p.Status().Route != "primary" {
		t.Fatal("primary route not restored")
	}
}
func freeDNSPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func TestDNSCacheTTLAndSeparation(t *testing.T) {
	r := &dnsRuntime{settings: DNSSettings{CacheSize: 2}, cache: map[string]dnsCacheEntry{}}
	q := new(dns.Msg)
	q.SetQuestion("music.example.", dns.TypeA)
	a := dnsAnswer(q)
	key := dnsCacheKey(q)
	r.cacheAnswer(key, a)
	entry := r.cache[key]
	entry.stored = time.Now().Add(-2 * time.Second)
	r.cache[key] = entry
	q.Id++
	cached := r.cached(key, q)
	if cached == nil || cached.Id != q.Id || cached.Answer[0].Header().Ttl > 58 || a.Answer[0].Header().Ttl != 60 {
		t.Fatal("TTL or original message mutated")
	}
	entry.expires = time.Now().Add(-time.Second)
	r.cache[key] = entry
	if r.cached(key, q) != nil {
		t.Fatal("stale cache returned")
	}
	do := q.Copy()
	do.SetEdns0(1232, true)
	if dnsCacheKey(q) == dnsCacheKey(do) {
		t.Fatal("DNSSEC variants share cache")
	}
}
func TestDNSAccessAndConfiguration(t *testing.T) {
	settings := defaultDNSSettings()
	settings.RestrictClients = true
	settings.AllowedNetworks = []string{"192.168.1.0/24"}
	if !dnsClientAllowed(settings, &net.UDPAddr{IP: net.ParseIP("192.168.1.3"), Port: 123}) || dnsClientAllowed(settings, &net.TCPAddr{IP: net.ParseIP("203.0.113.1"), Port: 123}) {
		t.Fatal("ACL failed")
	}
	settings.RestrictClients = false
	if !dnsClientAllowed(settings, &net.UDPAddr{IP: net.ParseIP("203.0.113.1"), Port: 123}) {
		t.Fatal("default open access failed")
	}
	for _, u := range []DNSUpstream{{Protocol: "doh", Address: "http://dns.example/query"}, {Protocol: "dot", Address: "dns.example", IP: "not-ip"}, {Protocol: "udp", Address: "1.1.1.1:0"}} {
		if u.validate() == nil {
			t.Fatalf("invalid upstream accepted: %+v", u)
		}
	}
	s := selectedState(t)
	s.Settings.Endpoint = "vpn.example"
	s.Settings.DNS.Enabled = true
	config, err := s.clientConfig()
	if err != nil || !strings.Contains(config, "DNS = 10.77.0.1") {
		t.Fatal("automatic WireGuard DNS missing")
	}
	s.Settings.DNS.AutoWireGuard = false
	s.Settings.DNS.ClientDNS = ""
	config, _ = s.clientConfig()
	if strings.Contains(config, "DNS =") {
		t.Fatal("empty custom DNS was ignored")
	}
	s.Settings.DNS.ClientDNS = "9.9.9.9"
	config, _ = s.clientConfig()
	if !strings.Contains(config, "DNS = 9.9.9.9") {
		t.Fatal("custom DNS missing")
	}
}
func TestDNSDoHManualIPTLSAndReuse(t *testing.T) {
	var connections atomic.Int32
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "example.com:") || r.URL.Path != "/dns-query" {
			t.Error("DoH Host/path lost")
		}
		wire, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if err := q.Unpack(wire); err != nil {
			t.Error(err)
			return
		}
		wire, _ = dnsAnswer(q).Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(wire)
	}))
	target.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	target.StartTLS()
	defer target.Close()
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	u := DNSUpstream{Protocol: "doh", Address: "https://example.com:" + port + "/dns-query", IP: "127.0.0.1", TLSName: "example.com"}
	// httptest's certificate covers example.com; add its issuing cert to this test client only.
	client := newDNSHTTPClient(u, hostDNSDial)
	defer client.CloseIdleConnections()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
	q := new(dns.Msg)
	q.SetQuestion("music.example.", dns.TypeA)
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		a, err := exchangeDNS(ctx, u, q, hostDNSDial, dnsExchangeOptions{HTTPClient: client})
		cancel()
		if err != nil || len(a.Answer) != 1 {
			t.Fatalf("DoH failed: %v", err)
		}
	}
	if connections.Load() != 1 {
		t.Fatal("DoH did not reuse HTTPS connection")
	}
	client.CloseIdleConnections()
	client.Transport.(*http.Transport).TLSClientConfig.ServerName = "wrong.example"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := exchangeDNS(ctx, u, q, hostDNSDial, dnsExchangeOptions{HTTPClient: client}); err == nil {
		t.Fatal("wrong TLS name accepted")
	}
}
func TestDNSBindConflictPreservesSettings(t *testing.T) {
	s, _ := fixture(t)
	settings := s.state.Settings
	settings.DNS.Enabled = true
	settings.DNS.Listen = "127.0.0.1"
	settings.DNS.Port = freeDNSPort(t)
	occupied, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(settings.DNS.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if err = s.Configure(t.Context(), settings); err == nil {
		t.Fatal("occupied UDP port accepted")
	}
	if s.state.Settings.DNS.Enabled {
		t.Fatal("failed DNS config persisted")
	}
}
func TestRealXrayDNSUDPAndTCP(t *testing.T) {
	binary := os.Getenv("HAPPY_TEST_XRAY")
	if binary == "" {
		t.Skip("HAPPY_TEST_XRAY is not set")
	}
	upstream := dnsFixture(t, func(w dns.ResponseWriter, q *dns.Msg) { w.WriteMsg(dnsAnswer(q)) })
	x := NewXray(binary, t.TempDir())
	defer x.Stop()
	p := newDNSProxy(x.dnsDial, x.dnsKey)
	defer p.Close()
	s := selectedState(t)
	s.Settings.DNS.Enabled = true
	s.Settings.DNS.Listen = "127.0.0.1"
	s.Settings.DNS.Port = freeDNSPort(t)
	s.Settings.DNS.Primary = DNSUpstream{Protocol: "udp", Address: upstream}
	s.Settings.DNS.CacheSize = 0
	s.Subscriptions[0].Nodes[0].Outbound = map[string]any{"tag": "proxy", "protocol": "freedom", "settings": map[string]any{}}
	if err := p.Apply(s.Settings.DNS); err != nil {
		t.Fatal(err)
	}
	x.setDNSForwardPort(p.internalPort())
	if err := x.Apply(t.Context(), s, false); err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"udp", "tcp"} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		u := s.Settings.DNS.Primary
		u.Protocol = network
		q := new(dns.Msg)
		q.SetQuestion("music.example.", dns.TypeA)
		a, err := exchangeDNS(ctx, u, q, x.dnsDial)
		cancel()
		if err != nil || len(a.Answer) != 1 {
			t.Fatalf("DNS through actual Xray %s: %v", network, err)
		}
	}
	generation := x.dnsKey()
	s.Settings.DNS.CacheSize = 20
	if err := x.Apply(t.Context(), s, false); err != nil {
		t.Fatal(err)
	}
	if x.dnsKey() != generation {
		t.Fatal("DNS-only update restarted Xray")
	}
	// The generated config must also pass the real core's validator.
	config, err := BuildConfig(s, freeDNSPort(t), "dns-test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, config, 0600)
	if out, err := exec.CommandContext(t.Context(), binary, "run", "-test", "-config", path).CombinedOutput(); err != nil {
		t.Fatalf("DNS config rejected: %v %s", err, out)
	}
}
func TestDNSRejectsMismatchedReply(t *testing.T) {
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	a := dnsAnswer(q)
	a.Id++
	if _, err := checkDNSReply(q, a); err == nil {
		t.Fatal("mismatched ID accepted")
	}
}

func TestDNSDoTTrusted(t *testing.T) {
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	certificates := certServer.TLS.Certificates
	cert := certServer.Certificate()
	certServer.Close()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(tcp, &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
	server := &dns.Server{Listener: listener, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) { w.WriteMsg(dnsAnswer(q)) })}
	ready := make(chan struct{})
	server.NotifyStartedFunc = func() { close(ready) }
	go server.ActivateAndServe()
	<-ready
	defer server.Shutdown()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	u := DNSUpstream{Protocol: "dot", Address: tcp.Addr().String(), TLSName: "example.com"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	a, err := exchangeDNS(ctx, u, q, hostDNSDial, dnsExchangeOptions{RootCAs: roots})
	if err != nil || len(a.Answer) != 1 {
		t.Fatalf("trusted DoT: %v", err)
	}
	u.TLSName = "wrong.example"
	if _, err = exchangeDNS(ctx, u, q, hostDNSDial, dnsExchangeOptions{RootCAs: roots}); err == nil {
		t.Fatal("wrong DoT name accepted")
	}
}

func TestDNSWildcardSettingsUpdateKeepsListener(t *testing.T) {
	p := newDNSProxy(hostDNSDial, func() string { return "" })
	defer p.Close()
	settings := defaultDNSSettings()
	settings.Enabled = true
	settings.Listen = "0.0.0.0"
	settings.Port = freeDNSPort(t)
	if err := p.Apply(settings); err != nil {
		t.Fatal(err)
	}
	listener := p.external
	settings.CacheSize++
	if err := p.Apply(settings); err != nil {
		t.Fatalf("saving DNS settings on the existing wildcard port: %v", err)
	}
	if p.external != listener {
		t.Fatal("upstream/cache update replaced listener")
	}
}
func TestDNSRebindSamePort(t *testing.T) {
	p := newDNSProxy(hostDNSDial, func() string { return "" })
	defer p.Close()
	settings := defaultDNSSettings()
	settings.Enabled = true
	settings.Listen = "127.0.0.1"
	settings.Port = freeDNSPort(t)
	if err := p.Apply(settings); err != nil {
		t.Fatal(err)
	}
	settings.Listen = "0.0.0.0"
	if err := p.Apply(settings); err != nil {
		t.Fatalf("changing bind address on the same port: %v", err)
	}
	settings.Listen = "127.0.0.1"
	if err := p.Apply(settings); err != nil {
		t.Fatal(err)
	}
	if a := dnsQuery(t, net.JoinHostPort(settings.Listen, strconv.Itoa(settings.Port)), "tcp"); a.Rcode != dns.RcodeServerFailure {
		t.Fatal("listener stopped answering")
	}
}

func TestDNSPortEnvironmentOverridesSavedAndUISettings(t *testing.T) {
	port := freeDNSPort(t)
	t.Setenv("HAPPY_DNS_PORT", strconv.Itoa(port))
	s, _ := fixture(t)
	if s.state.Settings.DNS.Port != port {
		t.Fatalf("service ignores configured DNS port: got %d, want %d", s.state.Settings.DNS.Port, port)
	}
	settings := s.state.Settings
	settings.DNS.Port = port + 1
	settings.DNS.Enabled = true
	settings.DNS.Listen = "127.0.0.1"
	if err := s.Configure(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if s.state.Settings.DNS.Port != port {
		t.Fatal("UI overwrote managed DNS port")
	}
	if a := dnsQuery(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "tcp"); a.Rcode != dns.RcodeServerFailure {
		t.Fatal("service did not listen on configured port")
	}
	saved, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Settings.DNS.Port != port {
		t.Fatal("managed DNS port was not persisted")
	}
	if s.View()["managedDnsPort"] != port {
		t.Fatal("UI was not told that the port is managed")
	}
	newPort := freeDNSPort(t)
	t.Setenv("HAPPY_DNS_PORT", strconv.Itoa(newPort))
	reloaded, err := NewService(s.store, s.xray)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if reloaded.state.Settings.DNS.Port != newPort {
		t.Fatal("saved port took precedence over updated service configuration")
	}
}

func TestDNSPortConflictKeepsOldListener(t *testing.T) {
	p := newDNSProxy(hostDNSDial, func() string { return "" })
	defer p.Close()
	settings := defaultDNSSettings()
	settings.Enabled = true
	settings.Listen = "127.0.0.1"
	settings.Port = freeDNSPort(t)
	if err := p.Apply(settings); err != nil {
		t.Fatal(err)
	}
	listener := p.external
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	changed := settings
	changed.Port = occupied.Addr().(*net.TCPAddr).Port
	if err = p.Apply(changed); err == nil {
		t.Fatal("another process's port was accepted")
	}
	if p.external != listener {
		t.Fatal("existing listener lost after failed update")
	}
	if a := dnsQuery(t, net.JoinHostPort(settings.Listen, strconv.Itoa(settings.Port)), "udp"); a.Rcode != dns.RcodeServerFailure {
		t.Fatal("previous DNS stopped serving")
	}
}
func TestDNSInvalidPortEnvironment(t *testing.T) {
	s, _ := fixture(t)
	for _, raw := range []string{"0", "65536", "not-a-port"} {
		t.Setenv("HAPPY_DNS_PORT", raw)
		if v, err := NewService(s.store, s.xray); err == nil {
			v.Close()
			t.Fatalf("invalid port accepted: %s", raw)
		}
	}
}
