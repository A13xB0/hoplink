package bridge

import (
	"fmt"

	"github.com/hectospark/hoplink/internal/discord"
)

// composedLength returns the UTF-8 byte length of "<name>: <content>" — the
// same string composeChunks would otherwise split — used to gate the
// byte-limit check before any backend send is attempted.
func composedLength(name, content string) int {
	return len(name) + len(": ") + len(content)
}

// notifyTooLong posts a bot-authored reply (not a webhook post — this must
// visibly come from hoplink itself, not impersonate a mesh node) telling
// the Discord sender their message was not sent to the mesh.
func (b *Bridge) notifyTooLong(msg discord.IncomingMessage, gotBytes, maxBytes int) {
	content := fmt.Sprintf("⚠️ Not sent to the mesh — message is %d bytes, limit is %d bytes.", gotBytes, maxBytes)
	if err := b.notify.Reply(msg.ChannelID, msg.MessageID, content); err != nil {
		logf("posting oversized-message notice: %v", err)
	}
}
