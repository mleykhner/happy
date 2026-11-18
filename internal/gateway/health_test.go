package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTransientConnectionFailureDoesNotRestartClients(t *testing.T) {
	s, _ := fixture(t)
	s.state = selectedState(t)
	if err := s.xray.Apply(t.Context(), s.state, true); err != nil {
		t.Fatal(err)
	}
	generation := s.xray.generation
	calls := 0
	probe := func(context.Context) (time.Duration, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("temporary health endpoint failure")
		}
		return time.Millisecond, nil
	}
	s.checkConnection(t.Context(), probe, s.xray.Restart)
	s.checkConnection(t.Context(), probe, s.xray.Restart)
	if s.xray.generation != generation {
		t.Fatal("one transient HTTPS failure restarted Xray and disconnected all clients")
	}
}

func TestConnectionRecoveryBoundedPerOutage(t *testing.T) {
	s := &Service{state: selectedState(t)}
	probes, restarts := 0, 0
	online := false
	probe := func(context.Context) (time.Duration, error) {
		probes++
		if online {
			return 25 * time.Millisecond, nil
		}
		return 0, errors.New("нет интернета")
	}
	restart := func(_ context.Context, state State) error {
		restarts++
		if state.Selected != s.state.Selected || state.ExitDisabled {
			t.Error("recovery changed the selected exit")
		}
		return errors.New("restart failed")
	}
	h := s.checkConnection(t.Context(), probe, restart)
	if h.Status != "degraded" || restarts != 0 {
		t.Fatalf("unconfirmed failure restarted clients: %+v", h)
	}
	h = s.checkConnection(t.Context(), probe, restart)
	if h.OK || h.Status != "offline" || h.Attempts != 3 || restarts != 3 || probes != 5 {
		t.Fatalf("health=%+v restarts=%d probes=%d", h, restarts, probes)
	}
	s.checkConnection(t.Context(), probe, restart)
	if restarts != 3 || probes != 6 {
		t.Fatal("exhausted retries restarted again or stopped probing")
	}
	online = true
	h = s.checkConnection(t.Context(), probe, restart)
	if !h.OK || h.Status != "online" || h.Attempts != 3 || h.Error != "" || h.LatencyMS != 25 {
		t.Fatalf("did not recover: %+v", h)
	}
	h = s.checkConnection(t.Context(), probe, restart)
	if h.Attempts != 0 {
		t.Fatal("stable recovery did not reset retry budget")
	}
	online = false
	s.checkConnection(t.Context(), probe, restart)
	s.checkConnection(t.Context(), probe, restart)
	if restarts != 6 {
		t.Fatal("new outage did not reset the retry budget")
	}
}

func TestConnectionRecoveryVerifiesSuccessfulReconnect(t *testing.T) {
	s := &Service{state: selectedState(t)}
	restarts := 0
	probe := func(context.Context) (time.Duration, error) {
		if restarts < 2 {
			return 0, errors.New("offline")
		}
		return time.Millisecond, nil
	}
	restart := func(context.Context, State) error { restarts++; return nil }
	s.checkConnection(t.Context(), probe, restart)
	h := s.checkConnection(t.Context(), probe, restart)
	if !h.OK || h.Status != "online" || restarts != 2 || h.Attempts != 2 {
		t.Fatalf("unexpected recovery: %+v, restarts=%d", h, restarts)
	}
}

func TestFlappingConnectionDoesNotResetRetryBudget(t *testing.T) {
	s := &Service{state: selectedState(t)}
	restarts := 0
	online := false
	probe := func(context.Context) (time.Duration, error) {
		if online {
			return time.Millisecond, nil
		}
		return 0, errors.New("offline")
	}
	restart := func(context.Context, State) error { restarts++; return nil }
	s.checkConnection(t.Context(), probe, restart)
	s.checkConnection(t.Context(), probe, restart)
	for range 4 {
		online = true
		s.checkConnection(t.Context(), probe, restart)
		online = false
		s.checkConnection(t.Context(), probe, restart)
		s.checkConnection(t.Context(), probe, restart)
	}
	if restarts != 3 {
		t.Fatalf("flapping connection restarted clients %d times; want at most 3", restarts)
	}
}

