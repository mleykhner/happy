package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const maxSubscription = 4 << 20

var errUnsupportedNodeProtocol = errors.New("поддерживаются VLESS, VMess, Trojan и Shadowsocks")

func decode64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := e.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("некорректный Base64")
}
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("нужен HTTP(S) URL подписки без userinfo и фрагмента")
	}
	return nil
}
func fetchSubscription(ctx context.Context, c *http.Client, raw, deviceID string) (nodes []Node, metadata SubscriptionMetadata, fetchErr error) {
	started := time.Now()
	stage := "validate"
	// The URL path/query and HWID can contain credentials or identify a device.
	// Log fingerprints so attempts can be compared without exposing these values.
	id := identity(raw)
	defer func() {
		if fetchErr != nil {
			log.Printf("subscription.fetch failed subscription=%q stage=%q duration_ms=%d error=%q", id, stage, time.Since(started).Milliseconds(), fetchErr)
		} else {
			log.Printf("subscription.fetch completed subscription=%q duration_ms=%d nodes=%d", id, time.Since(started).Milliseconds(), len(nodes))
		}
	}()
	if err := validateURL(raw); err != nil {
		return nil, SubscriptionMetadata{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, SubscriptionMetadata{}, errors.New("не удалось создать запрос")
	}
	req.Header.Set("User-Agent", "Happy/0.1")
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("X-Hwid", deviceID)
	req.Header.Set("X-Device-Os", runtime.GOOS)
	req.Header.Set("X-Device-Model", "Happy Gateway ("+runtime.GOARCH+")")
	req.Header.Set("X-App-Version", "0.1.0")
	log.Printf("subscription.fetch started subscription=%q host=%q user_agent=%q device_os=%q device_model=%q app_version=%q hwid_present=%t hwid_fingerprint=%q", id, req.URL.Host, req.UserAgent(), req.Header.Get("X-Device-Os"), req.Header.Get("X-Device-Model"), req.Header.Get("X-App-Version"), deviceID != "", identity(deviceID))
	stage = "request"
	resp, err := c.Do(req)
	if err != nil {
		// http.Client errors include the secret subscription URL. Record only
		// the underlying error type and timeout/cancellation classification.
		cause := err
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			cause = requestErr.Err
		}
		log.Printf("subscription.fetch request_error subscription=%q error_type=%T timeout=%t canceled=%t", id, cause, errors.As(err, &requestErr) && requestErr.Timeout(), errors.Is(err, context.Canceled))
		return nil, SubscriptionMetadata{}, errors.New("не удалось загрузить подписку: проверьте адрес и соединение")
	}
	defer resp.Body.Close()
	finalHost := req.URL.Host
	redirected := false
	if resp.Request != nil && resp.Request.URL != nil {
		finalHost = resp.Request.URL.Host
		redirected = resp.Request.URL.String() != req.URL.String()
	}
	log.Printf("subscription.fetch response subscription=%q host=%q redirected=%t status=%d protocol=%q duration_ms=%d content_type=%q content_length=%d server=%q cf_ray=%q hwid_active=%t hwid_not_supported=%t hwid_max_devices_reached=%t", id, finalHost, redirected, resp.StatusCode, resp.Proto, time.Since(started).Milliseconds(), resp.Header.Get("Content-Type"), resp.ContentLength, resp.Header.Get("Server"), resp.Header.Get("Cf-Ray"), headerEnabled(resp.Header.Get("X-Hwid-Active")), headerEnabled(resp.Header.Get("X-Hwid-Not-Supported")), headerEnabled(resp.Header.Get("X-Hwid-Max-Devices-Reached")))
	stage = "response"
	if headerEnabled(resp.Header.Get("X-Hwid-Max-Devices-Reached")) {
		return nil, SubscriptionMetadata{}, errors.New("достигнут лимит устройств подписки; удалите неиспользуемое устройство в кабинете провайдера")
	}
	if headerEnabled(resp.Header.Get("X-Hwid-Not-Supported")) {
		return nil, SubscriptionMetadata{}, errors.New("провайдер не принял HWID шлюза; проверьте поддержку Happy у провайдера")
	}
	if resp.StatusCode == http.StatusNotFound && headerEnabled(resp.Header.Get("X-Hwid-Active")) {
		return nil, SubscriptionMetadata{}, errors.New("провайдер отказал HWID шлюза; проверьте доступ устройства в кабинете провайдера")
	}
	if resp.StatusCode != 200 {
		return nil, SubscriptionMetadata{}, fmt.Errorf("провайдер ответил HTTP %d", resp.StatusCode)
	}
	stage = "read"
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscription+1))
	if err != nil {
		return nil, SubscriptionMetadata{}, errors.New("ошибка чтения подписки")
	}
	if len(b) > maxSubscription {
		return nil, SubscriptionMetadata{}, errors.New("подписка превышает 4 МиБ")
	}
	stage = "parse"
	nodes, err = ParseSubscription(string(b))
	if err != nil {
		return nil, SubscriptionMetadata{}, err
	}
	return nodes, subscriptionMetadata(resp.Header, string(b)), nil
}
func ParseSubscription(body string) ([]Node, error) {
	body = strings.TrimSpace(strings.TrimPrefix(body, "\ufeff"))
	if len(body) > maxSubscription {
		return nil, errors.New("подписка превышает 4 МиБ")
	}
	if wireguardDocument(body) {
		return nil, errors.New("WireGuard/AmneziaWG в подписке пока не поддерживается")
	}
	if !strings.Contains(body, "://") && !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
		b, err := decode64(body)
		if err != nil {
			return nil, errors.New("ожидается список ссылок или Base64-подписка")
		}
		body = strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	}
	if wireguardDocument(body) {
		return nil, errors.New("WireGuard/AmneziaWG в подписке пока не поддерживается")
	}
	if strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[") {
		return parseJSONSubscription(body)
	}
	nodes := []Node{}
	seen := map[string]bool{}
	lineNumber, skipped := 0, 0
	defer func() {
		if skipped > 0 {
			log.Printf("subscription.parse unsupported_skipped count=%d", skipped)
		}
	}()
	for line := range strings.SplitSeq(body, "\n") {
		lineNumber++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if subscriptionRoutingDirective(line) {
			continue
		}
		n, err := parseNode(line)
		if errors.Is(err, errUnsupportedNodeProtocol) {
			skipped++
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("строка %d: %w", lineNumber, err)
		}
		if err := providerNotice(n); err != nil {
			return nil, err
		}
		if !seen[n.ID] {
			if len(nodes) >= 2000 {
				return nil, errors.New("слишком много узлов")
			}
			nodes = append(nodes, n)
			seen[n.ID] = true
		}
	}
	if len(nodes) == 0 {
		return nil, errors.New("в подписке нет поддерживаемых узлов")
	}
	return nodes, nil
}
func parseNode(raw string) (Node, error) {
	if strings.HasPrefix(raw, "vmess://") {
		return parseVMess(raw)
	}
	// Legacy Shadowsocks encodes method:password@host:port as one Base64 payload.
	if strings.HasPrefix(raw, "ss://") && !strings.Contains(strings.SplitN(strings.TrimPrefix(raw, "ss://"), "#", 2)[0], "@") {
		payload, label, _ := strings.Cut(strings.TrimPrefix(raw, "ss://"), "#")
		decoded, e := decode64(strings.TrimSuffix(payload, "/"))
		if e != nil {
			return Node{}, errors.New("некорректный Shadowsocks Base64")
		}
		credentials, host, ok := strings.Cut(string(decoded), "@")
		if !ok {
			return Node{}, errors.New("нет адреса Shadowsocks")
		}
		method, password, ok := strings.Cut(credentials, ":")
		if !ok {
			return Node{}, errors.New("нет пароля Shadowsocks")
		}
		raw = "ss://" + url.UserPassword(method, password).String() + "@" + host
		if label != "" {
			raw += "#" + label
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Node{}, errors.New("некорректная ссылка узла")
	}
	if u.Scheme != "vless" && u.Scheme != "trojan" && u.Scheme != "ss" {
		if u.Scheme == "" {
			return Node{}, errors.New("некорректная ссылка узла")
		}
		return Node{}, errUnsupportedNodeProtocol
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Hostname() == "" {
		return Node{}, errors.New("некорректный адрес или порт узла")
	}
	n := Node{ID: identity(raw), Name: u.Fragment, Protocol: u.Scheme, Host: u.Hostname(), Port: port}
	if n.Name == "" {
		n.Name = n.Host
	}
	if u.User == nil {
		return Node{}, errors.New("нет данных авторизации узла")
	}
	q := u.Query()
	settings := map[string]any{}
	switch u.Scheme {
	case "vless":
		settings = map[string]any{"vnext": []any{map[string]any{"address": n.Host, "port": port, "users": []any{map[string]any{"id": u.User.Username(), "encryption": value(q.Get("encryption"), "none"), "flow": q.Get("flow")}}}}}
	case "trojan":
		settings = map[string]any{"servers": []any{map[string]any{"address": n.Host, "port": port, "password": u.User.Username()}}}
	case "ss":
		method, password, ok := strings.Cut(u.User.String(), ":")
		if ok {
			method = u.User.Username()
			password, _ = u.User.Password()
		} else {
			b, e := decode64(u.User.Username())
			if e != nil {
				return Node{}, errors.New("некорректные данные Shadowsocks")
			}
			method, password, ok = strings.Cut(string(b), ":")
		}
		if !ok || method == "" || password == "" {
			return Node{}, errors.New("нет метода или пароля Shadowsocks")
		}
		if q.Get("plugin") != "" {
			return Node{}, errors.New("плагины Shadowsocks пока не поддерживаются")
		}
		settings = map[string]any{"servers": []any{map[string]any{"address": n.Host, "port": port, "method": method, "password": password}}}
	}
	stream, err := streamSettings(q, u.Scheme)
	if err != nil {
		return Node{}, err
	}
	coreProtocol := n.Protocol
	if coreProtocol == "ss" {
		coreProtocol = "shadowsocks"
	}
	n.Outbound = map[string]any{"tag": "proxy", "protocol": coreProtocol, "settings": settings, "streamSettings": stream}
	return n, nil
}
func value(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
func streamSettings(q url.Values, protocol string) (map[string]any, error) {
	network := value(q.Get("type"), "tcp")
	security := value(q.Get("security"), "none")
	if protocol == "trojan" && q.Get("security") == "" {
		security = "tls"
	}
	s := map[string]any{"network": network, "security": security}
	switch network {
	case "tcp", "raw":
		if q.Get("headerType") != "" && q.Get("headerType") != "none" {
			return nil, errors.New("TCP headerType не поддерживается")
		}
	case "ws":
		s["wsSettings"] = map[string]any{"path": value(q.Get("path"), "/"), "headers": map[string]any{"Host": q.Get("host")}}
	case "grpc":
		s["grpcSettings"] = map[string]any{"serviceName": q.Get("serviceName"), "multiMode": q.Get("mode") == "multi"}
	case "xhttp":
		settings := map[string]any{"host": q.Get("host"), "path": value(q.Get("path"), "/"), "mode": value(q.Get("mode"), "auto")}
		if q.Has("extra") {
			// url.Values has already URL-decoded the share-link parameter. Preserve
			// its JSON verbatim, including nested downloadSettings, for Xray to validate.
			raw := strings.TrimSpace(q.Get("extra"))
			if len(raw) > 256<<10 {
				return nil, errors.New("XHTTP extra превышает 256 КиБ")
			}
			if len(raw) == 0 || raw[0] != '{' || !json.Valid([]byte(raw)) {
				return nil, errors.New("XHTTP extra должен содержать корректный JSON-объект")
			}
			settings["extra"] = json.RawMessage(raw)
		}
		s["xhttpSettings"] = settings
	case "httpupgrade":
		s["httpupgradeSettings"] = map[string]any{"host": q.Get("host"), "path": value(q.Get("path"), "/")}
	default:
		return nil, errors.New("неподдерживаемый транспорт узла")
	}
	switch security {
	case "none":
	case "tls":
		if q.Get("allowInsecure") == "1" || q.Get("allowInsecure") == "true" {
			return nil, errors.New("небезопасный TLS запрещён")
		}
		tls := map[string]any{"serverName": value(q.Get("sni"), q.Get("peer")), "fingerprint": value(q.Get("fp"), "chrome")}
		if q.Get("alpn") != "" {
			tls["alpn"] = strings.Split(q.Get("alpn"), ",")
		}
		s["tlsSettings"] = tls
	case "reality":
		if q.Get("pbk") == "" {
			return nil, errors.New("нет публичного ключа REALITY")
		}
		s["realitySettings"] = map[string]any{"serverName": q.Get("sni"), "fingerprint": value(q.Get("fp"), "chrome"), "publicKey": q.Get("pbk"), "shortId": q.Get("sid"), "spiderX": q.Get("spx")}
	default:
		return nil, errors.New("неподдерживаемая защита транспорта")
	}
	return s, nil
}
func parseVMess(raw string) (Node, error) {
	b, err := decode64(strings.TrimPrefix(raw, "vmess://"))
	if err != nil {
		return Node{}, err
	}
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		return Node{}, errors.New("некорректный VMess JSON")
	}
	str := func(k string) string {
		if v[k] == nil {
			return ""
		}
		return fmt.Sprint(v[k])
	}
	port, err := strconv.Atoi(str("port"))
	if err != nil || port < 1 || port > 65535 || str("add") == "" || str("id") == "" {
		return Node{}, errors.New("некорректный VMess адрес или ID")
	}
	aid := 0
	if str("aid") != "" {
		aid, err = strconv.Atoi(str("aid"))
		if err != nil {
			return Node{}, errors.New("некорректный VMess alterId")
		}
	}
	q := url.Values{"type": {value(str("net"), "tcp")}, "security": {value(str("tls"), "none")}, "sni": {str("sni")}, "host": {str("host")}, "path": {str("path")}, "alpn": {str("alpn")}, "fp": {str("fp")}}
	if str("net") == "grpc" {
		q.Set("serviceName", str("path"))
	}
	if str("type") != "" && str("type") != "none" && str("net") == "tcp" {
		q.Set("headerType", str("type"))
	}
	stream, err := streamSettings(q, "vmess")
	if err != nil {
		return Node{}, err
	}
	n := Node{ID: identity(raw), Name: value(str("ps"), str("add")), Protocol: "vmess", Host: str("add"), Port: port}
	n.Outbound = map[string]any{"tag": "proxy", "protocol": "vmess", "settings": map[string]any{"vnext": []any{map[string]any{"address": n.Host, "port": port, "users": []any{map[string]any{"id": str("id"), "alterId": aid, "security": value(str("scy"), "auto")}}}}}, "streamSettings": stream}
	return n, nil
}

func headerEnabled(s string) bool { return s == "1" || strings.EqualFold(s, "true") }
func providerNotice(n Node) error {
	name := strings.ToLower(n.Name)
	if strings.Contains(name, "приложение не поддерживает hwid") {
		return errors.New("провайдер сообщает, что HWID не принят; это служебное сообщение, а не VPN-узел")
	}
	if strings.Contains(name, "используйте happ или incy") {
		return errors.New("провайдер разрешает только Happ или INCY; запросите поддержку Happy у провайдера")
	}
	return nil
}
