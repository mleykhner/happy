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
	if h.OK || h.Status != "offline" || h.Attempts != 3 || restarts != 3 || probes != 4 {
		t.Fatalf("health=%+v restarts=%d probes=%d", h, restarts, probes)
	}
	s.checkConnection(t.Context(), probe, restart)
	if restarts != 3 || probes != 5 {
		t.Fatal("exhausted retries restarted again or stopped probing")
	}
	online = true
	h = s.checkConnection(t.Context(), probe, restart)
	if !h.OK || h.Status != "online" || h.Attempts != 0 || h.Error != "" || h.LatencyMS != 25 {
		t.Fatalf("did not recover: %+v", h)
	}
	online = false
	s.checkConnection(t.Context(), probe, restart)
	if restarts != 6 {
		t.Fatal("new outage did not reset the retry budget")
	}
}

func TestConnectionRecoveryVerifiesSuccessfulReconnect(t *testing.T) {
	s := &Service{state: selectedState(t)}
	restarts := 0
	h := s.checkConnection(t.Context(), func(context.Context) (time.Duration, error) {
		if restarts < 2 {
			return 0, errors.New("offline")
		}
		return time.Millisecond, nil
	}, func(context.Context, State) error { restarts++; return nil })
	if !h.OK || h.Status != "online" || restarts != 2 || h.Attempts != 0 {
		t.Fatalf("unexpected recovery: %+v, restarts=%d", h, restarts)
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
