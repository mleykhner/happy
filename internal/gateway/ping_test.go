package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func waitForPingJob(t *testing.T, s *Service) PingJobView {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		view, changed := s.pingJob.snapshot()
		if view.Status != "running" {
			return view
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatal("ping job did not finish")
		}
	}
}

func TestPingJobUsesDirectHostSocketsWithoutXray(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	// A selected exit with no running core must still permit direct host probes.
	s := &Service{state: State{Selected: "ip", Subscriptions: []Subscription{{Nodes: []Node{
		{ID: "ip", Host: "127.0.0.1", Port: port, Outbound: map[string]any{"protocol": "blackhole"}},
		{ID: "hostname", Host: "localhost", Port: port},
	}}}}}
	t.Cleanup(s.stopPings)
	if _, err := s.StartPings(); err != nil {
		t.Fatal(err)
	}
	view := waitForPingJob(t, s)
	if view.Completed != 2 || view.Total != 2 {
		t.Fatalf("missing results: %+v", view)
	}
	for _, result := range view.Results {
		if !result.OK || result.LatencyMS < 1 || result.Checked.IsZero() {
			t.Fatalf("direct host probe failed: %+v", result)
		}
	}
}

func TestPingLatencyExcludesHostDNS(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	start := time.Now()
	result := measureHostTCP(t.Context(), Node{ID: "dns", Host: "slow-dns.example", Port: listener.Addr().(*net.TCPAddr).Port}, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
	})
	if !result.OK || time.Since(start) < 250*time.Millisecond || result.LatencyMS >= 200 {
		t.Fatalf("DNS delay leaked into TCP latency: %+v", result)
	}
}

func TestLegacyPingResponseIsArray(t *testing.T) {
	s, _ := fixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s.state.Subscriptions = []Subscription{{Nodes: []Node{{ID: "legacy", Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}}}}
	hash, _ := bcrypt.GenerateFromPassword([]byte("legacy-test-password"), bcrypt.MinCost)
	if err := s.store.SetPassword(hash); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(s, fstest.MapFS{}, "http://panel.example", false)
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest("POST", "http://panel.example/api/login", strings.NewReader(`{"password":"legacy-test-password"}`))
	login.Header.Set("Origin", "http://panel.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, login)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	request := httptest.NewRequest("POST", "http://panel.example/api/ping", nil)
	request.Header.Set("Origin", "http://panel.example")
	request.AddCookie(w.Result().Cookies()[0])
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	var results []NodePing
	if err := json.Unmarshal(w.Body.Bytes(), &results); err != nil {
		t.Fatalf("old UI expects an array for .filter(), got HTTP %d: %v", w.Code, err)
	}
	if w.Code != 200 || len(results) != 1 || !results[0].OK {
		t.Fatalf("legacy ping failed: %d %s", w.Code, w.Body.String())
	}
}

func TestPingJobsShareGlobalConcurrencyLimit(t *testing.T) {
	var nodes []Node
	for i := range 8 {
		nodes = append(nodes, Node{ID: fmt.Sprint(i)})
	}
	s := &Service{state: State{Subscriptions: []Subscription{{Nodes: nodes}, {Nodes: nodes}}}}
	t.Cleanup(s.stopPings)
	gate, started := make(chan struct{}), make(chan struct{}, 16)
	var active, peak atomic.Int32
	probe := func(ctx context.Context, n Node) NodePing {
		now := active.Add(1)
		for previous := peak.Load(); now > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, now) {
				break
			}
		}
		defer active.Add(-1)
		started <- struct{}{}
		select {
		case <-ctx.Done():
		case <-gate:
		}
		return NodePing{ID: n.ID, Checked: time.Now(), OK: true}
	}
	first, err := s.startPings(probe)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	var callers sync.WaitGroup
	for range 10 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			view, err := s.StartPings()
			if err != nil || view.ID != first.ID {
				t.Error("overlapping job was started")
			}
		}()
	}
	callers.Wait()
	select {
	case <-started:
		t.Fatal("more than two simultaneous probes")
	case <-time.After(30 * time.Millisecond):
	}
	close(gate)
	view := waitForPingJob(t, s)
	if peak.Load() != 2 || view.Total != 8 || view.Completed != 8 || view.Status != "completed" {
		t.Fatalf("limit/dedup/completion: peak=%d view=%+v", peak.Load(), view)
	}
}