func TestHTTPSProbeFallback(t *testing.T) {
	for _, tc := range []struct {
		name               string
		primary, secondary int
		wantOK             bool
		wantRequests       int
	}{
		{"primary works", 204, 500, true, 1},
		{"primary blocked", 403, 204, true, 2},
		{"primary redirects", 302, 204, true, 2},
		{"primary timeout", 0, 204, true, 2},
		{"both fail", 403, 500, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: subscriptionRoundTrip(func(r *http.Request) (*http.Response, error) {
				requests++
				status := tc.primary
				if requests == 2 {
					status = tc.secondary
					if r.URL.Host != "cp.cloudflare.com" {
						t.Errorf("fallback did not use an independent host: %s", r.URL.Host)
					}
				}
				if status == 0 {
					return nil, context.DeadlineExceeded
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			_, err := probeHTTPS(t.Context(), client)
			if (err == nil) != tc.wantOK || requests != tc.wantRequests {
				t.Fatalf("err=%v requests=%d", err, requests)
			}
		})
	}
}

func TestConnectionCheckDiscardedWhenExitDisabled(t *testing.T) {
	s, _ := fixture(t)
	s.state = selectedState(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan Health, 1)
	go func() {
		done <- s.checkConnection(t.Context(), func(context.Context) (time.Duration, error) {
			close(entered)
			<-release
			return time.Millisecond, nil
		}, func(context.Context, State) error {
			t.Error("stale check restarted disabled exit")
			return nil
		})
	}()
	<-entered
	// Applying a configuration must remain possible while HTTPS is in flight.
	if err := s.SetConnection(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	close(release)
	if h := <-done; h.OK || !h.Checked.IsZero() {
		t.Fatalf("old exit reported healthy: %+v", h)
	}
	h := s.checkConnection(t.Context(), func(context.Context) (time.Duration, error) {
		t.Error("disabled exit was probed")
		return 0, nil
	}, nil)
	if h.Status != "idle" {
		t.Fatalf("disabled check: %+v", h)
	}
}

func TestConnectionCheckCancellationAndInterval(t *testing.T) {
	s := &Service{state: selectedState(t)}
	ctx, cancel := context.WithCancel(t.Context())
	s.checkConnection(ctx, func(context.Context) (time.Duration, error) {
		cancel()
		return 0, context.Canceled
	}, func(context.Context, State) error { t.Error("cancelled check restarted"); return nil })
	if !s.health.Checked.IsZero() || s.health.Attempts != 0 || s.health.Status == "checking" {
		t.Fatalf("cancellation recorded as failure: %+v", s.health)
	}
	s.health = Health{Checked: time.Now(), Status: "online", OK: true}
	s.checkConnectionDue(t.Context()) // No Xray needed: a recent result must skip probing.
	if s.health.Status != "online" {
		t.Fatal("check ignored configured interval")
	}
}

func TestXrayRestartReplacesUnchangedProcess(t *testing.T) {
	s, _ := fixture(t)
	state := selectedState(t)
	if err := s.xray.Apply(t.Context(), state, true); err != nil {
		t.Fatal(err)
	}
	generation := s.xray.generation
	if err := s.xray.Apply(t.Context(), state, true); err != nil {
		t.Fatal(err)
	}
	if s.xray.generation != generation {
		t.Fatal("unchanged apply restarted")
	}
	if err := s.xray.Restart(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if s.xray.generation != generation+1 || !s.xray.Running() {
		t.Fatal("reconnect did not replace the process")
	}
}

type subscriptionRoundTrip func(*http.Request) (*http.Response, error)

func (f subscriptionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDailySubscriptionSchedule(t *testing.T) {
	s, _ := fixture(t)
	s.state.Subscriptions = []Subscription{
		{ID: "fresh", URL: "https://provider.example/fresh", Attempted: time.Now().Add(-23 * time.Hour)},
		{ID: "due", URL: "https://provider.example/due", Attempted: time.Now().Add(-25 * time.Hour)},
	}
	requests := 0
	s.fetcher.Transport = subscriptionRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path != "/due" {
			t.Error("subscription refreshed before a day elapsed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sampleVLESS)), Request: r}, nil
	})
	s.refreshDue(t.Context())
	s.refreshDue(t.Context())
	if requests != 1 || len(s.state.Subscriptions[1].Nodes) != 1 {
		t.Fatalf("daily refresh: requests=%d", requests)
	}
}

func TestHealthSettingsDefaultsAndMigration(t *testing.T) {
	for _, minutes := range []int{60, 120} {
		s, _ := fixture(t)
		s.state.Settings.RefreshMinutes = minutes
		s.state.Settings.HealthIntervalSeconds = 0
		if err := s.store.save(s.state); err != nil {
			t.Fatal(err)
		}
		migrated, err := NewService(s.store, s.xray)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(migrated.Close)
		want := minutes
		if minutes == 60 {
			want = 1440
		}
		stored, err := s.store.load()
		if err != nil || migrated.state.Settings.HealthIntervalSeconds != 30 || stored.Settings.RefreshMinutes != want {
			t.Fatalf("migration: %+v, %v", stored.Settings, err)
		}
	}
	state, _ := initialState()
	for _, seconds := range []int{-1, 0, 4, 3601} {
		state.Settings.HealthIntervalSeconds = seconds
		if state.Settings.validate() == nil {
			t.Errorf("accepted invalid interval %d", seconds)
		}
	}
}
