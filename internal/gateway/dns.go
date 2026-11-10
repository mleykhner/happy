package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

type DNSStatus struct {
	Running   bool   `json:"running"`
	Route     string `json:"route"`
	Error     string `json:"error"`
	Queries   uint64 `json:"queries"`
	CacheHits uint64 `json:"cacheHits"`
}
type dnsCacheEntry struct {
	message         *dns.Msg
	stored, expires time.Time
}
type dnsRuntime struct {
	settings       DNSSettings
	mu             sync.Mutex
	primaryKey     string
	healthy        bool
	nextProbe      time.Time
	cache          map[string]dnsCacheEntry
	status         DNSStatus
	probeMu        sync.Mutex
	primaryClient  *http.Client
	fallbackClient *http.Client
}
type dnsListeners struct {
	udp     *dns.Server
	tcp     *dns.Server
	address string
}

func (l *dnsListeners) close() {
	if l == nil {
		return
	}
	_ = l.udp.PacketConn.Close()
	_ = l.tcp.Listener.Close()
	go l.udp.Shutdown()
	go l.tcp.Shutdown()
}

type DNSProxy struct {
	mu                 sync.Mutex
	current            atomic.Pointer[dnsRuntime]
	internal, external *dnsListeners
	primaryDial        dnsDial
	available          func() string
	cancel             context.CancelFunc
	slots              chan struct{}
	lastError          string
}

