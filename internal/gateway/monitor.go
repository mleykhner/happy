package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type Traffic struct {
	Checked      time.Time `json:"checked"`
	Available    bool      `json:"available"`
	Upload       int64     `json:"upload"`
	Download     int64     `json:"download"`
	UploadRate   float64   `json:"uploadRate"`
	DownloadRate float64   `json:"downloadRate"`
}

// Traffic reads only client counters; health checks never contribute to them.
// Counters belong to the current Xray process and reset when it restarts.
func (s *Service) Traffic(ctx context.Context) Traffic {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	x := s.xray
	x.mu.Lock()
	port, generation, running := x.metricsPort, x.generation, x.running()
	x.mu.Unlock()
	if !running || port == 0 {
		return Traffic{}
	}
	if s.traffic.Available && generation == s.trafficGeneration && time.Since(s.traffic.Checked) < 750*time.Millisecond {
		return s.traffic
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/debug/vars", port), nil)
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return Traffic{}
	}
	defer resp.Body.Close()
	var data struct {
		Stats struct {
			Inbound map[string]struct {
				Uplink   int64 `json:"uplink"`
				Downlink int64 `json:"downlink"`
			} `json:"inbound"`
		} `json:"stats"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&data) != nil {
		return Traffic{}
	}
	x.mu.Lock()
	same := generation == x.generation && x.running()
	x.mu.Unlock()
	if !same {
		return Traffic{}
	}
	counter := data.Stats.Inbound["wireguard"]
	next := Traffic{Checked: time.Now(), Available: true, Upload: counter.Uplink, Download: counter.Downlink}
	previous := s.traffic
	if previous.Available && generation == s.trafficGeneration {
		elapsed := next.Checked.Sub(previous.Checked).Seconds()
		next.UploadRate = float64(max(0, next.Upload-previous.Upload)) / elapsed
		next.DownloadRate = float64(max(0, next.Download-previous.Download)) / elapsed
	}
	s.traffic, s.trafficGeneration = next, generation
	return next
}

type NodePing struct {
	ID        string    `json:"id"`
	Checked   time.Time `json:"checked"`
	OK        bool      `json:"ok"`
	LatencyMS int64     `json:"latencyMs"`
}

// PingNodes measures TCP connection establishment from the gateway, independently
// of the selected outbound. At most eight probes run concurrently.
func (s *Service) PingNodes(ctx context.Context) []NodePing {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	s.mu.Lock()
	var nodes []Node
	for _, sub := range s.state.Subscriptions {
		nodes = append(nodes, sub.Nodes...)
	}
	s.mu.Unlock()
	results := make([]NodePing, len(nodes))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range min(8, len(nodes)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				n := nodes[i]
				start := time.Now()
				dialer := net.Dialer{Timeout: 3 * time.Second}
				conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(n.Host, strconv.Itoa(n.Port)))
				result := NodePing{ID: n.ID, Checked: time.Now(), OK: err == nil}
				if err == nil {
					result.LatencyMS = max(1, time.Since(start).Milliseconds())
					conn.Close()
				}
				results[i] = result
			}
		}()
	}
	for i := range nodes {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return results
		}
	}
	close(jobs)
	wg.Wait()
	return results
}
