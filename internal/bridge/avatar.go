package bridge

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// twemojiVersion pins a specific Twemoji release so avatar URLs stay stable
// (rather than depending on a moving "latest" tag).
const twemojiVersion = "14.0.2"

// avatarURLForName picks a webhook avatar for a MeshCore sender name: the
// first emoji found anywhere in the name (rendered via the Twemoji CDN), or
// failing that the first letter/digit (rendered as an initial via
// ui-avatars.com, uppercased). Returns "" when name has no usable character
// at all, in which case Discord falls back to the webhook's own avatar.
//
// This is a practical approximation, not a full emoji-property
// implementation: multi-codepoint sequences (flags, ZWJ-joined emoji, skin
// tone modifiers) aren't specially handled and fall back to the letter
// avatar via their non-emoji surrounding characters, or are skipped if none
// are found before them.
func avatarURLForName(name string) string {
	for _, r := range name {
		if isEmojiRune(r) {
			return twemojiURL(r)
		}
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return initialsAvatarURL(r)
		}
	}
	return ""
}

func twemojiURL(r rune) string {
	return fmt.Sprintf("https://cdn.jsdelivr.net/gh/twitter/twemoji@%s/assets/72x72/%x.png", twemojiVersion, r)
}

func initialsAvatarURL(r rune) string {
	letter := strings.ToUpper(string(r))
	v := url.Values{
		"name":       {letter},
		"size":       {"128"},
		"background": {"random"},
		"bold":       {"true"},
		"format":     {"png"},
	}
	return "https://ui-avatars.com/api/?" + v.Encode()
}

// isEmojiRune reports whether r falls in a Unicode block Twemoji renders as
// a standalone pictograph.
func isEmojiRune(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF: // pictographs, emoticons, transport, symbols
		return true
	case r >= 0x2600 && r <= 0x27BF: // misc symbols & dingbats
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // misc symbols and arrows
		return true
	default:
		return false
	}
}