func TestPingJobStopsOnServiceShutdown(t *testing.T) {
	s := &Service{state: State{Subscriptions: []Subscription{{Nodes: []Node{{ID: "pending"}}}}}}
	started := make(chan struct{})
	_, err := s.startPings(func(ctx context.Context, n Node) NodePing {
		close(started)
		<-ctx.Done()
		return NodePing{ID: n.ID}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	s.stopPings()
	view := waitForPingJob(t, s)
	if view.Status != "cancelled" || view.Completed != 0 {
		t.Fatalf("shutdown: %+v", view)
	}
	if _, err := s.StartPings(); err == nil {
		t.Fatal("shutdown service started a job")
	}
}

func readPingEvent(t *testing.T, reader *bufio.Reader) (string, []byte) {
	t.Helper()
	var event, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" && event != "" {
			return event, []byte(data)
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			data = value
		}
	}
}

func TestPingSSEStreamsBeforeCompletionAndReplaysAfterDisconnect(t *testing.T) {
	s, _ := fixture(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("ping-test-password"), bcrypt.MinCost)
	if err := s.store.SetPassword(hash); err != nil {
		t.Fatal(err)
	}
	s.state.Subscriptions = []Subscription{{Nodes: []Node{{ID: "fast"}, {ID: "slow"}}}}
	fast, slow := make(chan struct{}), make(chan struct{})
	job, err := s.startPings(func(ctx context.Context, n Node) NodePing {
		gate := fast
		if n.ID == "slow" {
			gate = slow
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return NodePing{ID: n.ID, OK: true, Checked: time.Now(), LatencyMS: 10}
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(s, fstest.MapFS{}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()
	client.Timeout = 4 * time.Second
	login, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/api/login", strings.NewReader(`{"password":"ping-test-password"}`))
	login.Header.Set("Origin", server.URL)
	response, err := client.Do(login)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("login failed")
	}
	cookie := response.Cookies()[0]
	ctx, cancel := context.WithCancel(t.Context())
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/ping/jobs", nil)
	request.Header.Set("Origin", server.URL)
	request.AddCookie(cookie)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var accepted PingJobView
	err = json.NewDecoder(response.Body).Decode(&accepted)
	response.Body.Close()
	cancel()
	if err != nil || response.StatusCode != 202 || accepted.ID != job.ID {
		t.Fatal("POST did not return the shared background job")
	}
	connect := func() *http.Response {
		t.Helper()
		r, _ := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/api/ping/"+job.ID+"/events", nil)
		r.AddCookie(cookie)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal("SSE response rejected")
		}
		return resp
	}
	stream := connect()
	reader := bufio.NewReader(stream.Body)
	event, data := readPingEvent(t, reader)
	if event != "snapshot" || !strings.Contains(string(data), `"completed":0`) {
		t.Fatalf("missing initial snapshot: %s %s", event, data)
	}
	close(fast)
	event, data = readPingEvent(t, reader)
	if event != "result" || !strings.Contains(string(data), `"id":"fast"`) || s.PingJob().Status != "running" {
		t.Fatalf("result was not streamed before slow node finished: %s %s", event, data)
	}
	stream.Body.Close()
	close(slow)
	if view := waitForPingJob(t, s); view.Completed != 2 {
		t.Fatal("disconnect cancelled the background job")
	}
	replay := connect()
	defer replay.Body.Close()
	reader = bufio.NewReader(replay.Body)
	event, data = readPingEvent(t, reader)
	var restored PingJobView
	if json.Unmarshal(data, &restored) != nil || event != "snapshot" || restored.Completed != 2 || len(restored.Results) != 2 {
		t.Fatalf("bad replay: %s %s", event, data)
	}
	event, _ = readPingEvent(t, reader)
	if event != "done" {
		t.Fatal("missing terminal event")
	}
	unauthenticated, err := client.Get(server.URL + "/api/ping/" + job.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatal("SSE bypassed authentication")
	}
}
