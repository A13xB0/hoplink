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
