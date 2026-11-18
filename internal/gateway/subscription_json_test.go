package gateway

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sampleXraySubscription = `{"remarks":"JSON gateway","inbounds":[{"tag":"socks-in","protocol":"socks","listen":"0.0.0.0","port":1080}],"outbounds":[{"tag":"proxy-a","protocol":"vless","settings":{"vnext":[{"address":"vpn.example","port":443,"users":[{"id":"123e4567-e89b-12d3-a456-426614174000","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"vpn.example"}}},{"tag":"block","protocol":"blackhole"},{"tag":"direct","protocol":"freedom"}],"routing":{"domainStrategy":"AsIs","rules":[{"type":"field","inboundTag":["socks-in"],"network":"tcp,udp","balancerTag":"pool"}],"balancers":[{"tag":"pool","selector":["proxy-"],"strategy":{"type":"random"}}]},"observatory":{"subjectSelector":["proxy-"],"probeURL":"https://www.gstatic.com/generate_204","probeInterval":"1m"},"env":{"SECRET":"do-not-import"},"log":{"error":"/tmp/SECRET"}}`

func TestXraySubscriptionContainers(t *testing.T) {
	for name, body := range map[string]string{
		"object":           sampleXraySubscription,
		"array":            "[" + sampleXraySubscription + "," + sampleXraySubscription + "]",
		"base64":           base64.StdEncoding.EncodeToString([]byte(sampleXraySubscription)),
		"base64url":        base64.RawURLEncoding.EncodeToString([]byte("[" + sampleXraySubscription + "]")),
		"mixed containers": "[" + sampleXraySubscription + `,{"type":"amneziawg","servers":[{"config":"INVALID"}]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			nodes, err := ParseSubscription(body)
			if err != nil {
				t.Fatal(err)
			}
			if len(nodes) != 1 || nodes[0].Name != "JSON gateway" || nodes[0].Host != "vpn.example" || nodes[0].FullConfig == nil {
				t.Fatal("JSON configuration was not imported as one server")
			}
			state := selectedState(t)
			state.Subscriptions[0].Nodes, state.Selected = nodes, nodes[0].ID
			restored := cloneState(state)
			b, err := BuildConfig(restored, 18080, "test-probe")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), `"port": 1080`) {
				t.Fatal("provider runtime configuration escaped into Happy")
			}
			var config struct {
				Routing struct {
					Rules     []map[string]any
					Balancers []map[string]any
				}
				Outbounds []map[string]any
			}
			if err = json.Unmarshal(b, &config); err != nil {
				t.Fatal(err)
			}
			if config.Routing.Rules[0]["outboundTag"] != "block" || len(config.Routing.Balancers) != 1 {
				t.Fatal("gateway protection or balancer lost")
			}
			if config.Routing.Rules[1]["balancerTag"] != "provider-pool" {
				t.Fatal("provider route not retained")
			}
			state.ExitDisabled = true
			b, err = BuildConfig(state, 18080, "test-probe")
			if err != nil || strings.Contains(string(b), "vpn.example") || strings.Contains(string(b), "provider-pool") {
				t.Fatal("disabled full configuration left a usable exit")
			}
		})
	}
}

func TestMixedSubscriptionRoutingDirectives(t *testing.T) {
	body := "#profile-title: Example\n://autorouting/onadd/https://example.org/routing.json\n" + sampleVLESS + "\n://routing/add/SECRET\nincy://routing/SECRET"
	for _, input := range []string{body, base64.StdEncoding.EncodeToString([]byte(body))} {
		nodes, err := ParseSubscription(input)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("routing directive treated as node: %v", err)
		}
	}
}

func TestJSONSubscriptionRejectsInvalidWithoutLeakingSecrets(t *testing.T) {
	for _, body := range []string{
		`{`, `[]`, `null`, `{"outbounds":null}`, `[{"outbounds":[]},null]`,
		strings.Replace(sampleXraySubscription, `"port":443`, `"port":99999`, 1),
		strings.Replace(sampleXraySubscription, `"serverName":"vpn.example"`, `"allowInsecure":true,"serverName":"SECRET"`, 1),
		strings.Replace(sampleXraySubscription, `"serverName":"vpn.example"`, `"certificates":[{"certificateFile":"/tmp/SECRET"}]`, 1),
	} {
		_, err := ParseSubscription(body)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("invalid JSON accepted or secret leaked")
		}
	}
}

func TestRealXrayJSONSubscription(t *testing.T) {
	binary := os.Getenv("HAPPY_TEST_XRAY")
	if binary == "" {
		t.Skip("HAPPY_TEST_XRAY is not set")
	}
	nodes, err := ParseSubscription(sampleXraySubscription)
	if err != nil {
		t.Fatal(err)
	}
	state := selectedState(t)
	state.Subscriptions[0].Nodes, state.Selected = nodes, nodes[0].ID
	b, err := BuildConfig(state, 18080, "test-probe")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), binary, "run", "-test", "-config", path).CombinedOutput(); err != nil {
		t.Fatalf("Xray rejected JSON subscription: %v\n%s", err, out)
	}
}

func TestXraySubscriptionDependenciesAndMetrics(t *testing.T) {
	var source map[string]any
	if err := json.Unmarshal([]byte(sampleXraySubscription), &source); err != nil {
		t.Fatal(err)
	}
	source["dns"] = map[string]any{"tag": "dns-in", "servers": []string{"1.1.1.1"}}
	source["policy"] = map[string]any{"levels": map[string]any{"0": map[string]any{"connIdle": 42}}}
	outbound := source["outbounds"].([]any)[0].(map[string]any)
	stream := outbound["streamSettings"].(map[string]any)
	stream["sockopt"] = map[string]any{"dialerProxy": "direct"}
	stream["xhttpSettings"] = map[string]any{"downloadSettings": map[string]any{"sockopt": map[string]any{"dialerProxy": "direct"}}}
	body, _ := json.Marshal(source)
	nodes, err := ParseSubscription(string(body))
	if err != nil {
		t.Fatal(err)
	}
	state := selectedState(t)
	state.Subscriptions[0].Nodes, state.Selected = nodes, nodes[0].ID
	before, _ := json.Marshal(nodes)
	first, err := buildConfig(state, 18080, "probe", 19090)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildConfig(cloneState(state), 18080, "probe", 19090)
	if err != nil || string(first) != string(second) {
		t.Fatal("configuration changes after state reload")
	}
	after, _ := json.Marshal(nodes)
	if string(before) != string(after) {
		t.Fatal("tag rewriting mutated persisted nodes")
	}
	var generated struct {
		Policy struct {
			Levels map[string]any
			System map[string]bool
		}
		DNS struct{ Tag string }
	}
	if err = json.Unmarshal(first, &generated); err != nil {
		t.Fatal(err)
	}
	if generated.Policy.Levels["0"] == nil || !generated.Policy.System["statsInboundUplink"] || generated.DNS.Tag != "provider-dns-in" {
		t.Fatal("provider policy or gateway metrics lost")
	}
	if strings.Count(string(first), `"dialerProxy": "provider-direct"`) != 2 {
		t.Fatal("nested outbound dependencies were not isolated")
	}
}

func TestUnsupportedWireGuardContainers(t *testing.T) {
	conf := "# Name = SECRET\n[Interface]\nPrivateKey = SECRET\n[Peer]\nPublicKey = SECRET\nEndpoint = example.net:51820"
	for _, body := range []string{conf, base64.StdEncoding.EncodeToString([]byte(conf))} {
		_, err := ParseSubscription(body)
		if err == nil || !strings.Contains(err.Error(), "WireGuard/AmneziaWG") || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("unsupported .conf format was not identified safely")
		}
	}
}
