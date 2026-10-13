package bridge

import "testing"

func TestFormatSenderName_None(t *testing.T) {
	cases := []string{"", "none", "bogus"}
	for _, style := range cases {
		if got := formatSenderName("Alice", originMeshcore, style); got != "Alice" {
			t.Errorf("formatSenderName(style=%q) = %q, want %q", style, got, "Alice")
		}
	}
}

func TestFormatSenderName_Short(t *testing.T) {
	cases := []struct {
		o    origin
		want string
	}{
		{originDiscord, "Alice (DC)"},
		{originMeshcore, "Alice (MC)"},
		{originMeshtastic, "Alice (MT)"},
	}
	for _, c := range cases {
		if got := formatSenderName("Alice", c.o, "short"); got != c.want {
			t.Errorf("formatSenderName(origin=%v, short) = %q, want %q", c.o, got, c.want)
		}
	}
}

func TestFormatSenderName_Full(t *testing.T) {
	cases := []struct {
		o    origin
		want string
	}{
		{originDiscord, "Alice (Discord)"},
		{originMeshcore, "Alice (MeshCore)"},
		{originMeshtastic, "Alice (Meshtastic)"},
	}
	for _, c := range cases {
		if got := formatSenderName("Alice", c.o, "full"); got != c.want {
			t.Errorf("formatSenderName(origin=%v, full) = %q, want %q", c.o, got, c.want)
		}
	}
}

// A name that already carries origin tags (relayed through another hoplink,
// or typed by the user) must never gain a second one.
func TestFormatSenderName_NeverStacksTags(t *testing.T) {
	cases := []struct {
		name  string
		o     origin
		style string
		want  string
	}{
		{"Alex (DC)", originMeshcore, "short", "Alex (DC)"},
		{"Alex (DC) (MC)", originMeshtastic, "short", "Alex (DC)"},
		{"Alex (DC) (MC) (MT)", originMeshcore, "short", "Alex (DC)"},
		{"Alex (Discord)", originMeshcore, "short", "Alex (DC)"},
		{"Alex (DC)", originMeshcore, "full", "Alex (Discord)"},
		{"Alex(mc) [MT]", originDiscord, "full", "Alex (MeshCore)"},
		{"Alex (DC) (MC)", originMeshtastic, "none", "Alex (DC)"},
		{"Alex (DC)", originMeshcore, "none", "Alex (DC)"},
		{"Alex (Discord) (MC)", originMeshcore, "none", "Alex (Discord)"},
		// Other bracketed text is part of the name.
		{"Bob (Portable)", originMeshcore, "short", "Bob (Portable) (MC)"},
		{"Bob (Portable) (MT)", originMeshcore, "short", "Bob (Portable) (MT)"},
		{"Bob (Portable)", originMeshcore, "none", "Bob (Portable)"},
		// A tag that is the whole name is left alone.
		{"(MC)", originDiscord, "short", "(MC) (DC)"},
	}
	for _, c := range cases {
		if got := formatSenderName(c.name, c.o, c.style); got != c.want {
			t.Errorf("formatSenderName(%q, %v, %q) = %q, want %q", c.name, c.o, c.style, got, c.want)
		}
	}
}

// Formatting an already-formatted name again (the next hop) is a no-op.
func TestFormatSenderName_Idempotent(t *testing.T) {
	for _, style := range []string{"short", "full"} {
		for _, o := range []origin{originDiscord, originMeshcore, originMeshtastic} {
			once := formatSenderName("Alice (Base)", o, style)
			for _, next := range []origin{originDiscord, originMeshcore, originMeshtastic} {
				if twice := formatSenderName(once, next, style); twice != once {
					t.Errorf("style %q: %q re-formatted via %v = %q", style, once, next, twice)
				}
			}
		}
	}
}
