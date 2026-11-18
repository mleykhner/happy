package gateway

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"
)

const pingParallelism = 2
const pingSpacing = 150 * time.Millisecond
const pingTimeout = 3 * time.Second

type NodePing struct {
	ID        string    `json:"id"`
	Checked   time.Time `json:"checked"`
	OK        bool      `json:"ok"`
	LatencyMS int64     `json:"latencyMs"`
	Error     string    `json:"error,omitempty"`
}

type PingJobView struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	Started   time.Time  `json:"started"`
	NodeIDs   []string   `json:"nodeIds"`
	Total     int        `json:"total"`
	Completed int        `json:"completed"`
	Results   []NodePing `json:"results"`
}

type pingJob struct {
	mu      sync.Mutex
	view    PingJobView
	changed chan struct{}
	cancel  context.CancelFunc
}

func (j *pingJob) snapshot() (PingJobView, <-chan struct{}) {
	return j.updates(0)
}

func (j *pingJob) updates(after int) (PingJobView, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := j.view
	v.Results = slices.Clone(v.Results[min(after, len(v.Results)):])
	if after == 0 {
		v.NodeIDs = slices.Clone(v.NodeIDs)
	} else {
		v.NodeIDs = nil
	}
	return v, j.changed
}

func (j *pingJob) publish(result NodePing) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.view.Results = append(j.view.Results, result)
	j.view.Completed++
	close(j.changed)
	j.changed = make(chan struct{})
}

// StartPings returns immediately. All browsers share one running job, so
// duplicate clicks or SSE reconnects cannot increase the number of probes.
func (s *Service) StartPings() (PingJobView, error) {
	return s.startPings(pingNodeDirect)
}

func (s *Service) startPings(probe func(context.Context, Node) NodePing) (PingJobView, error) {
	j, err := s.startPingJob(probe)
	if err != nil {
		return PingJobView{}, err
	}
	view, _ := j.snapshot()
	return view, nil
}

func (s *Service) startPingJob(probe func(context.Context, Node) NodePing) (*pingJob, error) {
	s.pingMu.Lock()
	defer s.pingMu.Unlock()
	if s.pingClosed {
		return nil, errors.New("сервис остановлен")
	}
	if s.pingJob != nil {
		view, _ := s.pingJob.snapshot()
		if view.Status == "running" {
			return s.pingJob, nil
		}
	}
	s.mu.Lock()
	var nodes []Node
	ids := []string{}
	seen := map[string]bool{}
	for _, sub := range s.state.Subscriptions {
		for _, n := range sub.Nodes {
			if !seen[n.ID] {
				seen[n.ID] = true
				nodes = append(nodes, Node{ID: n.ID, Host: n.Host, Port: n.Port})
				ids = append(ids, n.ID)
			}
		}
	}
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	j := &pingJob{view: PingJobView{ID: randomToken(), Status: "running", Started: time.Now(), NodeIDs: ids, Total: len(nodes), Results: []NodePing{}}, changed: make(chan struct{}), cancel: cancel}
	s.pingJob = j
	go j.run(ctx, nodes, probe)
	return j, nil
}

func (s *Service) PingJob() PingJobView {
	s.pingMu.Lock()
	defer s.pingMu.Unlock()
	if s.pingJob == nil {
		return PingJobView{Status: "idle", NodeIDs: []string{}, Results: []NodePing{}}
	}
	view, _ := s.pingJob.snapshot()
	return view
}

func (s *Service) stopPings() {
	s.pingMu.Lock()
	defer s.pingMu.Unlock()
	s.pingClosed = true
	if s.pingJob != nil {
		s.pingJob.cancel()
	}
}

func (j *pingJob) run(ctx context.Context, nodes []Node, probe func(context.Context, Node) NodePing) {
	defer j.cancel()
	jobs := make(chan Node)
	var workers sync.WaitGroup
	for range min(pingParallelism, len(nodes)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for n := range jobs {
				if ctx.Err() != nil {
					return
				}
				result := probe(ctx, n)
				if ctx.Err() != nil {
					return
				}
				j.publish(result)
				timer := time.NewTimer(pingSpacing)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
dispatch:
	for _, n := range nodes {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- n:
		}
	}
	close(jobs)
	workers.Wait()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.view.Status = "completed"
	if ctx.Err() != nil {
		j.view.Status = "cancelled"
	}
	close(j.changed)
}

// Resolve through the host's resolver, then time only TCP establishment. This
// uses neither an HTTP/SOCKS proxy nor Xray's DNS or outbound configuration.
func pingNodeDirect(ctx context.Context, n Node) NodePing {
	return measureHostTCP(ctx, n, net.DefaultResolver.LookupIPAddr)
}

func measureHostTCP(ctx context.Context, n Node, lookup func(context.Context, string) ([]net.IPAddr, error)) NodePing {
	result := NodePing{ID: n.ID}
	ip := net.ParseIP(n.Host)
	if ip == nil {
		dnsCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		addresses, err := lookup(dnsCtx, n.Host)
		cancel()
		if err != nil || len(addresses) == 0 {
			result.Checked, result.Error = time.Now(), "Не удалось разрешить адрес сервера"
			return result
		}
		ip = addresses[0].IP
		for _, address := range addresses {
			if address.IP.To4() != nil {
				ip = address.IP
				break
			}
		}
	}
	dialCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	dialer := net.Dialer{}
	start := time.Now()
	conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(n.Port)))
	result.Checked = time.Now()
	if err != nil {
		result.Error = "TCP-порт недоступен"
		return result
	}
	result.OK, result.LatencyMS = true, max(1, result.Checked.Sub(start).Milliseconds())
	conn.Close()
	return result
}
