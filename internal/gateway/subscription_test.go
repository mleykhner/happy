package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const sampleVLESS = "vless://123e4567-e89b-12d3-a456-426614174000@vpn.example:443?security=tls&sni=vpn.example#Test"

func TestFetchSubscriptionDiagnostics(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		headers  http.Header
		redirect bool
		err      error
		want     []string
		wantErr  bool
	}{
		{name: "forbidden", status: 403, body: "BODY_SECRET", want: []string{"status=403", `stage="response"`, "subscription.fetch failed"}, wantErr: true},
		{name: "hwid refusal", status: 403, headers: http.Header{"X-Hwid-Max-Devices-Reached": {"true"}}, want: []string{"status=403", "hwid_max_devices_reached=true", "достигнут лимит устройств"}, wantErr: true},
		{name: "redirected forbidden", status: 403, redirect: true, want: []string{`host="edge.example"`, "redirected=true", "status=403"}, wantErr: true},
		{name: "success", status: 200, body: sampleVLESS, want: []string{"subscription.fetch completed", "nodes=1"}},
		{name: "parse failure", status: 200, body: "<html>BODY_SECRET</html>", want: []string{`stage="parse"`, "subscription.fetch failed"}, wantErr: true},
		{name: "network failure", err: errors.New("TRANSPORT_SECRET"), want: []string{"subscription.fetch request_error", `stage="request"`}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			t.Cleanup(func() { log.SetOutput(previous) })
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				if tc.redirect && r.URL.Host == "provider.example" {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://edge.example/REDIRECT_SECRET?token=QUERY_SECRET"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				headers := tc.headers.Clone()
				if headers == nil {
					headers = make(http.Header)
				}
				headers.Set("Set-Cookie", "session=COOKIE_SECRET")
				headers.Set("Server", "test-provider")
				return &http.Response{StatusCode: tc.status, Header: headers, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})}
			_, _, err := fetchSubscription(t.Context(), client, "https://provider.example/PATH_SECRET?token=QUERY_SECRET", "HWID_SECRET")
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected result: %v", err)
			}
			logs := output.String()
			for _, want := range append([]string{"subscription.fetch started", "duration_ms=", "hwid_present=true"}, tc.want...) {
				if !strings.Contains(logs, want) {
					t.Errorf("missing diagnostic %q in %s", want, logs)
				}
			}
			for _, secret := range []string{"PATH_SECRET", "QUERY_SECRET", "HWID_SECRET", "BODY_SECRET", "TRANSPORT_SECRET", "COOKIE_SECRET", "REDIRECT_SECRET", sampleVLESS} {
				if strings.Contains(logs, secret) {
					t.Fatal("subscription logs expose sensitive data")
				}
			}
		})
	}
}

func TestSubscriptionFormats(t *testing.T) {
	vmess := `{"v":"2","ps":"VMess","add":"vm.example","port":"443","id":"123e4567-e89b-12d3-a456-426614174000","aid":"0","net":"ws","path":"/socket","host":"vm.example","tls":"tls"}`
	cases := map[string]string{"plain": sampleVLESS, "base64": base64.StdEncoding.EncodeToString([]byte(sampleVLESS)), "rawurl": base64.RawURLEncoding.EncodeToString([]byte(sampleVLESS)), "trojan": "trojan://secret@vpn.example:443#Trojan", "ss": "ss://" + base64.RawStdEncoding.EncodeToString([]byte("aes-256-gcm:secret")) + "@vpn.example:8388#SS", "legacy-ss": "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret@vpn.example:8388")) + "#Legacy", "vmess": "vmess://" + base64.StdEncoding.EncodeToString([]byte(vmess))}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			nodes, err := ParseSubscription(body)
			if err != nil {
				t.Fatal(err)
			}
			if len(nodes) != 1 || nodes[0].Outbound == nil {
				t.Fatalf("unexpected nodes: %v", nodes)
			}
		})
	}
}
func TestSubscriptionSkipsUnsupportedProtocols(t *testing.T) {
	body := "hysteria2://SECRET@host:443\n" + sampleVLESS + "\nunknown://SECRET@host:443\n" + sampleVLESS + "\ntrojan://SECRET@host:443#Trojan\ntuic://SECRET@host:443"
	for name, input := range map[string]string{"plain": body, "base64": base64.StdEncoding.EncodeToString([]byte(body)), "rawurl": base64.RawURLEncoding.EncodeToString([]byte(body))} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previous) })
			nodes, err := ParseSubscription(input)
			if err != nil {
				t.Fatal(err)
			}
			if len(nodes) != 2 || nodes[0].Protocol != "vless" || nodes[1].Protocol != "trojan" {
				t.Fatal("supported nodes lost or duplicates retained")
			}
			if !strings.Contains(logs.String(), "unsupported_skipped count=3") || strings.Contains(logs.String(), "SECRET") {
				t.Fatal("missing safe diagnostic for skipped protocols")
			}
		})
	}
}

