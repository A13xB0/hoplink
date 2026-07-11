package discord

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// IncomingMessage is a Discord message handed to the bridge for forwarding
// to the mesh.
type IncomingMessage struct {
	ChannelID  string
	MessageID  string // this message's own ID, for replying (e.g. an oversized-message notice)
	GuildID    string // the guild (server) this message was sent in
	AuthorName string // guild nickname if set, else username
	Content    string
	WebhookID  string // non-empty if this message was itself posted by a webhook
	AuthorID   string
}

// MessageHandler receives every message seen in a channel the bot has
// access to; the bridge orchestrator filters by ChannelID and drops
// messages whose WebhookID matches one of its own webhook senders (loop
// guard) before acting on them.
type MessageHandler func(IncomingMessage)

// Bot is a Discord gateway connection used to read messages (sending is
// done separately via WebhookSender, so mesh node names can be impersonated).
type Bot struct {
	session           *discordgo.Session
	onMessage         MessageHandler
	preferDisplayName bool
}

// NewBot creates a Discord gateway session using botToken. Call OnMessage
// to register a handler, then Open to connect.
//
// preferDisplayName controls which identity is used as AuthorName when a
// message has no per-server nickname set: true resolves to the account's
// global display name (falling back to username if the account has none —
// discordgo.User.DisplayName), false uses the raw username. A per-server
// nickname, when set, always wins over either.
func NewBot(botToken string, preferDisplayName bool) (*Bot, error) {
	session, err := discordgo.New("Bot " + botToken)
	if err != nil {
		return nil, fmt.Errorf("discord: creating session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentMessageContent

	b := &Bot{session: session, preferDisplayName: preferDisplayName}
	session.AddHandler(b.handleMessageCreate)
	return b, nil
}

// OnMessage registers the handler invoked for every message the bot sees.
// Must be called before Open.
func (b *Bot) OnMessage(fn MessageHandler) {
	b.onMessage = fn
}

// Open connects to the Discord gateway.
func (b *Bot) Open() error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord: opening gateway session: %w", err)
	}
	return nil
}

// Close disconnects from the Discord gateway.
func (b *Bot) Close() error {
	return b.session.Close()
}

// SelfUserID returns this bot's own Discord user ID, valid after Open.
func (b *Bot) SelfUserID() string {
	if b.session.State == nil || b.session.State.User == nil {
		return ""
	}
	return b.session.State.User.ID
}

func (b *Bot) handleMessageCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	if b.onMessage == nil || m.Author == nil {
		return
	}
	if m.Author.ID == b.SelfUserID() {
		return // never forward the bridge's own bot messages
	}

	name := m.Author.Username
	if b.preferDisplayName {
		if m.Member != nil {
			m.Member.User = m.Author // discordgo.Member.DisplayName() reads m.User, unset on gateway events
			name = m.Member.DisplayName()
		} else {
			name = m.Author.DisplayName()
		}
	} else if m.Member != nil && m.Member.Nick != "" {
		name = m.Member.Nick
	}

	b.onMessage(IncomingMessage{
		ChannelID:  m.ChannelID,
		MessageID:  m.ID,
		GuildID:    m.GuildID,
		AuthorName: name,
		Content:    m.Content,
		WebhookID:  m.WebhookID,
		AuthorID:   m.Author.ID,
	})
}

// Reply posts content as a bot-authored reply to messageID in channelID —
// distinct from WebhookSender.Send, which impersonates a mesh node. Used
// for notices that must visibly come from hoplink itself (e.g. an
// oversized-message rejection).
func (b *Bot) Reply(channelID, messageID, content string) error {
	_, err := b.session.ChannelMessageSendReply(channelID, content, &discordgo.MessageReference{
		MessageID: messageID,
		ChannelID: channelID,
	})
	if err != nil {
		return fmt.Errorf("discord: sending reply: %w", err)
	}
	return nil
}
