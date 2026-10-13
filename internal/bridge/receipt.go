package bridge

import "sync"

// Delivery-status reactions on the Discord message a MeshCore send came
// from (discord.delivery_reactions). One discordReceipt spans every chunk
// of that message: 📡 when the radio first accepts a chunk, then — once
// every chunk has settled (see reportDelivery) — ✅ if all were confirmed
// delivered, otherwise ⚠️, and the 📡 is removed. A ⚠️ is upgraded to ✅ if
// late CoreScope evidence later confirms every chunk that was unconfirmed
// (see lateConfirm).

const (
	reactAccepted    = "📡"
	reactDelivered   = "✅"
	reactUnconfirmed = "⚠️"
)

// reactor is the subset of *discord.Bot used for delivery reactions — an
// interface so tests can substitute a fake.
type reactor interface {
	React(channelID, messageID, emoji string) error
	Unreact(channelID, messageID, emoji string) error
}

type discordReceipt struct {
	react                reactor
	channelID, messageID string

	mu       sync.Mutex
	pending  int  // chunks not yet settled
	failed   int  // chunks settled unconfirmed (and not since confirmed late)
	accepted bool // 📡 added
}

// newDiscordReceipt returns a receipt for a message split into chunks
// pieces, or nil when delivery reactions are off — every method is a no-op
// on a nil receipt.
func (b *Bridge) newDiscordReceipt(channelID, messageID string, chunks int) *discordReceipt {
	if b.react == nil || messageID == "" {
		return nil
	}
	return &discordReceipt{react: b.react, channelID: channelID, messageID: messageID, pending: chunks}
}

// markAccepted adds 📡 the first time any chunk is accepted by the radio.
func (r *discordReceipt) markAccepted() {
	if r == nil {
		return
	}
	r.mu.Lock()
	first := !r.accepted && r.pending > 0
	r.accepted = true
	r.mu.Unlock()
	if first {
		r.do(r.react.React, reactAccepted)
	}
}

// settle records one chunk's final outcome; the last one to settle sets the
// message's final reaction.
func (r *discordReceipt) settle(confirmed bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.pending <= 0 {
		r.mu.Unlock()
		return
	}
	r.pending--
	if !confirmed {
		r.failed++
	}
	done, ok, hadAccepted := r.pending == 0, r.failed == 0, r.accepted
	r.mu.Unlock()
	if !done {
		return
	}
	final := reactUnconfirmed
	if ok {
		final = reactDelivered
	}
	r.do(r.react.React, final)
	if hadAccepted {
		r.do(r.react.Unreact, reactAccepted)
	}
}

// lateConfirm records that one chunk previously settled as unconfirmed has
// since been confirmed (late CoreScope evidence). Once every chunk has
// settled and none remain unconfirmed, the ⚠️ is swapped for ✅.
func (r *discordReceipt) lateConfirm() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.failed <= 0 {
		r.mu.Unlock()
		return
	}
	r.failed--
	upgrade := r.failed == 0 && r.pending == 0
	r.mu.Unlock()
	if upgrade {
		r.do(r.react.React, reactDelivered)
		r.do(r.react.Unreact, reactUnconfirmed)
	}
}

func (r *discordReceipt) do(fn func(channelID, messageID, emoji string) error, emoji string) {
	if err := fn(r.channelID, r.messageID, emoji); err != nil {
		logfDiscord("delivery reaction on message %s: %v (does the bot have the Add Reactions permission?)", r.messageID, err)
	}
}
