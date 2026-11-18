package gateway

import (
	"context"
	"fmt"
	"time"
)

const maxReconnectAttempts = 3

func (s *Service) runHealthChecks(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkConnectionDue(ctx)
		}
	}
}

func (s *Service) checkConnectionDue(ctx context.Context) {
	// A manual check and the scheduler share one recovery cycle.
	if !s.probeMu.TryLock() {
		return
	}
	defer s.probeMu.Unlock()
	s.mu.Lock()
	due := !s.state.ExitDisabled && s.state.activeNode() != nil &&
		time.Since(s.health.Checked) >= time.Duration(s.state.Settings.HealthIntervalSeconds)*time.Second
	s.mu.Unlock()
	if due {
		s.checkConnection(ctx, s.xray.Probe, s.xray.Restart)
	}
}

// Callers hold probeMu. Network requests run without mu so state remains readable
// and selecting/disabling an exit can invalidate a check in flight.
func (s *Service) checkConnection(ctx context.Context, probe func(context.Context) (time.Duration, error), restart func(context.Context, State) error) Health {
	s.mu.Lock()
	if s.state.ExitDisabled || s.state.activeNode() == nil {
		s.health = Health{Status: "idle"}
		health := s.health
		s.mu.Unlock()
		return health
	}
	revision := s.configRevision
	previous := s.health
	s.health.Status = "checking"
	s.mu.Unlock()

	for {
		latency, err := probe(ctx)
		s.mu.Lock()
		if revision != s.configRevision {
			health := s.health
			s.mu.Unlock()
			return health
		}
		if ctx.Err() != nil {
			if s.health.Attempts > previous.Attempts {
				s.health.Status = "offline"
			} else {
				s.health = previous
			}
			health := s.health
			s.mu.Unlock()
			return health
		}
		s.health.Checked = time.Now()
		s.health.OK = err == nil
		s.health.LatencyMS = latency.Milliseconds()
		if err == nil {
			if s.health.Attempts > 0 || previous.Status == "offline" {
				s.event("Подключение к интернету восстановлено")
			}
			s.health.Status, s.health.Error, s.health.Attempts = "online", "", 0
			health := s.health
			s.mu.Unlock()
			return health
		}
		if s.health.Attempts == 0 && previous.Error == "" {
			s.event("Нет подключения к интернету через выбранный сервер")
		}
		s.health.Error = err.Error()
		if s.health.Attempts >= maxReconnectAttempts {
			s.health.Status = "offline"
			health := s.health
			s.mu.Unlock()
			return health
		}
		s.health.Attempts++
		s.health.Status = "reconnecting"
		s.event(fmt.Sprintf("Переподключение к выбранному серверу: %d/%d", s.health.Attempts, maxReconnectAttempts))
		// Serialize the restart with configuration changes. Never switch servers or
		// enable an exit disabled by the user during a network check.
		if err := restart(ctx, s.state); err != nil && ctx.Err() == nil {
			s.event("Не удалось переподключиться: " + err.Error())
		}
		s.mu.Unlock()
		// Verify every reconnect through the proxy, including the last attempt.
	}
}
