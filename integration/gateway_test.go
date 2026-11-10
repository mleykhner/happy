package integration

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"happy/internal/gateway"
)

// A real Xray provider terminates VLESS and redirects public test destinations
// to local TCP/UDP fixtures. No public service, paid provider, TUN or root needed.
func TestWireGuardThroughProvider(t *testing.T) {
	binary := os.Getenv("HAPPY_TEST_XRAY")
	if binary == "" {
		t.Skip("set HAPPY_TEST_XRAY to run the full TCP/UDP integration test")
	}
	var providerRequests atomic.Int64
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		_, _ = io.WriteString(w, "through-provider")
	}))
	defer httpTarget.Close()
	udpTarget, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpTarget.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, a, e := udpTarget.ReadFrom(b)
			if e != nil {
				return
			}
			_, _ = udpTarget.WriteTo(b[:n], a)
		}
	}()
	tcpPort := freeTCP(t)
	wgPort := freeUDP(t)
	provider := map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": tcpPort, "protocol": "vless", "settings": map[string]any{"clients": []any{map[string]any{"id": "123e4567-e89b-12d3-a456-426614174000"}}, "decryption": "none"}}}, "outbounds": []any{map[string]any{"tag": "tcp", "protocol": "freedom", "settings": map[string]any{"redirect": strings.TrimPrefix(httpTarget.URL, "http://")}}, map[string]any{"tag": "udp", "protocol": "freedom", "settings": map[string]any{"redirect": udpTarget.LocalAddr().String()}}}, "routing": map[string]any{"rules": []any{map[string]any{"type": "field", "network": "udp", "outboundTag": "udp"}}}}
	b, _ := json.Marshal(provider)
	config := filepath.Join(t.TempDir(), "provider.json")
	if err = os.WriteFile(config, b, 0600); err != nil {
		t.Fatal(err)
	}
	upstream := exec.CommandContext(t.Context(), binary, "run", "-config", config)
	if err = upstream.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Process.Kill(); _ = upstream.Wait() })
	waitProviderListener(t, tcpPort)
	link := fmt.Sprintf("vless://123e4567-e89b-12d3-a456-426614174000@127.0.0.1:%d?security=none#Test", tcpPort)
	var body atomic.Value
	body.Store(link)
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body.Load().(string)) }))
	defer sub.Close()
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
	if err = service.Configure(t.Context(), gateway.Settings{Endpoint: "127.0.0.1", WireGuardPort: wgPort, RefreshMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	if err = service.ChangeClient(t.Context(), "create"); err != nil {
		t.Fatal(err)
	}
	clientConfig, err := service.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	wg, tnet := connect(t, clientConfig)
	defer wg.Close()
	// No subscription: no TCP traffic reaches even a valid destination.
	assertBlocked(t, tnet, "1.1.1.1:80")
	if err = service.AddSubscription(t.Context(), "Fixture", sub.URL); err != nil {
		t.Fatal(err)
	}
	subscriptions := service.View()["subscriptions"].([]any)
	id := subscriptions[0].(map[string]any)["id"].(string)
	if err = service.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	subscriptions = service.View()["subscriptions"].([]any)
	nodes := subscriptions[0].(map[string]any)["nodes"].([]any)
	nodeID := nodes[0].(map[string]any)["id"].(string)
	if err = service.Select(t.Context(), nodeID); err != nil {
		t.Fatal(err)
	}
	t.Log("checking selected VLESS outbound through the local Xray HTTP proxy")
	assertProviderReachable(t, healthProxyClient(t, dir))
	// Isolate basic forwarding from recovery of the old responder session.
	wg.Close()
	wg, tnet = connect(t, clientConfig)
	defer wg.Close()
	t.Log("checking fresh WireGuard handshake")
	assertHandshake(t, wg)
	t.Log("checking TCP through fresh WireGuard session and selected VLESS outbound")
	client := &http.Client{Transport: &http.Transport{DialContext: tnet.DialContext, DisableKeepAlives: true}, Timeout: 2 * time.Second}
	defer func() {
		if t.Failed() {
			t.Logf("local HTTP target requests: %d; WireGuard counters: %v", providerRequests.Load(), wireGuardCounters(wg))
		}
	}()
	assertProviderReachable(t, client)
	// Preserve the client here: recovery must not be hidden by reconnecting.
	t.Log("checking existing WireGuard session recovery after Xray restart")
	if err := service.Select(t.Context(), nodeID); err != nil {
		t.Fatal(err)
	}
	assertProviderReachable(t, client)

	// A DNS-shaped UDP payload proves UDP is carried through WireGuard and VLESS.
	udp, err := tnet.DialContext(t.Context(), "udp", "1.1.1.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	_ = udp.SetDeadline(time.Now().Add(8 * time.Second))
	query := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, 'a', 0, 0, 1, 0, 1}
	if _, err = udp.Write(query); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := udp.Read(buf)
	if err != nil || string(buf[:n]) != string(query) {
		t.Fatalf("UDP failed: %v", err)
	}
	assertBlocked(t, tnet, "192.168.1.1:80")
	assertBlocked(t, tnet, "[2606:4700:4700::1111]:80")
	if err = service.ChangeClient(t.Context(), "rotate"); err != nil {
		t.Fatal(err)
	}
	assertBlocked(t, tnet, "1.1.1.1:80")
	wg.Close()
	clientConfig, err = service.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	wg, tnet = connect(t, clientConfig)
	defer wg.Close()
	client.Transport = &http.Transport{DialContext: tnet.DialContext, DisableKeepAlives: true}
	t.Log("waiting for TCP with rotated client keys")
	assertProviderReachable(t, client)
	// Disappearing selected node must not leave the previous live route behind.
	body.Store(strings.Replace(link, "#Test", "#Changed", 1))
	if err = service.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	assertBlocked(t, tnet, "1.1.1.1:80")
}

// Bound startup/recovery failures while requiring an exact fixture response.
func assertProviderReachable(t *testing.T, client *http.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := waitProviderResponse(ctx, client, "http://1.1.1.1/test"); err != nil {
		t.Fatal(err)
	}
}

func waitProviderResponse(ctx context.Context, client *http.Client, target string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if readErr != nil {
				err = readErr
			} else if resp.StatusCode != http.StatusOK || string(payload) != "through-provider" {
				return fmt.Errorf("unexpected provider response: HTTP %d, body %q", resp.StatusCode, payload)
			} else {
				return nil
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("test target did not become reachable: %w", errors.Join(ctx.Err(), last))
		case <-ticker.C:
		}
	}
}

