package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/crypto/bcrypt"
)

func fixture(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	script := filepath.Join(dir, "xray")
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	body := `#!` + shell + `
if [ "$2" = "-test" ]; then
 [ ! -f "` + dir + `/fail-test" ]
 exit $?
fi
if [ -f "` + dir + `/fail-start" ]; then rm "` + dir + `/fail-start"; exit 1; fi
exec sleep 600
`
	if err = os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	xray := NewXray(script, dir)
	xray.probePort = 18080
	t.Cleanup(xray.Stop)
	s, err := NewService(store, xray)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, dir
}
func selectedState(t *testing.T) State {
	t.Helper()
	s, err := initialState()
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := ParseSubscription(sampleVLESS)
	if err != nil {
		t.Fatal(err)
	}
	s.Subscriptions = []Subscription{{ID: "sub", Nodes: nodes}}
	s.Selected = nodes[0].ID
	priv, pub, err := keyPair()
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &Client{PrivateKey: priv, PublicKey: pub, Enabled: true}
	return s
}
func TestNoDirectOutboundAndIPv6Blocked(t *testing.T) {
	for _, selected := range []bool{false, true} {
		s := selectedState(t)
		if !selected {
			s.Selected = ""
		}
		b, err := BuildConfig(s, 18080, "probe-secret")
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err = json.Unmarshal(b, &config); err != nil {
			t.Fatal(err)
		}
		for _, o := range config["outbounds"].([]any) {
			if o.(map[string]any)["protocol"] == "freedom" {
				t.Fatal("direct outbound present")
			}
		}
		rules := config["routing"].(map[string]any)["rules"].([]any)
		last := rules[len(rules)-1].(map[string]any)
		want := "block"
		if selected {
			want = "proxy"
		}
		if last["outboundTag"] != want {
			t.Fatalf("fallback = %v", last)
		}
		if !strings.Contains(string(b), `"::/0"`) {
			t.Fatal("IPv6 not blocked")
		}
	}
}
func TestClientConfigCapturesIPv6AndDNS(t *testing.T) {
	s := selectedState(t)
	s.Settings.Endpoint = "vpn.example"
	config, err := s.clientConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AllowedIPs = 0.0.0.0/0, ::/0", "fd77::2/128", "DNS = 1.1.1.1", "vpn.example:51820"} {
		if !strings.Contains(config, want) {
			t.Fatalf("missing %s", want)
		}
	}
}
func TestApplyRollbackAndForcedClose(t *testing.T) {
	s, dir := fixture(t)
	old := selectedState(t)
	if err := s.xray.Apply(t.Context(), old, false); err != nil {
		t.Fatal(err)
	}
	previous := string(s.xray.last)
	if err := os.WriteFile(filepath.Join(dir, "fail-start"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	next := cloneState(old)
	next.Settings.WireGuardPort++
	if err := s.xray.Apply(t.Context(), next, false); err == nil {
		t.Fatal("expected failed start")
	}
	if !s.xray.Running() || string(s.xray.last) != previous {
		t.Fatal("rollback did not restore process")
	}
	if err := os.WriteFile(filepath.Join(dir, "fail-test"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.xray.Apply(t.Context(), next, true); err == nil {
		t.Fatal("expected validation failure")
	}
	if s.xray.Running() {
		t.Fatal("old process survived forced closure")
	}
}
func TestRefreshFailurePreservesNodesAndRemovedNodeClosesExit(t *testing.T) {
	s, _ := fixture(t)
	body := sampleVLESS
	status := 200
	s.fetcher = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	if err := s.AddSubscription(t.Context(), "Provider", "https://provider.example/SECRET"); err != nil {
		t.Fatal(err)
	}
	id := s.state.Subscriptions[0].ID
	if err := s.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	node := s.state.Subscriptions[0].Nodes[0].ID
	if err := s.Select(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	status = 500
	if err := s.Refresh(t.Context(), id); err == nil {
		t.Fatal("expected fetch failure")
	}
	if s.state.Selected != node || s.state.activeNode() == nil {
		t.Fatal("lost last good subscription")
	}
	status = 200
	body = strings.ReplaceAll(sampleVLESS, "vpn.example", "other.example")
	if err := s.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if s.state.Selected != "" || s.state.activeNode() != nil {
		t.Fatal("vanished node remains selected")
	}
	persisted, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Selected != "" {
		t.Fatal("closure not persisted")
	}
	view, _ := json.Marshal(s.View())
	for _, secret := range []string{"SECRET", s.state.ServerPrivate} {
		if strings.Contains(string(view), secret) {
			t.Fatal("secret exposed in public state")
		}
	}
}
func TestAdminAuthenticationCSRFAndSecretExport(t *testing.T) {
	s, _ := fixture(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("a-long-test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.SetPassword(hash); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(s, fstest.MapFS{"index.html": {Data: []byte("hello")}}, "https://panel.example", true)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/state", "", "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("POST", "/api/login", `{"password":"a-long-test-password"}`, "https://evil.example", nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w := request("POST", "/api/login", `{"password":"a-long-test-password"}`, "https://panel.example", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("weak cookie")
	}
	if w = request("GET", "/api/state", "", "", cookie); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("POST", "/api/select", `{"id":""}`, "https://evil.example", cookie); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w = request("PUT", "/api/settings", `{"endpoint":"vpn.example","wireguardPort":51820,"refreshMinutes":60}`, "https://panel.example", cookie); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("POST", "/api/client/create", "", "https://panel.example", cookie); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request("GET", "/api/client/config", "", "", nil); w.Code != 401 {
		t.Fatal("unauthenticated key export")
	}
	if w = request("GET", "/api/client/config", "", "", cookie); w.Code != 200 || !strings.Contains(w.Body.String(), "PrivateKey = ") {
		t.Fatal("missing authenticated client config")
	}
	if w = request("GET", "/api/client/qr", "", "", cookie); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(w.Body.String(), "\x89PNG") {
		t.Fatal("invalid QR response")
	}
	if w = request("GET", "/api/state", "", "", cookie); strings.Contains(w.Body.String(), s.state.Clients[0].PrivateKey) {
		t.Fatal("private key exposed in overview")
	}

	if w = request("POST", "/api/clients", `{"name":"Second"}`, "https://panel.example", cookie); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	second := s.state.Clients[1]
	path := "/api/clients/" + second.ID
	for _, suffix := range []string{"/config", "/qr"} {
		if w = request("GET", path+suffix, "", "", nil); w.Code != 401 {
			t.Fatal("unauthenticated client export", suffix)
		}
		if w = request("GET", path+suffix, "", "", cookie); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("client export failed", suffix, w.Body.String())
		}
	}
	if w = request("GET", path+"/config", "", "", cookie); !strings.Contains(w.Body.String(), second.PrivateKey) || strings.Contains(w.Body.String(), s.state.Clients[0].PrivateKey) {
		t.Fatal("wrong client exported")
	}
	if w = request("GET", "/api/clients/missing/config", "", "", cookie); w.Code == 200 {
		t.Fatal("missing client exported")
	}
	if w = request("POST", path+"/disable", "", "https://evil.example", cookie); w.Code != 403 {
		t.Fatal("client mutation accepted foreign origin")
	}
	if w = request("POST", path+"/disable", "", "https://panel.example", cookie); w.Code != 200 || s.state.Clients[1].Enabled || !s.state.Clients[0].Enabled {
		t.Fatal("wrong client disabled")
	}
	if w = request("DELETE", path, "", "https://panel.example", cookie); w.Code != 200 || len(s.state.Clients) != 1 {
		t.Fatal("client deletion failed")
	}
	if w = request("POST", "/api/logout", "", "https://panel.example", cookie); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("GET", "/api/state", "", "", cookie); w.Code != 401 {
		t.Fatal("session survived logout")
	}
}
func TestSettingsRejectConfigInjection(t *testing.T) {
	for _, host := range []string{"vpn.example\n[Peer]", "vpn.example:123", "https://vpn.example", "bad host", "bad;host"} {
		if err := (Settings{Endpoint: host, WireGuardPort: 51820, RefreshMinutes: 60}).validate(); err == nil {
			t.Fatalf("accepted %q", host)
		}
	}
}
func TestDisabledClientRemovedFromInbound(t *testing.T) {
	s := selectedState(t)
	s.Client.Enabled = false
	b, err := BuildConfig(s, 18080, "probe")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"protocol": "wireguard"`) {
		t.Fatal("disabled peer still listening")
	}
}
func TestKeyAndStateSurviveRestart(t *testing.T) {
	s, _ := fixture(t)
	next := selectedState(t)
	if err := s.store.save(next); err != nil {
		t.Fatal(err)
	}
	restored, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	if next.Client.PrivateKey != restored.Client.PrivateKey || next.ServerPrivate != restored.ServerPrivate || next.Selected != restored.Selected {
		t.Fatal("lost state")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRevocationLatchSurvivesDatabaseWriteFailure(t *testing.T) {
	s, dir := fixture(t)
	state := selectedState(t)
	s.state = state
	if err := s.store.save(state); err != nil {
		t.Fatal(err)
	}
	// Leave a persisted old state but make the update fail after the latch is durable.
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	next := cloneState(state)
	next.Selected = ""
	if err := s.commit(t.Context(), next, true); err == nil {
		t.Fatal("expected database failure")
	}
	if s.state.Selected != "" || s.xray.Running() {
		t.Fatal("revoked state reopened in memory")
	}
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := NewService(reopened, s.xray)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.state.Selected != "" || recovered.state.Clients[0].Enabled {
		t.Fatal("old exit restored after restart")
	}
}

func TestRefreshSendsStableHWID(t *testing.T) {
	s, _ := fixture(t)
	var deviceIDs []string
	notice := "vless://123e4567-e89b-12d3-a456-426614174000@vpn.example:443#Приложение%20не%20поддерживает%20HWID\nvless://123e4567-e89b-12d3-a456-426614174000@vpn.example:443#Используйте%20Happ%20или%20INCY"
	s.fetcher = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deviceIDs = append(deviceIDs, r.Header.Get("X-Hwid"))
		if id := r.Header.Get("X-Hwid"); id != "" && !regexp.MustCompile(`^[a-zA-Z0-9=-]{10,64}$`).MatchString(id) {
			t.Fatal("invalid HWID format")
		}
		if r.Header.Get("X-Device-Os") == "" || r.Header.Get("X-Device-Model") == "" {
			t.Fatal("missing device metadata")
		}
		body := sampleVLESS
		if r.Header.Get("X-Hwid") == "" {
			body = notice
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	if err := s.AddSubscription(t.Context(), "Provider", "https://provider.example/sub/test"); err != nil {
		t.Fatal(err)
	}
	id := s.state.Subscriptions[0].ID
	if err := s.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if len(s.state.Subscriptions[0].Nodes) != 1 || s.state.Subscriptions[0].Nodes[0].Name != "Test" {
		t.Fatal("HWID refusal imported as selectable nodes instead of actual subscription")
	}
	if err := s.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if len(deviceIDs) != 2 || deviceIDs[0] == "" || deviceIDs[0] != deviceIDs[1] {
		t.Fatal("missing or changing HWID")
	}
	restarted, err := NewService(s.store, s.xray)
	if err != nil {
		t.Fatal(err)
	}
	restarted.fetcher = s.fetcher
	if err = restarted.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if deviceIDs[0] != deviceIDs[2] {
		t.Fatal("HWID changed after restart")
	}
}

func TestProviderHWIDRefusalsAreNotImported(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		body   string
		status int
	}{
		{"unsupported", http.Header{"X-Hwid-Not-Supported": {"true"}}, sampleVLESS, 200},
		{"device-limit", http.Header{"X-Hwid-Max-Devices-Reached": {"true"}}, sampleVLESS, 200},
		{"blocked", http.Header{"X-Hwid-Active": {"true"}}, "", 404},
		{"notice-only", make(http.Header), "vless://id@vpn.example:443#Приложение%20не%20поддерживает%20HWID", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixture(t)
			if err := s.AddSubscription(t.Context(), "Provider", "https://provider.example/SECRET"); err != nil {
				t.Fatal(err)
			}
			s.fetcher = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: tc.header, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})}
			if err := s.Refresh(t.Context(), s.state.Subscriptions[0].ID); err == nil {
				t.Fatal("provider refusal treated as successful subscription")
			}
			if len(s.state.Subscriptions[0].Nodes) != 0 {
				t.Fatal("refusal imported as node")
			}
			if strings.Contains(s.state.Subscriptions[0].Error, "SECRET") {
				t.Fatal("URL leaked")
			}
		})
	}
}
func TestLegacyStateGetsHWIDAndDiscardsOldNoticeNodes(t *testing.T) {
	s, _ := fixture(t)
	state := selectedState(t)
	state.DeviceID = ""
	state.Subscriptions[0].Nodes[0].Name = "Приложение не поддерживает HWID"
	if err := s.store.save(state); err != nil {
		t.Fatal(err)
	}
	migrated, err := NewService(s.store, s.xray)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.state.DeviceID == "" || len(migrated.state.Subscriptions[0].Nodes) != 0 || migrated.state.Selected != "" {
		t.Fatal("legacy state was not migrated safely")
	}
	next, err := NewService(s.store, s.xray)
	if err != nil {
		t.Fatal(err)
	}
	if next.state.DeviceID != migrated.state.DeviceID {
		t.Fatal("migrated HWID changed")
	}
}

func TestPasswordChangePersistsAndRevokesOtherSessions(t *testing.T) {
	s, _ := fixture(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("old-password-for-test"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetPassword(hash); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(s, fstest.MapFS{}, "https://panel.example", true)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		method := "POST"
		if path == "/api/password" {
			method = "PUT"
		}
		if path == "/api/state" {
			method = "GET"
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", "https://panel.example")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	login := func() *http.Cookie {
		w := request("/api/login", `{"password":"old-password-for-test"}`, nil)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		return w.Result().Cookies()[0]
	}
	first, second := login(), login()
	for _, body := range []string{`{"currentPassword":"wrong","newPassword":"new-password-for-test"}`, `{"currentPassword":"old-password-for-test","newPassword":"short"}`} {
		if w := request("/api/password", body, first); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	if w := request("/api/password", `{"currentPassword":"old-password-for-test","newPassword":"new-password-for-test"}`, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("/api/password", `{"currentPassword":"old-password-for-test","newPassword":"new-password-for-test"}`, first); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request("/api/state", "", first); w.Code != 200 {
		t.Fatal("current session revoked")
	}
	if w := request("/api/state", "", second); w.Code != 401 {
		t.Fatal("other session survived")
	}
	if w := request("/api/login", `{"password":"old-password-for-test"}`, nil); w.Code != 401 {
		t.Fatal("old password accepted")
	}
	if w := request("/api/login", `{"password":"new-password-for-test"}`, nil); w.Code != 200 {
		t.Fatal("new password rejected")
	}
	if err := s.store.InitializePassword(hash); err != nil {
		t.Fatal(err)
	}
	persisted, err := s.store.password()
	if err != nil || bcrypt.CompareHashAndPassword(persisted, []byte("new-password-for-test")) != nil {
		t.Fatal("bootstrap overwrote changed password")
	}
	restarted, err := Handler(s, fstest.MapFS{}, "https://panel.example", true)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"new-password-for-test"}`))
	r.Header.Set("Origin", "https://panel.example")
	w := httptest.NewRecorder()
	restarted.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("password did not survive restart")
	}
}

func TestConnectionTogglePreservesServerAndBlocksAfterRestart(t *testing.T) {
	s, _ := fixture(t)
	s.state = selectedState(t)
	selected := s.state.Selected
	if err := s.SetConnection(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if s.state.Selected != selected || !s.state.ExitDisabled {
		t.Fatal("disconnect lost server selection")
	}
	restored, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	config, err := BuildConfig(restored, 18080, "test")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Outbounds []struct {
			Protocol string `json:"protocol"`
		}
		Routing struct {
			Rules []struct {
				OutboundTag string `json:"outboundTag"`
			}
		}
	}
	if err := json.Unmarshal(config, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Outbounds) != 1 || v.Outbounds[0].Protocol != "blackhole" || v.Routing.Rules[len(v.Routing.Rules)-1].OutboundTag != "block" {
		t.Fatal("disconnected gateway permits exit")
	}
	if err := s.SetConnection(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if s.state.ExitDisabled || s.state.Selected != selected {
		t.Fatal("reconnect failed")
	}
	if err := s.Select(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConnection(t.Context(), true); err == nil {
		t.Fatal("connected without subscription node")
	}
}
