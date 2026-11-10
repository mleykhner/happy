package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Event struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}
type Health struct {
	Checked   time.Time `json:"checked"`
	OK        bool      `json:"ok"`
	LatencyMS int64     `json:"latencyMs"`
	Error     string    `json:"error"`
}
type Service struct {
	mu        sync.Mutex
	refreshMu sync.Mutex
	store     *Store
	xray      *Xray
	state     State
	fetcher   *http.Client
	events    []Event
	health    Health
}

func NewService(store *Store, xray *Xray) (*Service, error) {
	state, err := store.load()
	if err != nil {
		return nil, err
	}
	latched, err := store.isLatched()
	if err != nil {
		return nil, err
	}
	if latched {
		state.Selected = ""
		if state.Client != nil {
			state.Client.Enabled = false
		}
		if err = store.save(state); err != nil {
			return nil, err
		}
		if err = store.clearLatch(); err != nil {
			return nil, err
		}
	}

	// Migrate existing installations once, preserving this identity thereafter.
	changed := false
	if state.DeviceID == "" {
		state.DeviceID = newDeviceID()
		changed = true
	}
	for i := range state.Subscriptions {
		clean := make([]Node, 0, len(state.Subscriptions[i].Nodes))
		for _, node := range state.Subscriptions[i].Nodes {
			if err := providerNotice(node); err != nil {
				changed = true
				state.Subscriptions[i].Error = err.Error()
				if state.Selected == node.ID {
					state.Selected = ""
				}
			} else {
				clean = append(clean, node)
			}
		}
		state.Subscriptions[i].Nodes = clean
	}
	if changed {
		if err = store.save(state); err != nil {
			return nil, err
		}
	}
	return &Service{store: store, xray: xray, state: state, events: []Event{}, fetcher: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("слишком много перенаправлений")
		}
		if via[len(via)-1].URL.Scheme == "https" && r.URL.Scheme != "https" {
			return errors.New("небезопасное перенаправление")
		}
		return validateURL(r.URL.String())
	}}}, nil
}
func (s *Service) event(message string) {
	s.events = append(s.events, Event{time.Now(), message})
	if len(s.events) > 100 {
		s.events = s.events[len(s.events)-100:]
	}
}
func cloneState(v State) State {
	b, _ := json.Marshal(v)
	var c State
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	_ = decoder.Decode(&c)
	return c
}
func (s *Service) commit(ctx context.Context, next State, forceClosed bool) error {
	old := s.state
	if forceClosed {
		if err := s.store.latchClose(); err != nil {
			s.xray.Stop()
			s.state = next
			return errors.New("не удалось сохранить запрет выхода; Xray остановлен")
		}
	}
	if err := s.xray.Apply(ctx, next, forceClosed); err != nil {
		s.event(err.Error())
		if forceClosed {
			s.xray.Stop()
			s.state = next
			if e := s.store.save(next); e != nil {
				return errors.Join(err, e)
			}
			s.state = next
			if e := s.store.clearLatch(); e != nil {
				return errors.Join(err, e)
			}
			s.health = Health{}
		}
		return err
	}
	if err := s.store.save(next); err != nil {
		if forceClosed {
			s.state = next
			s.xray.Stop()
		} else {
			_ = s.xray.Apply(ctx, old, true)
		}
		return errors.New("не удалось сохранить настройки; выход остановлен либо восстановлен")
	}
	s.state = next
	if forceClosed {
		if err := s.store.clearLatch(); err != nil {
			s.xray.Stop()
			s.state.Selected = ""
			return errors.New("не удалось снять защитный запрет; выход заблокирован")
		}
	}
	s.health = Health{}
	s.event("Конфигурация применена")
	return nil
}
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if err := s.xray.Apply(ctx, s.state, true); err != nil {
		s.event(err.Error())
	}
	s.mu.Unlock()
	go func() {
		watchdog := time.NewTicker(10 * time.Second)
		defer watchdog.Stop()
		refresh := time.NewTicker(time.Minute)
		defer refresh.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-refresh.C:
				s.refreshDue(ctx)
			case <-watchdog.C:
				s.mu.Lock()
				if !s.xray.Running() {
					s.health = Health{}
					if err := s.xray.Apply(ctx, s.state, true); err != nil {
						s.event(err.Error())
					} else {
						s.event("Xray перезапущен")
					}
				}
				s.mu.Unlock()
			}
		}
	}()
}
func (s *Service) refreshDue(ctx context.Context) {
	s.mu.Lock()
	ids := []string{}
	for _, sub := range s.state.Subscriptions {
		if time.Since(sub.Attempted) >= time.Duration(s.state.Settings.RefreshMinutes)*time.Minute {
			ids = append(ids, sub.ID)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.Refresh(ctx, id)
	}
}
func (s *Service) AddSubscription(ctx context.Context, name, raw string) error {
	if err := validateURL(raw); err != nil {
		return err
	}
	if len(name) == 0 || len(name) > 120 {
		return errors.New("имя подписки: от 1 до 120 символов")
	}
	// Persist even if the provider is temporarily unavailable, so scheduled retries work.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.Subscriptions) >= 20 {
		return errors.New("максимум 20 подписок")
	}
	for _, sub := range s.state.Subscriptions {
		if sub.URL == raw {
			return errors.New("эта подписка уже добавлена")
		}
	}
	next := cloneState(s.state)
	next.Subscriptions = append(next.Subscriptions, Subscription{ID: randomToken(), Name: name, URL: raw, Nodes: []Node{}})
	if err := s.store.save(next); err != nil {
		return err
	}
	s.state = next
	s.event("Подписка добавлена; обновите её для загрузки узлов")
	return nil
}
func (s *Service) Refresh(ctx context.Context, id string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	var raw, deviceID string
	deviceID = s.state.DeviceID
	for _, sub := range s.state.Subscriptions {
		if sub.ID == id {
			raw = sub.URL
		}
	}
	s.mu.Unlock()
	if raw == "" {
		return errors.New("подписка не найдена")
	}
	nodes, fetchErr := fetchSubscription(ctx, s.fetcher, raw, deviceID)
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	index := -1
	for i, sub := range next.Subscriptions {
		if sub.ID == id {
			index = i
		}
	}
	if index < 0 {
		return errors.New("подписка удалена")
	}
	sub := &next.Subscriptions[index]
	sub.Attempted = time.Now()
	if fetchErr != nil {
		sub.Error = fetchErr.Error()
		if err := s.store.save(next); err != nil {
			return err
		}
		s.state = next
		s.event("Не удалось обновить подписку; сохранена последняя корректная версия")
		return fetchErr
	}
	sub.Nodes = nodes
	sub.Updated = time.Now()
	sub.Error = ""
	missing := s.state.activeNode() != nil && next.activeNode() == nil
	if missing {
		next.Selected = ""
	}
	if err := s.commit(ctx, next, missing); err != nil {
		return err
	}
	s.event(fmt.Sprintf("Подписка обновлена: %d узлов", len(nodes)))
	return nil
}
func (s *Service) DeleteSubscription(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	found := false
	for i, sub := range next.Subscriptions {
		if sub.ID == id {
			next.Subscriptions = append(next.Subscriptions[:i], next.Subscriptions[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return errors.New("подписка не найдена")
	}
	missing := s.state.activeNode() != nil && next.activeNode() == nil
	if missing {
		next.Selected = ""
	}
	return s.commit(ctx, next, missing)
}
func (s *Service) Select(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	next.Selected = id
	if id != "" && next.activeNode() == nil {
		return errors.New("узел не найден")
	}
	return s.commit(ctx, next, id == "")
}
func (s *Service) Configure(ctx context.Context, v Settings) error {
	if err := v.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	next.Settings = v
	return s.commit(ctx, next, false)
}
func (s *Service) ChangeClient(ctx context.Context, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	force := false
	switch action {
	case "create", "rotate":
		if action == "create" && next.Client != nil {
			return errors.New("клиент уже создан")
		}
		if action == "rotate" && next.Client == nil {
			return errors.New("клиент ещё не создан")
		}
		priv, pub, err := keyPair()
		if err != nil {
			return err
		}
		next.Client = &Client{PrivateKey: priv, PublicKey: pub, Enabled: true}
	case "enable", "disable":
		if next.Client == nil {
			return errors.New("клиент ещё не создан")
		}
		next.Client.Enabled = action == "enable"
		force = action == "disable"
	default:
		return errors.New("неизвестное действие")
	}
	return s.commit(ctx, next, force)
}
func (s *Service) Probe(ctx context.Context) Health {
	// Serialize with configuration changes so a probe cannot report a previous exit as healthy.
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.xray.Probe(ctx)
	s.health = Health{Checked: time.Now(), OK: err == nil, LatencyMS: d.Milliseconds()}
	if err != nil {
		s.health.Error = err.Error()
	}
	return s.health
}
func (s *Service) View() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := []any{}
	for _, sub := range s.state.Subscriptions {
		nodes := []any{}
		for _, n := range sub.Nodes {
			nodes = append(nodes, map[string]any{"id": n.ID, "name": n.Name, "protocol": n.Protocol, "host": n.Host, "port": n.Port})
		}
		subs = append(subs, map[string]any{"id": sub.ID, "name": sub.Name, "nodes": nodes, "updated": sub.Updated, "error": sub.Error})
	}
	var client any
	if s.state.Client != nil {
		client = map[string]any{"enabled": s.state.Client.Enabled, "publicKey": s.state.Client.PublicKey}
	}
	return map[string]any{"deviceId": s.state.DeviceID, "settings": s.state.Settings, "subscriptions": subs, "selected": s.state.Selected, "client": client, "running": s.xray.Running(), "blocked": s.state.activeNode() == nil || !s.xray.Running() || s.state.Client == nil || !s.state.Client.Enabled, "health": s.health, "events": append([]Event{}, s.events...)}
}
func (s *Service) ClientConfig() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.clientConfig()
}
