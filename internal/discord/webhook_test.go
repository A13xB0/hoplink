package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

	sender := NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test")
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

	sender := NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test")
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

	sender := NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test")
	huge := strings.Repeat("x", maxDiscordContentLen+500)
	if err := sender.Send(context.Background(), "Bob", "", huge); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(gotBody.Content) != maxDiscordContentLen {
		t.Errorf("content length = %d, want %d", len(gotBody.Content), maxDiscordContentLen)
	}
}

func TestWebhookSender_Enqueue_DeliversInOrder(t *testing.T) {
	var mu sync.Mutex
	var received []string
	done := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body webhookPayload
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		received = append(received, body.Content)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		done <- struct{}{}
	}))
	defer server.Close()

	sender := NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test")
	for i := 0; i < 5; i++ {
		sender.Enqueue("Alice", "", fmt.Sprintf("message %d", i))
	}

	for i := 0; i < 5; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for message %d to be delivered", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"message 0", "message 1", "message 2", "message 3", "message 4"}
	if len(received) != len(want) {
		t.Fatalf("received %v, want %v", received, want)
	}
	for i := range want {
		if received[i] != want[i] {
			t.Errorf("received[%d] = %q, want %q (order not preserved: %v)", i, received[i], want[i], received)
		}
	}
}

func TestWebhookSender_Enqueue_RetriesAfter429(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	done := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited","retry_after":0.05}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		done <- struct{}{}
	}))
	defer server.Close()

	sender := NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test")
	sender.Enqueue("Alice", "", "hello")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the retried send to succeed")
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Errorf("attempts = %d, want exactly 2 (one 429, one success)", attempts)
	}
}

func TestWebhookSender_Enqueue_DropsWhenQueueFull(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // block the worker on the very first job
		w.WriteHeader(http.StatusNoContent)
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	sender := NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test")
	sender.Enqueue("Alice", "", "job A") // picked up immediately; blocks the worker on the server

	// Give the worker a moment to actually start the blocking request before
	// filling the buffer behind it.
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < webhookQueueSize; i++ {
		sender.Enqueue("Alice", "", fmt.Sprintf("filler %d", i))
	}

	// The queue is now full; one more Enqueue must return promptly (dropped),
	// never block waiting for room.
	enqueued := make(chan struct{})
	go func() {
		sender.Enqueue("Alice", "", "overflow")
		close(enqueued)
	}()
	select {
	case <-enqueued:
	case <-time.After(1 * time.Second):
		t.Fatal("Enqueue blocked instead of dropping when the queue was full")
	}
}

func TestWebhookSender_Send_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer server.Close()

	sender := NewWebhookSender(server.URL+"/api/webhooks/1/tok", "test")
	err := sender.Send(context.Background(), "Bob", "", "hi")
	if err == nil {
		t.Fatal("expected error for 429 response")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should include response body, got: %v", err)
	}
}
