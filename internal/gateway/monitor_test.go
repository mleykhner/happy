package gateway

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

func TestTrafficCountersAndRestart(t *testing.T) {
	var upload, download int64 = 1024, 2048
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"stats":{"inbound":{"wireguard":{"uplink":%d,"downlink":%d},"health":{"uplink":9999,"downlink":9999}}}}`, upload, download)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	x := NewXray("", "")
	x.cmd = &exec.Cmd{}
	x.done = make(chan struct{})
	x.metricsPort = p
	s := &Service{xray: x}
	first := s.Traffic(t.Context())
	if !first.Available || first.Upload != 1024 || first.Download != 2048 {
		t.Fatalf("unexpected counters: %+v", first)
	}
	upload, download = 4096, 8192
	s.traffic.Checked = s.traffic.Checked.Add(-2_000_000_000)
	next := s.Traffic(t.Context())
	if next.UploadRate <= 0 || next.DownloadRate <= 0 {
		t.Fatalf("missing rates: %+v", next)
	}
	x.generation++
	upload, download = 1, 2
	restarted := s.Traffic(t.Context())
	if restarted.Upload != 1 || restarted.UploadRate != 0 || restarted.DownloadRate != 0 {
		t.Fatalf("bad restart sample: %+v", restarted)
	}
	close(x.done)
	if s.Traffic(t.Context()).Available {
		t.Fatal("stopped core reported available")
	}
}

func TestRealXrayMetrics(t *testing.T) {
	binary := os.Getenv("HAPPY_TEST_XRAY")
	if binary == "" {
		t.Skip("HAPPY_TEST_XRAY is not set")
	}
	x := NewXray(binary, t.TempDir())
	defer x.Stop()
	state, err := initialState()
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Apply(t.Context(), state, true); err != nil {
		t.Fatal(err)
	}
	s := &Service{xray: x}
	if sample := s.Traffic(t.Context()); !sample.Available {
		t.Fatal("real core metrics unavailable")
	}
}
