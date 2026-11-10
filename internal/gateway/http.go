package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

type session struct{ expires time.Time }
type loginLimit struct {
	count int
	since time.Time
}
type API struct {
	service  *Service
	hash     []byte
	origin   string
	secure   bool
	mu       sync.Mutex
	sessions map[string]session
	limits   map[string]loginLimit
}

func Handler(s *Service, assets fs.FS, origin string, secure bool) (http.Handler, error) {
	hash, err := s.store.password()
	if err != nil {
		return nil, errors.New("администратор не настроен: выполните happy set-password --password-file FILE")
	}
	if origin != "" {
		u, e := url.Parse(origin)
		if e != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" {
			return nil, errors.New("HAPPY_ORIGIN должен содержать только схему и хост")
		}
	}
	if strings.HasPrefix(origin, "https://") {
		secure = true
	}
	a := &API{service: s, hash: hash, origin: origin, secure: secure, sessions: map[string]session{}, limits: map[string]loginLimit{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.auth(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		if c, e := r.Cookie("happy_session"); e == nil {
			delete(a.sessions, tokenHash(c.Value))
		}
		a.mu.Unlock()
		http.SetCookie(w, a.cookie("", -1))
		respond(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /api/state", a.auth(func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.View()) }))
	mux.HandleFunc("POST /api/subscriptions", a.auth(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if !decode(w, r, &v) {
			return
		}
		a.result(w, s.AddSubscription(r.Context(), v.Name, v.URL))
	}))
	mux.HandleFunc("POST /api/subscriptions/{id}/refresh", a.auth(func(w http.ResponseWriter, r *http.Request) { a.result(w, s.Refresh(r.Context(), r.PathValue("id"))) }))
	mux.HandleFunc("DELETE /api/subscriptions/{id}", a.auth(func(w http.ResponseWriter, r *http.Request) {
		a.result(w, s.DeleteSubscription(r.Context(), r.PathValue("id")))
	}))
	mux.HandleFunc("POST /api/select", a.auth(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &v) {
			return
		}
		a.result(w, s.Select(r.Context(), v.ID))
	}))
	mux.HandleFunc("PUT /api/settings", a.auth(func(w http.ResponseWriter, r *http.Request) {
		var v Settings
		if !decode(w, r, &v) {
			return
		}
		a.result(w, s.Configure(r.Context(), v))
	}))
	mux.HandleFunc("POST /api/client/{action}", a.auth(func(w http.ResponseWriter, r *http.Request) {
		a.result(w, s.ChangeClient(r.Context(), r.PathValue("action")))
	}))
	mux.HandleFunc("GET /api/client/config", a.auth(func(w http.ResponseWriter, r *http.Request) {
		config, e := s.ClientConfig()
		if e != nil {
			apiError(w, 400, e.Error())
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="happy.conf"`)
		_, _ = w.Write([]byte(config))
	}))
	mux.HandleFunc("GET /api/client/qr", a.auth(func(w http.ResponseWriter, r *http.Request) {
		config, e := s.ClientConfig()
		if e != nil {
			apiError(w, 400, e.Error())
			return
		}
		png, e := qrcode.Encode(config, qrcode.Medium, 384)
		if e != nil {
			apiError(w, 500, "не удалось создать QR")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	mux.HandleFunc("POST /api/probe", a.auth(func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.Probe(r.Context())) }))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { apiError(w, 404, "метод API не найден") })
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.FileServer(http.FS(assets)).ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" {
			expected := a.origin
			if expected == "" {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				expected = scheme + "://" + r.Host
			}
			if r.Header.Get("Origin") != expected {
				apiError(w, 403, "недопустимый Origin")
				return
			}
		}
		mux.ServeHTTP(w, r)
	}), nil
}
func tokenHash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func (a *API) cookie(v string, age int) *http.Cookie {
	return &http.Cookie{Name: "happy_session", Value: v, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: age}
}
func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("happy_session")
		if err != nil {
			apiError(w, 401, "нужен вход администратора")
			return
		}
		a.mu.Lock()
		v, ok := a.sessions[tokenHash(c.Value)]
		if ok && time.Now().After(v.expires) {
			delete(a.sessions, tokenHash(c.Value))
			ok = false
		}
		a.mu.Unlock()
		if !ok {
			apiError(w, 401, "сессия истекла")
			return
		}
		next(w, r)
	}
}
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	a.mu.Lock()
	for k, v := range a.limits {
		if now.Sub(v.since) > 15*time.Minute {
			delete(a.limits, k)
		}
	}
	limit := a.limits[host]
	if limit.since.IsZero() {
		limit.since = now
	}
	limit.count++
	if len(a.limits) >= 10000 || limit.count > 10 {
		a.mu.Unlock()
		apiError(w, 429, "слишком много попыток; повторите через 15 минут")
		return
	}
	a.limits[host] = limit
	a.mu.Unlock()
	var v struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &v) {
		return
	}
	if len(v.Password) > 72 || bcrypt.CompareHashAndPassword(a.hash, []byte(v.Password)) != nil {
		apiError(w, 401, "неверный пароль")
		return
	}
	token := randomToken()
	a.mu.Lock()
	for k, v := range a.sessions {
		if now.After(v.expires) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) >= 32 {
		clear(a.sessions)
	}
	a.sessions[tokenHash(token)] = session{now.Add(12 * time.Hour)}
	delete(a.limits, host)
	a.mu.Unlock()
	http.SetCookie(w, a.cookie(token, 43200))
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *API) result(w http.ResponseWriter, err error) {
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		apiError(w, 400, "некорректный JSON")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err == nil || !errors.Is(err, io.EOF) {
		apiError(w, 400, "ожидается один JSON объект")
		return false
	}
	return true
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"error": msg})
}
