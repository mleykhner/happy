package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWaitProviderResponseRecoversTransientFailure(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("WireGuard session unavailable")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("through-provider")), Header: make(http.Header)}, nil
	})}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitProviderResponse(ctx, client, "http://fixture.test/test"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("unexpected attempts: %d", attempts)
	}
}

func TestWaitProviderResponseHonorsDeadline(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := waitProviderResponse(ctx, client, "http://fixture.test/test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing deadline error: %v", err)
	}
}

func TestWaitProviderResponseRejectsWrongPayload(t *testing.T) {
	for _, status := range []int{200, 503} {
		client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("wrong-target")), Header: make(http.Header)}, nil
		})}
		if err := waitProviderResponse(t.Context(), client, "http://fixture.test/test"); err == nil {
			t.Fatal("accepted response from wrong target")
		}
	}
}
