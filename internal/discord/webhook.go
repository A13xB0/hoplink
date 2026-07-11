// Package discord wires the bridge to Discord: a gateway bot that reads
// messages from configured channels, and per-channel webhook senders that
// post mesh messages under the originating node's name.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func logf(format string, args ...any) {
	log.Printf("[discord] "+format, args...)
}

// webhookQueueSize bounds how many not-yet-sent posts a single webhook can
// have queued before Enqueue starts dropping the newest ones. Sized well
// above any realistic burst (chunked oversized messages, a flurry of sibling
// relays) while still catching a truly stuck webhook rather than growing
// memory without bound.
const webhookQueueSize = 64

// maxWebhookRetries bounds how many times sendWithRetry will retry a single
// message after a 429, so one persistently rate-limited webhook can't starve
// the process retrying forever.
const maxWebhookRetries = 3

// maxWebhookRetryWait caps how long a single retry ever waits, regardless of
// what Discord's retry_after says, so a misbehaving/huge value can't stall
// this webhook's queue indefinitely.
const maxWebhookRetryWait = 30 * time.Second

// WebhookSender posts messages to one Discord channel's webhook, overriding
// the displayed username per message so each MeshCore/Meshtastic node (or a
// sibling bridge's Discord user) reads as its own "user" in Discord.
//
// Posts are queued and delivered one at a time, in order, by a single
// worker goroutine per webhook — rather than one goroutine per message —
// so concurrent callers (inbound mesh traffic, sibling-bridge relay) can't
// reorder delivery, and a 429 response pauses only this webhook's queue for
// the required wait instead of being silently dropped.
type WebhookSender struct {
	url    string
	id     string // webhook ID, parsed from url; used for the bridge's own-echo loop guard
	name   string // bridge name, for log attribution
	client *http.Client
	queue  chan webhookJob
}

type webhookJob struct {
	username, avatarURL, content string
}

// NewWebhookSender wraps a Discord webhook URL
// (https://discord.com/api/webhooks/<id>/<token>) for bridge name (used only
// for log attribution) and starts its delivery worker. The worker runs for
// the lifetime of the process, same as the Bridge that owns it.
func NewWebhookSender(url, name string) *WebhookSender {
	w := &WebhookSender{
		url:    url,
		id:     webhookIDFromURL(url),
		name:   name,
		client: &http.Client{Timeout: 10 * time.Second},
		queue:  make(chan webhookJob, webhookQueueSize),
	}
	go w.worker()
	return w
}

// ID returns the webhook's snowflake ID (parsed from the URL), or "" if it
// couldn't be parsed. Compare against discordgo.MessageCreate.WebhookID to
// recognise — and ignore — messages this bridge itself posted.
func (w *WebhookSender) ID() string {
	return w.id
}

func webhookIDFromURL(url string) string {
	parts := strings.Split(strings.TrimRight(url, "/"), "/")
	for i, p := range parts {
		if p == "webhooks" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// Enqueue queues content for delivery under username/avatarURL, preserving
// send order relative to every other Enqueue call on this same webhook. If
// the queue is full (this webhook is badly backed up — persistently rate
// limited, or Discord is down), the message is dropped rather than growing
// memory without bound.
func (w *WebhookSender) Enqueue(username, avatarURL, content string) {
	select {
	case w.queue <- webhookJob{username: username, avatarURL: avatarURL, content: content}:
	default:
		logf("webhook %q: queue full, dropping message", w.name)
	}
}

// worker delivers queued jobs one at a time, in order. A 429 pauses this
// worker (and so this webhook's queue) for the required wait before
// retrying — other webhooks' workers are unaffected.
func (w *WebhookSender) worker() {
	for job := range w.queue {
		w.sendWithRetry(job)
	}
}

func (w *WebhookSender) sendWithRetry(job webhookJob) {
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := w.Send(ctx, job.username, job.avatarURL, job.content)
		cancel()
		if err == nil {
			return
		}

		var rl *rateLimitError
		if !errors.As(err, &rl) || attempt >= maxWebhookRetries {
			logf("posting to webhook %q: %v", w.name, err)
			return
		}
		wait := rl.RetryAfter
		if wait > maxWebhookRetryWait {
			wait = maxWebhookRetryWait
		}
		logf("webhook %q: rate limited, retrying in %s (attempt %d/%d)", w.name, wait, attempt+1, maxWebhookRetries)
		time.Sleep(wait)
	}
}

type webhookPayload struct {
	Content         string          `json:"content"`
	Username        string          `json:"username,omitempty"`
	AvatarURL       string          `json:"avatar_url,omitempty"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

// allowedMentions is always sent with an empty Parse list. Relayed content
// originates from Discord users, mesh node names, or (via sibling-bridge
// relay) another Discord channel entirely — none of which should be able to
// ping @everyone, @here, a role, or an arbitrary user just by typing or
// naming themselves that. Discord webhooks honor those mentions by default
// unless explicitly suppressed.
type allowedMentions struct {
	// Parse must be a non-nil empty slice (marshals to "[]", not "null") —
	// an absent/null allowed_mentions falls back to Discord's default
	// (everything pings).
	Parse []string `json:"parse"`
}

// maxDiscordContentLen is Discord's hard limit on a single message body.
const maxDiscordContentLen = 2000

// rateLimitError is returned by Send when Discord responds 429, carrying how
// long to wait before retrying (see sendWithRetry).
type rateLimitError struct {
	RetryAfter time.Duration
	body       string
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s: %s", e.RetryAfter, e.body)
}

// Send posts content to this channel's webhook under username, with an
// optional avatarURL override (pass "" to use the webhook's own default
// avatar). @everyone/@here/role/user mentions in content are always
// suppressed (see allowedMentions) — they show up as inert plain text, never
// as a real ping. This is the single-attempt, single-POST primitive; callers
// wanting ordering and 429 retry across many messages should use Enqueue
// instead. A 429 response returns a *rateLimitError; other non-2xx
// responses include the response body for diagnosability.
func (w *WebhookSender) Send(ctx context.Context, username, avatarURL, content string) error {
	if len(content) > maxDiscordContentLen {
		content = content[:maxDiscordContentLen]
	}
	body, err := json.Marshal(webhookPayload{
		Content:         content,
		Username:        username,
		AvatarURL:       avatarURL,
		AllowedMentions: allowedMentions{Parse: []string{}},
	})
	if err != nil {
		return fmt.Errorf("discord: marshalling webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("discord: building webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord: webhook request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return &rateLimitError{RetryAfter: parseRetryAfter(resp.Header, buf.Bytes()), body: buf.String()}
	}
	if resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("discord: webhook returned status %d: %s", resp.StatusCode, buf.String())
	}
	return nil
}

// parseRetryAfter extracts how long to wait before retrying a 429: Discord's
// webhook execute endpoint reports this as a fractional-seconds retry_after
// field in the JSON body, falling back to the standard Retry-After header,
// falling back to a conservative default if neither parses.
func parseRetryAfter(header http.Header, body []byte) time.Duration {
	var parsed struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.RetryAfter > 0 {
		return time.Duration(parsed.RetryAfter * float64(time.Second))
	}
	if h := header.Get("Retry-After"); h != "" {
		if secs, err := strconv.ParseFloat(h, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return time.Second
}
