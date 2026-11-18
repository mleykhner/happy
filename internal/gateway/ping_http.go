package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Keep the original array response for pages that were opened before the
// asynchronous API was deployed. Waiting here never creates extra workers.
func (a *API) legacyPing(w http.ResponseWriter, r *http.Request) {
	j, err := a.service.startPingJob(pingNodeDirect)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for {
		j.mu.Lock()
		status, changed := j.view.Status, j.changed
		j.mu.Unlock()
		if status != "running" {
			view, _ := j.snapshot()
			respond(w, http.StatusOK, view.Results)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			view, _ := j.snapshot()
			respond(w, http.StatusOK, view.Results)
			return // Keep the shared background task running for modern clients.
		case <-changed:
		}
	}
}

func (a *API) pingEvents(w http.ResponseWriter, r *http.Request) {
	s := a.service
	s.pingMu.Lock()
	j := s.pingJob
	if j == nil || j.view.ID != r.PathValue("id") {
		s.pingMu.Unlock()
		apiError(w, http.StatusNotFound, "задача проверки не найдена")
		return
	}
	s.pingMu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	write := func(event string, value any) error {
		// Refresh the deadline for every frame; the server's default write timeout
		// must not cut off a large job, and a slow browser must not stall a worker.
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
			return err
		}
		return controller.Flush()
	}
	view, changed := j.snapshot()
	if write("snapshot", view) != nil {
		return
	}
	cursor := view.Completed
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if view.Status != "running" {
			_ = write("done", map[string]any{"status": view.Status, "completed": view.Completed, "total": view.Total})
			return
		}
		select {
		case <-r.Context().Done():
			return // The job belongs to the service, not this HTTP connection.
		case <-ticker.C:
			cookie, err := r.Cookie("happy_session")
			if err != nil {
				return
			}
			a.mu.Lock()
			session, valid := a.sessions[tokenHash(cookie.Value)]
			a.mu.Unlock()
			if !valid || time.Now().After(session.expires) {
				return
			}
			if write("heartbeat", map[string]bool{"ok": true}) != nil {
				return
			}
		case <-changed:
			view, changed = j.updates(cursor)
			for _, result := range view.Results {
				cursor++
				if write("result", map[string]any{"result": result, "completed": cursor, "total": view.Total}) != nil {
					return
				}
			}
		}
	}
}
