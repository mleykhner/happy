package integration

import (
	"context"
	"encoding/json"
	"fmt"
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
	"happy/internal/gateway"
)

func TestWireGuardDNSProxyThroughProvider(t *testing.T) {
	binary := os.Getenv("HAPPY_TEST_XRAY")
	if binary == "" {
		t.Skip("HAPPY_TEST_XRAY is not set")
	}
	var providerQueries, fallbackQueries atomic.Int32
	fixture := func(counter *atomic.Int32) string {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		udp, err := net.ListenPacket("udp", tcp.Addr().String())
		if err != nil {
			tcp.Close()
			t.Fatal(err)
		}
		handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			counter.Add(1)
			a := new(dns.Msg)
			a.SetReply(q)
			a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.123")}}
			w.WriteMsg(a)
		})
		for _, server := range []*dns.Server{{Listener: tcp, Handler: handler}, {PacketConn: udp, Handler: handler}} {
			ready := make(chan struct{})
			server.NotifyStartedFunc = func() { close(ready) }
			go server.ActivateAndServe()
			<-ready
			t.Cleanup(func() { server.Shutdown() })
		}
		return tcp.Addr().String()
	}
	primary, fallback := fixture(&providerQueries), fixture(&fallbackQueries)
	port := freeTCP(t)
	provider := map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]any{"clients": []any{map[string]any{"id": "123e4567-e89b-12d3-a456-426614174000"}}, "decryption": "none"}}}, "outbounds": []any{fixtureFreedom(t, "tcp", primary), fixtureFreedom(t, "udp", primary)}, "routing": map[string]any{"rules": []any{map[string]any{"type": "field", "network": "udp", "outboundTag": "udp"}}}}
	body, _ := json.Marshal(provider)
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	process := exec.CommandContext(t.Context(), binary, "run", "-config", path)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.Process.Kill(); process.Wait() })
	waitProviderListener(t, port)
	link := fmt.Sprintf("vless://123e4567-e89b-12d3-a456-426614174000@127.0.0.1:%d?security=none#DNS", port)
	subscription := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, link) }))
	defer subscription.Close()
	dir := t.TempDir()
	store, err := gateway.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	xray := gateway.NewXray(binary, dir)
	defer xray.Stop()
	service, err := gateway.NewService(store, xray)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	settings := service.View()["settings"].(gateway.Settings)
	settings.Endpoint = "127.0.0.1"
	settings.WireGuardPort = freeUDP(t)
	settings.DNS.Enabled = true
	settings.DNS.Listen = "127.0.0.1"
	settings.DNS.Port = freeTCP(t)
	settings.DNS.Primary = gateway.DNSUpstream{Protocol: "udp", Address: "1.1.1.1:53"}
	settings.DNS.Fallback = gateway.DNSUpstream{Protocol: "tcp", Address: fallback}
	if err = service.Configure(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if err = service.AddSubscription(t.Context(), "DNS fixture", subscription.URL); err != nil {
		t.Fatal(err)
	}
	id := service.View()["subscriptions"].([]any)[0].(map[string]any)["id"].(string)
	if err = service.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	node := service.View()["subscriptions"].([]any)[0].(map[string]any)["nodes"].([]any)[0].(map[string]any)["id"].(string)
	if err = service.Select(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	if err = service.ChangeClient(t.Context(), "create"); err != nil {
		t.Fatal(err)
	}
	config, err := service.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config, "DNS = 10.77.0.1") {
		t.Fatal("WG DNS config missing")
	}
	wg, tnet := connect(t, config)
	defer wg.Close()
	query := func(network string) *dns.Msg {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		var last error
		for ctx.Err() == nil {
			c, e := tnet.DialContext(ctx, network, "10.77.0.1:53")
			if e != nil {
				last = e
				continue
			}
			c.SetDeadline(time.Now().Add(time.Second))
			q := new(dns.Msg)
			q.SetQuestion("music.example.", dns.TypeA)
			var a *dns.Msg
			if network == "udp" {
				wire, _ := q.Pack()
				_, e = c.Write(wire)
				if e == nil {
					buf := make([]byte, 4096)
					var n int
					n, e = c.Read(buf)
					if e == nil {
						a = new(dns.Msg)
						e = a.Unpack(buf[:n])
					}
				}
			} else {
				dc := &dns.Conn{Conn: c}
				e = dc.WriteMsg(q)
				if e == nil {
					a, e = dc.ReadMsg()
				}
			}
			c.Close()
			if e == nil {
				return a
			}
			last = e
		}
		t.Fatalf("DNS through WireGuard %s: %v", network, last)
		return nil
	}
	for _, protocol := range []string{"udp", "tcp"} {
		settings.DNS.Primary.Protocol = protocol
		if err = service.Configure(t.Context(), settings); err != nil {
			t.Fatal(err)
		}
		for _, network := range []string{"udp", "tcp"} {
			a := query(network)
			if len(a.Answer) != 1 {
				t.Fatalf("missing DNS answer, primary %s ingress %s: %s", protocol, network, a)
			}
		}
	}
	external := net.JoinHostPort("127.0.0.1", strconv.Itoa(settings.DNS.Port))
	q := new(dns.Msg)
	q.SetQuestion("external.example.", dns.TypeA)
	a, _, err := (&dns.Client{}).ExchangeContext(t.Context(), q, external)
	if err != nil || len(a.Answer) != 1 {
		t.Fatalf("external DNS failed: %v", err)
	}
	if providerQueries.Load() == 0 || fallbackQueries.Load() != 0 {
		t.Fatal("primary did not stay through provider")
	}
	if err = service.SetConnection(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	wg.Close()
	wg, tnet = connect(t, config)
	defer wg.Close()
	if a = query("udp"); a.Rcode != dns.RcodeServerFailure {
		t.Fatal("DNS/cache answered while VPN blocked")
	}
	if fallbackQueries.Load() != 0 {
		t.Fatal("block mode leaked")
	}
	settings.DNS.FailureMode = "fallback"
	if err = service.Configure(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if a = query("tcp"); len(a.Answer) != 1 || fallbackQueries.Load() == 0 {
		t.Fatal("fallback through host failed")
	}
	if err = service.SetConnection(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	wg.Close()
	wg, tnet = connect(t, config)
	defer wg.Close()
	if a = query("udp"); len(a.Answer) != 1 {
		t.Fatal("primary did not recover")
	}
}
