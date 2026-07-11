package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func newTestBot(t *testing.T, preferDisplayName bool) *Bot {
	t.Helper()
	b, err := NewBot("fake-token-not-used", preferDisplayName)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	b.session.State.User = &discordgo.User{ID: "bot-id"}
	return b
}

func TestBot_HandleMessageCreate_UsesNicknameWhenPresent(t *testing.T) {
	b := newTestBot(t, false)

	var got IncomingMessage
	b.OnMessage(func(m IncomingMessage) { got = m })

	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "chan-1",
		Content:   "hello mesh",
		Author:    &discordgo.User{ID: "user-1", Username: "someuser"},
		Member:    &discordgo.Member{Nick: "CoolNick"},
	}})

	if got.AuthorName != "CoolNick" {
		t.Errorf("AuthorName = %q, want %q", got.AuthorName, "CoolNick")
	}
	if got.ChannelID != "chan-1" || got.Content != "hello mesh" {
		t.Errorf("got %+v", got)
	}
}

func TestBot_HandleMessageCreate_PropagatesMessageIDAndGuildID(t *testing.T) {
	b := newTestBot(t, false)

	var got IncomingMessage
	b.OnMessage(func(m IncomingMessage) { got = m })

	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:        "msg-1",
		GuildID:   "guild-1",
		ChannelID: "chan-1",
		Content:   "hi",
		Author:    &discordgo.User{ID: "user-1", Username: "someuser"},
	}})

	if got.MessageID != "msg-1" {
		t.Errorf("MessageID = %q, want %q", got.MessageID, "msg-1")
	}
	if got.GuildID != "guild-1" {
		t.Errorf("GuildID = %q, want %q", got.GuildID, "guild-1")
	}
}

func TestBot_HandleMessageCreate_FallsBackToUsername(t *testing.T) {
	b := newTestBot(t, false)

	var got IncomingMessage
	b.OnMessage(func(m IncomingMessage) { got = m })

	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "chan-1",
		Content:   "hi",
		Author:    &discordgo.User{ID: "user-1", Username: "someuser"},
	}})

	if got.AuthorName != "someuser" {
		t.Errorf("AuthorName = %q, want %q", got.AuthorName, "someuser")
	}
}

func TestBot_HandleMessageCreate_IgnoresOwnBotMessages(t *testing.T) {
	b := newTestBot(t, false)

	called := false
	b.OnMessage(func(m IncomingMessage) { called = true })

	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "chan-1",
		Content:   "hi",
		Author:    &discordgo.User{ID: "bot-id", Username: "hoplink"},
	}})

	if called {
		t.Fatal("handler should not be invoked for the bot's own messages")
	}
}

func TestBot_HandleMessageCreate_PropagatesWebhookID(t *testing.T) {
	b := newTestBot(t, false)

	var got IncomingMessage
	b.OnMessage(func(m IncomingMessage) { got = m })

	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "chan-1",
		Content:   "relayed from mesh",
		Author:    &discordgo.User{ID: "webhook-user-1", Username: "SomeNode"},
		WebhookID: "webhook-42",
	}})

	if got.WebhookID != "webhook-42" {
		t.Errorf("WebhookID = %q, want %q", got.WebhookID, "webhook-42")
	}
}

func TestBot_HandleMessageCreate_PreferDisplayName_UsesGlobalNameOverUsername(t *testing.T) {
	b := newTestBot(t, true)

	var got IncomingMessage
	b.OnMessage(func(m IncomingMessage) { got = m })

	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "chan-1",
		Content:   "hi",
		Author:    &discordgo.User{ID: "user-1", Username: "raw_handle_99", GlobalName: "Cool Person"},
	}})

	if got.AuthorName != "Cool Person" {
		t.Errorf("AuthorName = %q, want %q", got.AuthorName, "Cool Person")
	}
}

func TestBot_HandleMessageCreate_PreferDisplayName_FallsBackToUsernameWithNoGlobalName(t *testing.T) {
	b := newTestBot(t, true)

	var got IncomingMessage
	b.OnMessage(func(m IncomingMessage) { got = m })

	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "chan-1",
		Content:   "hi",
		Author:    &discordgo.User{ID: "user-1", Username: "raw_handle_99"},
	}})

	if got.AuthorName != "raw_handle_99" {
		t.Errorf("AuthorName = %q, want %q", got.AuthorName, "raw_handle_99")
	}
}

func TestBot_HandleMessageCreate_PreferDisplayName_NicknameStillWinsOverGlobalName(t *testing.T) {
	b := newTestBot(t, true)

	var got IncomingMessage
	b.OnMessage(func(m IncomingMessage) { got = m })

	// Member.User is intentionally left nil here — discordgo does not
	// populate it on gateway MessageCreate events (Discord's payload only
	// includes a partial member without a nested user object, since the
	// top-level message already carries Author). handleMessageCreate must
	// backfill it before calling Member.DisplayName(), or this panics.
	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "chan-1",
		Content:   "hi",
		Author:    &discordgo.User{ID: "user-1", Username: "raw_handle_99", GlobalName: "Cool Person"},
		Member:    &discordgo.Member{Nick: "ServerNick"},
	}})

	if got.AuthorName != "ServerNick" {
		t.Errorf("AuthorName = %q, want %q (nickname should outrank global display name)", got.AuthorName, "ServerNick")
	}
}

func TestBot_HandleMessageCreate_PreferDisplayName_MemberWithNoNickUsesGlobalName(t *testing.T) {
	b := newTestBot(t, true)

	var got IncomingMessage
	b.OnMessage(func(m IncomingMessage) { got = m })

	b.handleMessageCreate(nil, &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: "chan-1",
		Content:   "hi",
		Author:    &discordgo.User{ID: "user-1", Username: "raw_handle_99", GlobalName: "Cool Person"},
		Member:    &discordgo.Member{}, // present, but no nickname set
	}})

	if got.AuthorName != "Cool Person" {
		t.Errorf("AuthorName = %q, want %q", got.AuthorName, "Cool Person")
	}
}
