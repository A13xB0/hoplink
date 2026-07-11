package bridge

import (
	"net/url"
	"strings"
	"testing"
)

func TestAvatarURLForName_FirstLetterWhenNoEmoji(t *testing.T) {
	got := avatarURLForName("Alice")
	if !strings.HasPrefix(got, "https://ui-avatars.com/api/?") {
		t.Fatalf("got %q, want a ui-avatars.com URL", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if u.Query().Get("name") != "A" {
		t.Errorf("name param = %q, want %q", u.Query().Get("name"), "A")
	}
}

func TestAvatarURLForName_UppercasesTheLetter(t *testing.T) {
	got := avatarURLForName("bob")
	u, _ := url.Parse(got)
	if u.Query().Get("name") != "B" {
		t.Errorf("name param = %q, want %q", u.Query().Get("name"), "B")
	}
}

func TestAvatarURLForName_SkipsLeadingSymbolsForLetterFallback(t *testing.T) {
	got := avatarURLForName("_bob_")
	u, _ := url.Parse(got)
	if u.Query().Get("name") != "B" {
		t.Errorf("name param = %q, want %q", u.Query().Get("name"), "B")
	}
}

func TestAvatarURLForName_UsesFirstEmojiWhenPresent(t *testing.T) {
	got := avatarURLForName("Alice 🔥 the fire mage")
	want := "https://cdn.jsdelivr.net/gh/twitter/twemoji@" + twemojiVersion + "/assets/72x72/1f525.png"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAvatarURLForName_EmojiTakesPriorityOverLeadingLetter(t *testing.T) {
	// The name starts with a letter, but an emoji appears later — the emoji
	// should still win per "first emoji in their name".
	got := avatarURLForName("Bob 🚀")
	want := "https://cdn.jsdelivr.net/gh/twitter/twemoji@" + twemojiVersion + "/assets/72x72/1f680.png"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAvatarURLForName_EmptyNameReturnsEmpty(t *testing.T) {
	if got := avatarURLForName(""); got != "" {
		t.Errorf("got %q, want empty (fall back to webhook's default avatar)", got)
	}
}

func TestAvatarURLForName_OnlySymbolsReturnsEmpty(t *testing.T) {
	if got := avatarURLForName("___---"); got != "" {
		t.Errorf("got %q, want empty (no letter/digit/emoji found)", got)
	}
}

func TestAvatarURLForName_DigitFallback(t *testing.T) {
	got := avatarURLForName("42node")
	u, _ := url.Parse(got)
	if u.Query().Get("name") != "4" {
		t.Errorf("name param = %q, want %q", u.Query().Get("name"), "4")
	}
}

func TestIsEmojiRune(t *testing.T) {
	cases := []struct {
		r    rune
		want bool
	}{
		{'🔥', true}, // U+1F525, pictographs block
		{'☕', true}, // U+2615, misc symbols & dingbats
		{'⭐', true}, // U+2B50, misc symbols and arrows
		{'A', false},
		{'4', false},
		{' ', false},
		{'_', false},
	}
	for _, c := range cases {
		if got := isEmojiRune(c.r); got != c.want {
			t.Errorf("isEmojiRune(%q) = %v, want %v", c.r, got, c.want)
		}
	}
}
