package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookIDFromURL(t *testing.T) {
	got := webhookIDFromURL("https://discord.com/api/webhooks/123456789/some-token-here")
	if got != "123456789" {
		t.Errorf("webhookIDFromURL = %q, want %q", got, "123456789")
	}
}

func TestWebhookIDFromURL_TrailingSlash(t *testing.T) {
	got := webhookIDFromURL("https://discord.com/api/webhooks/123456789/token/")
	if got != "123456789" {
		t.Errorf("webhookIDFromURL = %q, want %q", got, "123456789")
	}
}

func TestWebhookIDFromURL_Malformed(t *testing.T) {
	if got := webhookIDFromURL("not-a-webhook-url"); got != "" {
		t.Errorf("webhookIDFromURL = %q, want empty", got)
	}
}

func TestWebhookSender_Send(t *testing.T) {
	var gotBody webhookPayload
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := NewWebhookSender(server.URL + "/api/webhooks/1/tok")
	if err := sender.Send(context.Background(), "Alice", "https://example.com/avatar.png", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotBody.Username != "Alice" || gotBody.Content != "hello" {
		t.Errorf("got payload %+v", gotBody)
	}
	if gotBody.AvatarURL != "https://example.com/avatar.png" {
		t.Errorf("AvatarURL = %q, want %q", gotBody.AvatarURL, "https://example.com/avatar.png")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if sender.ID() != "1" {
		t.Errorf("ID() = %q, want %q", sender.ID(), "1")
	}
}

func TestWebhookSender_Send_SuppressesMentions(t *testing.T) {
	var rawBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		rawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := NewWebhookSender(server.URL + "/api/webhooks/1/tok")
	if err := sender.Send(context.Background(), "Alice", "", "@everyone @here <@&123> <@456> hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !strings.Contains(string(rawBody), `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("expected an empty allowed_mentions.parse to suppress all mentions, got body: %s", rawBody)
	}

	var gotBody webhookPayload
	if err := json.Unmarshal(rawBody, &gotBody); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if gotBody.AllowedMentions.Parse == nil {
		t.Error("AllowedMentions.Parse must be a non-nil empty slice, not nil/omitted")
	}
	if len(gotBody.AllowedMentions.Parse) != 0 {
		t.Errorf("AllowedMentions.Parse = %v, want empty", gotBody.AllowedMentions.Parse)
	}
}

func TestWebhookSender_Send_TruncatesOversizedContent(t *testing.T) {
	var gotBody webhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender := NewWebhookSender(server.URL + "/api/webhooks/1/tok")
	huge := strings.Repeat("x", maxDiscordContentLen+500)
	if err := sender.Send(context.Background(), "Bob", "", huge); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(gotBody.Content) != maxDiscordContentLen {
		t.Errorf("content length = %d, want %d", len(gotBody.Content), maxDiscordContentLen)
	}
}

func TestWebhookSender_Send_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer server.Close()

	sender := NewWebhookSender(server.URL + "/api/webhooks/1/tok")
	err := sender.Send(context.Background(), "Bob", "", "hi")
	if err == nil {
		t.Fatal("expected error for 429 response")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should include response body, got: %v", err)
	}
}