func waitProviderListener(t *testing.T, port int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	dialer := net.Dialer{Timeout: 200 * time.Millisecond}
	for {
		c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		if err == nil {
			c.Close()
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("local VLESS provider did not start: %v", err)
		case <-ticker.C:
		}
	}
}

// Never log full UAPI output: it contains private keys.
func wireGuardCounters(wg *device.Device) map[string]int64 {
	result := make(map[string]int64)
	text, err := wg.IpcGet()
	if err != nil {
		return result
	}
	for line := range strings.SplitSeq(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "last_handshake_time_sec", "tx_bytes", "rx_bytes":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				result[key] = n
			}
		}
	}
	return result
}

func assertHandshake(t *testing.T, wg *device.Device) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if wireGuardCounters(wg)["last_handshake_time_sec"] > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("WireGuard handshake did not complete; counters: %v", wireGuardCounters(wg))
		case <-ticker.C:
		}
	}
}

// Exercise the selected outbound without the WireGuard ingress.
func healthProxyClient(t *testing.T, dir string) *http.Client {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "xray.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Inbounds []struct {
			Tag      string `json:"tag"`
			Port     int    `json:"port"`
			Settings struct {
				Accounts []struct {
					User string `json:"user"`
					Pass string `json:"pass"`
				} `json:"accounts"`
			} `json:"settings"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	for _, inbound := range config.Inbounds {
		if inbound.Tag != "health" || len(inbound.Settings.Accounts) != 1 {
			continue
		}
		account := inbound.Settings.Accounts[0]
		proxy := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(inbound.Port)), User: url.UserPassword(account.User, account.Pass)}
		transport := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Timeout: 2 * time.Second}
	}
	t.Fatal("local Xray health proxy configuration missing")
	return nil
}

func connect(t *testing.T, config string) (*device.Device, *netstack.Net) {
	t.Helper()
	get := func(key string) string {
		for line := range strings.SplitSeq(config, "\n") {
			if value, ok := strings.CutPrefix(line, key+" = "); ok {
				return value
			}
		}
		t.Fatalf("missing %s", key)
		return ""
	}
	toHex := func(s string) string {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.77.0.2"), netip.MustParseAddr("fd77::2")}, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, 1280)
	if err != nil {
		t.Fatal(err)
	}
	wg := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	ipc := fmt.Sprintf("private_key=%s\npublic_key=%s\nendpoint=%s\nallowed_ip=0.0.0.0/0\nallowed_ip=::/0\npersistent_keepalive_interval=1\n", toHex(get("PrivateKey")), toHex(get("PublicKey")), get("Endpoint"))
	if err = wg.IpcSet(ipc); err != nil {
		wg.Close()
		t.Fatal(err)
	}
	if err = wg.Up(); err != nil {
		wg.Close()
		t.Fatal(err)
	}
	return wg, tnet
}
func assertBlocked(t *testing.T, n *netstack.Net, address string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	// The userspace TCP stack may acknowledge SYN before outbound routing runs;
	// only receipt of the provider marker demonstrates an actual traffic leak.
	client := &http.Client{Transport: &http.Transport{DialContext: n.DialContext, DisableKeepAlives: true}, Timeout: 1500 * time.Millisecond}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+address+"/blocked", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err == nil && strings.Contains(string(payload), "through-provider") {
		t.Fatalf("traffic unexpectedly reached %s", address)
	}
}
func freeTCP(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func freeUDP(t *testing.T) int {
	t.Helper()
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.LocalAddr().(*net.UDPAddr).Port
}
