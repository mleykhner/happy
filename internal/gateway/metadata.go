package gateway

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SubscriptionMetadata is provider-supplied information, never local usage or billing history.
type SubscriptionMetadata struct {
	Title           string `json:"title,omitempty"`
	Description     string `json:"description,omitempty"`
	Announcement    string `json:"announcement,omitempty"`
	Website         string `json:"website,omitempty"`
	Support         string `json:"support,omitempty"`
	AnnouncementURL string `json:"announcementUrl,omitempty"`
	Upload          *int64 `json:"upload,omitempty"`
	Download        *int64 `json:"download,omitempty"`
	Total           *int64 `json:"total,omitempty"`
	Expire          *int64 `json:"expire,omitempty"`
}

func subscriptionMetadata(headers http.Header, body string) SubscriptionMetadata {
	inline := make(http.Header)
	body = strings.TrimSpace(strings.TrimPrefix(body, "\ufeff"))
	if !strings.Contains(body, "://") {
		if decoded, err := decode64(body); err == nil {
			body = string(decoded)
		}
	}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "#"), ":")
		if ok && len(value) <= 16384 {
			inline.Set(strings.TrimSpace(key), strings.TrimSpace(value))
		}
	}
	get := func(keys ...string) string {
		// All HTTP headers take priority over inline fallback, including aliases.
		for _, source := range []http.Header{headers, inline} {
			for _, key := range keys {
				if values, ok := source[http.CanonicalHeaderKey(key)]; ok && len(values) > 0 {
					return values[0]
				}
			}
		}
		return ""
	}
	m := SubscriptionMetadata{
		Title:           metadataText(get("profile-title", "subscription-name"), 200),
		Description:     metadataText(get("profile-description"), 4096),
		Announcement:    metadataText(get("announce"), 4096),
		Website:         metadataURL(get("profile-web-page-url", "homepage")),
		Support:         metadataURL(get("support-url")),
		AnnouncementURL: metadataURL(get("announce-url")),
	}
	for field := range strings.SplitSeq(get("subscription-userinfo"), ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		// Keep values exactly representable in the web UI, and ignore malformed optional fields.
		if err != nil || n < 0 || n > 9007199254740991 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "upload":
			m.Upload = &n
		case "download":
			m.Download = &n
		case "total":
			m.Total = &n
		case "expire":
			if n > 32000000000 {
				n /= 1000
			}
			if n <= 253402300799 {
				m.Expire = &n
			}
		}
	}
	return m
}

func metadataText(raw string, limit int) string {
	if len(raw) > 16384 {
		return ""
	}
	if encoded, ok := strings.CutPrefix(raw, "base64:"); ok {
		b, err := decode64(encoded)
		if err != nil {
			return ""
		}
		raw = string(b)
	}
	if !utf8.ValidString(raw) {
		return ""
	}
	raw = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, raw)
	runes := []rune(strings.TrimSpace(raw))
	return string(runes[:min(len(runes), limit)])
}

func metadataURL(raw string) string {
	if strings.ContainsAny(raw, "\r\n\t") {
		return ""
	}
	raw = metadataText(raw, 4096)
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}
