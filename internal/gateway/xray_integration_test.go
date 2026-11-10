package gateway

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Enable in CI or locally with HAPPY_TEST_XRAY=/absolute/path/to/xray.
// This deliberately uses the actual pinned executable rather than a mock.
func TestRealXrayConfiguration(t *testing.T) {
	binary := os.Getenv("HAPPY_TEST_XRAY")
	if binary == "" {
		t.Skip("HAPPY_TEST_XRAY is not set")
	}
	for _, raw := range []string{"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"add":"vpn.example","port":"443","id":"123e4567-e89b-12d3-a456-426614174000","net":"ws","tls":"tls","path":"/"}`)), xhttpTestLink(sampleXHTTPExtra), sampleVLESS, "trojan://password@vpn.example:443?security=tls#Trojan", "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ@vpn.example:8388#SS"} {
		nodes, err := ParseSubscription(raw)
		if err != nil {
			t.Fatal(err)
		}
		s := selectedState(t)
		s.Subscriptions[0].Nodes = nodes
		s.Selected = nodes[0].ID
		t.Run(nodes[0].Protocol, func(t *testing.T) {
			b, err := BuildConfig(s, 18080, "test-probe")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err = os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.CommandContext(t.Context(), binary, "run", "-test", "-config", path).CombinedOutput(); err != nil {
				t.Fatalf("Xray rejected generated configuration: %v\n%s", err, out)
			}
		})
	}
}

func TestRealXrayStagedConfiguration(t *testing.T) {
	binary := os.Getenv("HAPPY_TEST_XRAY")
	if binary == "" {
		t.Skip("HAPPY_TEST_XRAY is not set")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, stagedConfigName)
	state := selectedState(t)
	body, err := BuildConfig(state, 18080, "probe-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	xray := NewXray(binary, dir)
	if err = xray.validate(t.Context(), path); err != nil {
		t.Fatalf("staged JSON configuration rejected: %v", err)
	}
}
