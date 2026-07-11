// Package discord wires the bridge to Discord: a gateway bot that reads
// messages from configured channels, and per-channel webhook senders that
// post mesh messages under the originating node's name.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// WebhookSender posts messages to one Discord channel's webhook, overriding
// the displayed username per message so each MeshCore node reads as its own
// "user" in Discord.
type WebhookSender struct {
	url    string
	id     string // webhook ID, parsed from url; used for the bridge's own-echo loop guard
	client *http.Client
}

// NewWebhookSender wraps a Discord webhook URL
// (https://discord.com/api/webhooks/<id>/<token>).
func NewWebhookSender(url string) *WebhookSender {
	return &WebhookSender{
		url:    url,
		id:     webhookIDFromURL(url),
		client: &http.Client{Timeout: 10 * time.Second},
	}
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

type webhookPayload struct {
	Content   string `json:"content"`
	Username  string `json:"username,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// maxDiscordContentLen is Discord's hard limit on a single message body.
const maxDiscordContentLen = 2000

// Send posts content to this channel's webhook under username, with an
// optional avatarURL override (pass "" to use the webhook's own default
// avatar). Errors on a non-2xx response include the response body for
// diagnosability.
func (w *WebhookSender) Send(ctx context.Context, username, avatarURL, content string) error {
	if len(content) > maxDiscordContentLen {
		content = content[:maxDiscordContentLen]
	}
	body, err := json.Marshal(webhookPayload{Content: content, Username: username, AvatarURL: avatarURL})
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

	if resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("discord: webhook returned status %d: %s", resp.StatusCode, buf.String())
	}
	return nil
}
