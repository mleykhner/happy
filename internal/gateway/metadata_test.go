package gateway

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSubscriptionMetadataHeadersAndInlineFallback(t *testing.T) {
	body := "#profile-title: Inline title\n#profile-description: base64:" + base64.StdEncoding.EncodeToString([]byte("Описание\nв две строки")) + "\n#profile-web-page-url: https://inline.example\n#support-url: https://t.me/provider\n#announce: Inline notice\n" + sampleVLESS
	headers := make(http.Header)
	headers.Set("Profile-Title", "base64:"+base64.StdEncoding.EncodeToString([]byte("Провайдер")))
	headers.Set("Homepage", "https://provider.example/account")
	headers.Set("Announce", "base64:"+base64.StdEncoding.EncodeToString([]byte("Сообщение провайдера")))
	headers.Set("Subscription-Userinfo", "upload=1024; download=2048; total=8192; expire=1790951622000")
	for _, text := range []string{body, base64.StdEncoding.EncodeToString([]byte(body))} {
		m := subscriptionMetadata(headers, text)
		if m.Title != "Провайдер" || m.Description != "Описание\nв две строки" || m.Announcement != "Сообщение провайдера" || m.Website != "https://provider.example/account" || m.Support != "https://t.me/provider" {
			t.Fatalf("wrong metadata: %+v", m)
		}
		if m.Upload == nil || *m.Upload != 1024 || m.Download == nil || *m.Download != 2048 || m.Total == nil || *m.Total != 8192 || m.Expire == nil || *m.Expire != 1790951622 {
			t.Fatal("wrong traffic or expiration")
		}
		if _, err := ParseSubscription(text); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubscriptionMetadataInvalidAndMissingFields(t *testing.T) {
	for _, raw := range []string{"", "0", "upload=-1; download=no; total=9223372036854775808; expire=9007199254740991"} {
		h := make(http.Header)
		h.Set("Subscription-Userinfo", raw)
		m := subscriptionMetadata(h, "")
		if m.Upload != nil || m.Download != nil || m.Total != nil || m.Expire != nil {
			t.Fatal("invalid or missing values became valid data")
		}
	}
	h := make(http.Header)
	h.Set("Subscription-Userinfo", "upload=0; download=0; total=0; expire=0")
	m := subscriptionMetadata(h, "")
	if m.Total == nil || *m.Total != 0 || m.Expire == nil || *m.Expire != 0 {
		t.Fatal("explicit unlimited values lost")
	}
	for _, raw := range []string{"javascript:alert(1)", "data:text/html,hello", "//provider.example", "https://user:pass@provider.example", "https://", "https://provider.example\n/path"} {
		if metadataURL(raw) != "" {
			t.Fatal("unsafe URL accepted")
		}
	}
	if metadataText("base64:invalid!", 100) != "" {
		t.Fatal("invalid Base64 accepted")
	}
	if len([]rune(metadataText(strings.Repeat("я", 5000), 4096))) != 4096 {
		t.Fatal("text limit not applied")
	}
}

func TestRefreshMetadataPersistenceAndFailure(t *testing.T) {
	s, _ := fixture(t)
	if err := s.AddSubscription(t.Context(), "Provider", "https://provider.example/sub/test"); err != nil {
		t.Fatal(err)
	}
	id := s.state.Subscriptions[0].ID
	success := true
	withMetadata := true
	s.fetcher.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		if withMetadata {
			h.Set("Profile-Description", "Provider information")
			h.Set("Subscription-Userinfo", "upload=10; download=20; total=100; expire=1790951622")
		}
		status := 200
		if !success {
			status = 503
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(sampleVLESS))}, nil
	})
	if err := s.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	restored, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Subscriptions[0].Metadata.Description != "Provider information" || restored.Subscriptions[0].Metadata.Total == nil {
		t.Fatal("metadata not persisted")
	}
	view := s.View()["subscriptions"].([]any)[0].(map[string]any)
	if _, ok := view["metadata"]; !ok {
		t.Fatal("metadata absent from API")
	}
	if _, ok := view["url"]; ok {
		t.Fatal("subscription credential exposed")
	}
	success = false
	if err := s.Refresh(t.Context(), id); err == nil {
		t.Fatal("expected failed refresh")
	}
	if s.state.Subscriptions[0].Metadata.Description != "Provider information" {
		t.Fatal("failed refresh erased metadata")
	}
	success, withMetadata = true, false
	if err := s.Refresh(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if s.state.Subscriptions[0].Metadata.Description != "" || s.state.Subscriptions[0].Metadata.Total != nil {
		t.Fatal("successful refresh retained removed metadata")
	}
}