func TestSubscriptionErrorUsesPhysicalLineNumber(t *testing.T) {
	body := "# comment\n\nhysteria2://SECRET@host:443\n" + sampleVLESS + "\n" + sampleVLESS + "\nvless://SECRET@host:no"
	_, err := ParseSubscription(body)
	if err == nil || !strings.HasPrefix(err.Error(), "строка 6:") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("incorrect line diagnostic: %v", err)
	}
}

func TestSubscriptionRejectsInvalidWithoutLeakingSecrets(t *testing.T) {
	for _, s := range []string{"", "<html>Provider error SECRET</html>", "vless://SECRET@host:no", "hysteria2://SECRET@host:443", "ss://SECRET@host:123?plugin=v2ray-plugin", "vless://SECRET@host:443?type=unsupported", sampleVLESS + "\nvless://SECRET@host:no"} {
		_, err := ParseSubscription(s)
		if err == nil {
			t.Fatalf("accepted invalid subscription %q", s)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatal("secret leaked")
		}
	}
}
func TestNodeIDStableAndDuplicatesRemoved(t *testing.T) {
	nodes, err := ParseSubscription(sampleVLESS + "\n" + sampleVLESS)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatal("duplicate retained")
	}
	again, _ := ParseSubscription(sampleVLESS)
	if nodes[0].ID != again[0].ID {
		t.Fatal("unstable identity")
	}
}
func TestTransports(t *testing.T) {
	for _, network := range []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"} {
		t.Run(network, func(t *testing.T) {
			_, err := ParseSubscription(fmt.Sprintf("vless://id@host:443?type=%s&security=reality&pbk=public-key&sni=host", network))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

const sampleXHTTPExtra = `{"xPaddingBytes":"100-1000","noGRPCHeader":false,"xmux":{"maxConcurrency":"16-32","hMaxRequestTimes":"600-900"},"downloadSettings":{"address":"download.example","port":443,"network":"xhttp","security":"tls","tlsSettings":{"serverName":"download.example"},"xhttpSettings":{"path":"/tunnel","mode":"auto"}}}`

func xhttpTestLink(extra string) string {
	q := url.Values{"type": {"xhttp"}, "security": {"tls"}, "sni": {"vpn.example"}, "host": {"vpn.example"}, "path": {"/tunnel"}, "mode": {"auto"}, "extra": {extra}}
	return "vless://123e4567-e89b-12d3-a456-426614174000@vpn.example:443?" + q.Encode() + "#XHTTP"
}
func TestXHTTPExtraSubscription(t *testing.T) {
	lines := make([]string, 0, 13)
	for i := range 12 {
		lines = append(lines, strings.Replace(sampleVLESS, "#Test", fmt.Sprintf("#Node%d", i), 1))
	}
	lines = append(lines, xhttpTestLink(sampleXHTTPExtra))
	nodes, err := ParseSubscription(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 13 {
		t.Fatal("subscription not fully imported")
	}
	stream := nodes[12].Outbound["streamSettings"].(map[string]any)
	encoded, err := json.Marshal(stream["xhttpSettings"])
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Host  string          `json:"host"`
		Path  string          `json:"path"`
		Mode  string          `json:"mode"`
		Extra json.RawMessage `json:"extra"`
	}
	if err = json.Unmarshal(encoded, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Host != "vpn.example" || settings.Path != "/tunnel" || settings.Mode != "auto" {
		t.Fatal("base XHTTP settings lost")
	}
	var original, actual any
	if err = json.Unmarshal([]byte(sampleXHTTPExtra), &original); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(settings.Extra, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, actual) {
		t.Fatal("XHTTP extra changed during import")
	}
}

func TestXHTTPExtraRejectsInvalidJSONWithoutLeakingContent(t *testing.T) {
	for _, extra := range []string{"", `null`, `[]`, `"SECRET"`, `{"header":"SECRET"`, `{} SECRET`, `{"value":"` + strings.Repeat("x", 256<<10) + `SECRET"}`} {
		_, err := ParseSubscription(xhttpTestLink(extra))
		if err == nil {
			t.Fatal("accepted invalid extra")
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatal("extra content leaked in error")
		}
	}
}
func TestXHTTPExtraPreservesJSONTypes(t *testing.T) {
	nodes, err := ParseSubscription(xhttpTestLink(`{"customInteger":9007199254740993,"enabled":false,"nested":{"token":"a+b%2Fc"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(nodes[0].Outbound)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "9007199254740993") || !strings.Contains(string(b), "a+b%2Fc") {
		t.Fatal("JSON number or escaping changed")
	}
	// State cloning must also preserve the raw extra payload, without float64 rounding.
	state := State{Subscriptions: []Subscription{{Nodes: nodes}}}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), "9007199254740993") {
		t.Fatal("JSON integer changed after SQLite round trip")
	}

	b, err = json.Marshal(cloneState(state))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "9007199254740993") {
		t.Fatal("JSON integer changed when cloning state")
	}
}
