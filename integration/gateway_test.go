package integration

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
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
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "through-provider") }))
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
	time.Sleep(400 * time.Millisecond)
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
	client := &http.Client{Transport: &http.Transport{DialContext: tnet.DialContext, DisableKeepAlives: true}, Timeout: 8 * time.Second}
	response, err := client.Get("http://1.1.1.1/test")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(payload) != "through-provider" {
		t.Fatalf("unexpected TCP result: %q %v", payload, err)
	}
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
	response, err = client.Get("http://1.1.1.1/test")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// Disappearing selected node must not leave the previous live route behind.
	body.Store(strings.Replace(link, "#Test", "#Changed", 1))
	if err = service.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	assertBlocked(t, tnet, "1.1.1.1:80")
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