func newDNSProxy(dial dnsDial, available func() string) *DNSProxy {
	p := &DNSProxy{primaryDial: dial, available: available, slots: make(chan struct{}, 128)}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if r := p.current.Load(); r != nil && r.settings.Enabled {
					p.checkPrimary(ctx, r)
				}
			}
		}
	}()
	return p
}
func (p *DNSProxy) listen(address string) (*dnsListeners, error) {
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("DNS TCP: %w", err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		tcp.Close()
		return nil, fmt.Errorf("DNS UDP: %w", err)
	}
	l := &dnsListeners{address: tcp.Addr().String(), tcp: &dns.Server{Listener: tcp, Handler: p, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxTCPQueries: 100}, udp: &dns.Server{PacketConn: udp, Handler: p, UDPSize: 4096}}
	// ActivateAndServe reports readiness before returning; sockets are already bound.
	readyTCP, readyUDP := make(chan struct{}), make(chan struct{})
	l.tcp.NotifyStartedFunc = func() { close(readyTCP) }
	l.udp.NotifyStartedFunc = func() { close(readyUDP) }
	go l.tcp.ActivateAndServe()
	go l.udp.ActivateAndServe()
	<-readyTCP
	<-readyUDP
	return l, nil
}
func (p *DNSProxy) Apply(settings DNSSettings) (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer func() {
		p.lastError = ""
		if err != nil {
			p.lastError = err.Error()
		}
	}()
	if old := p.current.Load(); old != nil && reflect.DeepEqual(old.settings, settings) {
		return nil
	}
	if settings.Enabled {
		if p.internal == nil {
			l, err := p.listen("127.0.0.1:0")
			if err != nil {
				return err
			}
			p.internal = l
		}
		address := net.JoinHostPort(settings.Listen, fmt.Sprint(settings.Port))
		if p.external == nil || p.external.address != address {
			l, err := p.listen(address)
			if err != nil {
				return err
			}
			p.external.close()
			p.external = l
		}
	} else {
		p.external.close()
		p.external = nil
	}
	if old := p.current.Load(); old != nil {
		old.primaryClient.CloseIdleConnections()
		old.fallbackClient.CloseIdleConnections()
	}
	p.current.Store(&dnsRuntime{settings: settings, cache: map[string]dnsCacheEntry{}, primaryClient: newDNSHTTPClient(settings.Primary, p.primaryDial), fallbackClient: newDNSHTTPClient(settings.Fallback, hostDNSDial), status: DNSStatus{Running: settings.Enabled, Route: "blocked"}})
	return nil
}
func (p *DNSProxy) internalPort() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.internal == nil {
		return 0
	}
	_, port, _ := net.SplitHostPort(p.internal.address)
	var n int
	fmt.Sscan(port, &n)
	return n
}
func (p *DNSProxy) Close() {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.current.Load(); old != nil {
		old.primaryClient.CloseIdleConnections()
		old.fallbackClient.CloseIdleConnections()
	}
	p.current.Store(nil)
	p.external.close()
	p.internal.close()
	p.external = nil
	p.internal = nil
}
func (p *DNSProxy) Status() DNSStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.current.Load()
	if r == nil {
		return DNSStatus{Route: "disabled", Error: p.lastError}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.status
	if p.lastError != "" {
		v.Error = p.lastError
	}
	if !r.settings.Enabled {
		v.Route = "disabled"
	}
	return v
}
func (p *DNSProxy) ServeDNS(w dns.ResponseWriter, q *dns.Msg) {
	r := p.current.Load()
	rcode := dns.RcodeServerFailure
	var answer *dns.Msg
	if r == nil || !r.settings.Enabled {
		rcode = dns.RcodeRefused
	} else if !dnsClientAllowed(r.settings, w.RemoteAddr()) {
		rcode = dns.RcodeRefused
	} else if len(q.Question) != 1 || q.Opcode != dns.OpcodeQuery || q.Response || q.Question[0].Qtype == dns.TypeAXFR || q.Question[0].Qtype == dns.TypeIXFR {
		rcode = dns.RcodeRefused
	} else {
		select {
		case p.slots <- struct{}{}:
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			answer, _ = p.resolve(ctx, r, q)
			cancel()
			<-p.slots
		default:
		}
	}
	if answer == nil {
		answer = new(dns.Msg)
		answer.SetRcode(q, rcode)
	}
	if _, ok := w.RemoteAddr().(*net.UDPAddr); ok {
		size := uint16(512)
		if opt := q.IsEdns0(); opt != nil {
			size = max(512, min(4096, opt.UDPSize()))
		}
		answer.Truncate(int(size))
	}
	_ = w.WriteMsg(answer)
}
func dnsClientAllowed(s DNSSettings, remote net.Addr) bool {
	if !s.RestrictClients {
		return true
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	// Loopback also carries only DNS redirected from authenticated WireGuard peers.
	if ip.IsLoopback() {
		return true
	}
	for _, raw := range s.AllowedNetworks {
		prefix, e := netip.ParsePrefix(raw)
		if e == nil && prefix.Contains(ip) {
			return true
		}
	}
	return false
}
func (p *DNSProxy) availableKey(r *dnsRuntime) string {
	if p.current.Load() != r {
		return ""
	}
	return p.available()
}
func (p *DNSProxy) checkPrimary(ctx context.Context, r *dnsRuntime) bool {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	key := p.availableKey(r)
	r.mu.Lock()
	fresh := key != "" && r.primaryKey == key && time.Now().Before(r.nextProbe)
	healthy := r.healthy
	r.mu.Unlock()
	if fresh {
		return healthy
	}
	if ctx.Err() != nil {
		return false
	}
	if key == "" {
		r.mu.Lock()
		r.healthy = false
		r.primaryKey = ""
		r.nextProbe = time.Now().Add(5 * time.Second)
		r.status.Route = "blocked"
		r.status.Error = "Основной DNS недоступен"
		if r.settings.FailureMode == "fallback" {
			r.status.Route = "fallback"
			r.status.Error = ""
		}
		r.mu.Unlock()
		return false
	}
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	_, err := p.attempt(ctx, r.settings.Primary, q, p.primaryDial, r.primaryClient)
	stillAvailable := p.availableKey(r) == key
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthy = err == nil && stillAvailable
	r.primaryKey = key
	if r.healthy {
		r.status.Route = "primary"
		r.status.Error = ""
	} else {
		r.status.Route = "blocked"
		r.status.Error = "Основной DNS недоступен"
		if r.settings.FailureMode == "fallback" {
			r.status.Route = "fallback"
			r.status.Error = ""
		}
	}
	r.nextProbe = time.Now().Add(5 * time.Second)
	return r.healthy
}
func (p *DNSProxy) attempt(ctx context.Context, u DNSUpstream, q *dns.Msg, dial dnsDial, clients ...*http.Client) (*dns.Msg, error) {
	var err error
	for range 2 {
		child, cancel := context.WithTimeout(ctx, 2*time.Second)
		var a *dns.Msg
		var opts dnsExchangeOptions
		if len(clients) > 0 {
			opts.HTTPClient = clients[0]
		}
		a, err = exchangeDNS(child, u, q, dial, opts)
		cancel()
		if err == nil {
			return a, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, err
}
func (p *DNSProxy) resolve(ctx context.Context, r *dnsRuntime, q *dns.Msg) (*dns.Msg, error) {
	key := p.availableKey(r)
	r.mu.Lock()
	r.status.Queries++
	healthy := r.healthy && key != "" && r.primaryKey == key
	probe := key != "" && (!time.Now().Before(r.nextProbe) || r.primaryKey != key)
	r.mu.Unlock()
	if !healthy && probe {
		healthy = p.checkPrimary(ctx, r)
	}
	route := "primary"
	upstream, dial := r.settings.Primary, p.primaryDial
	if !healthy {
		route = "blocked"
		if r.settings.FailureMode == "fallback" {
			route = "fallback"
			upstream, dial = r.settings.Fallback, hostDNSDial
		}
	}
	r.mu.Lock()
	r.status.Route = route
	r.mu.Unlock()
	if route == "blocked" {
		return nil, errors.New("основной DNS недоступен")
	}
	cacheKey := dnsCacheKey(q) + "|" + route + "|" + key
	if cached := r.cached(cacheKey, q); cached != nil {
		if p.current.Load() == r && (route != "primary" || p.availableKey(r) == key) {
			return cached, nil
		}
		return nil, errors.New("выход изменён")
	}
	client := r.primaryClient
	if route == "fallback" {
		client = r.fallbackClient
	}
	a, err := p.attempt(ctx, upstream, q, dial, client)
	if err != nil && route == "primary" {
		r.mu.Lock()
		r.healthy = false
		r.nextProbe = time.Now().Add(5 * time.Second)
		r.mu.Unlock()
		if r.settings.FailureMode == "fallback" {
			route = "fallback"
			cacheKey = dnsCacheKey(q) + "|fallback|" + key
			a, err = p.attempt(ctx, r.settings.Fallback, q, hostDNSDial, r.fallbackClient)
		}
	}
	if p.current.Load() != r {
		return nil, errors.New("настройки DNS изменены")
	}
	if route == "primary" && p.availableKey(r) != key {
		return nil, errors.New("выход изменён")
	}
	r.mu.Lock()
	r.status.Route = route
	r.status.Error = ""
	if err != nil {
		r.status.Error = "DNS сервер не отвечает"
		r.status.Route = "blocked"
	}
	r.mu.Unlock()
	if err == nil {
		r.cacheAnswer(cacheKey, a)
	}
	return a, err
}
func dnsCacheKey(q *dns.Msg) string {
	c := q.Copy()
	c.Id = 0
	wire, _ := c.Pack()
	return string(wire)
}
func (r *dnsRuntime) cached(key string, q *dns.Msg) *dns.Msg {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[key]
	if !ok {
		return nil
	}
	if !time.Now().Before(entry.expires) {
		delete(r.cache, key)
		return nil
	}
	r.status.CacheHits++
	a := entry.message.Copy()
	a.Id = q.Id
	a.Question = append([]dns.Question{}, q.Question...)
	age := uint32(time.Since(entry.stored) / time.Second)
	for _, rrs := range [][]dns.RR{a.Answer, a.Ns, a.Extra} {
		for _, rr := range rrs {
			if rr.Header().Rrtype != dns.TypeOPT {
				rr.Header().Ttl -= min(age, rr.Header().Ttl)
			}
		}
	}
	return a
}
func (r *dnsRuntime) cacheAnswer(key string, a *dns.Msg) {
	if r.settings.CacheSize == 0 || a.Truncated || a.IsTsig() != nil || (a.Rcode != dns.RcodeSuccess && a.Rcode != dns.RcodeNameError) {
		return
	}
	ttl := uint32(86400)
	if len(a.Answer) == 0 {
		found := false
		for _, rr := range a.Ns {
			if soa, ok := rr.(*dns.SOA); ok {
				ttl = min(ttl, soa.Hdr.Ttl, soa.Minttl)
				found = true
			}
		}
		if !found {
			return
		}
	}
	for _, rrs := range [][]dns.RR{a.Answer, a.Ns, a.Extra} {
		for _, rr := range rrs {
			if rr.Header().Rrtype != dns.TypeOPT {
				ttl = min(ttl, rr.Header().Ttl)
			}
		}
	}
	if ttl == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= r.settings.CacheSize {
		for k, e := range r.cache {
			if time.Now().After(e.expires) {
				delete(r.cache, k)
			}
		}
		if len(r.cache) >= r.settings.CacheSize {
			for k := range r.cache {
				delete(r.cache, k)
				break
			}
		}
	}
	now := time.Now()
	r.cache[key] = dnsCacheEntry{message: a.Copy(), stored: now, expires: now.Add(time.Duration(ttl) * time.Second)}
}

// Used for the comma-separated DNS field in WireGuard configuration files.
func (s DNSSettings) wireGuardLine() string {
	value := strings.TrimSpace(s.ClientDNS)
	if s.Enabled && s.AutoWireGuard {
		value = wireGuardDNS
	}
	if value == "" {
		return ""
	}
	return "DNS = " + value + "\n"
}
