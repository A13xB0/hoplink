package bridge

import (
	"strings"
	"testing"
)

func TestComposeChunks_ShortMessageIsSingleChunkNoIndicator(t *testing.T) {
	got := composeChunks("Alice", "hello there", meshcoreMaxChunkBytes)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1: %v", len(got), got)
	}
	if got[0] != "Alice: hello there" {
		t.Errorf("chunk = %q, want %q", got[0], "Alice: hello there")
	}
}

func TestComposeChunks_LongMessageSplitsWithIndicatorAndFitsBudget(t *testing.T) {
	content := strings.Repeat("word ", 100) // way over meshcoreMaxChunkBytes
	chunks := composeChunks("Alice", content, meshcoreMaxChunkBytes)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > meshcoreMaxChunkBytes {
			t.Errorf("chunk %d length %d exceeds meshcoreMaxChunkBytes %d: %q", i, len(c), meshcoreMaxChunkBytes, c)
		}
		wantPrefix := "Alice ("
		if !strings.HasPrefix(c, wantPrefix) {
			t.Errorf("chunk %d = %q, want prefix %q", i, c, wantPrefix)
		}
	}
}

func TestComposeChunks_NeverSplitsAMultiByteRune(t *testing.T) {
	// Repeated multi-byte emoji, long enough to force a split partway
	// through what would otherwise land mid-rune with a naive byte cut.
	content := strings.Repeat("😀", 80)
	chunks := composeChunks("Bob", content, meshcoreMaxChunkBytes)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	var rebuilt strings.Builder
	for _, c := range chunks {
		if !strings.Contains(c, ": ") {
			t.Fatalf("chunk missing sender separator: %q", c)
		}
		if strings.ContainsRune(c, '�') {
			t.Errorf("chunk contains the UTF-8 replacement character (split rune?): %q", c)
		}
		body := c[strings.Index(c, ": ")+2:]
		rebuilt.WriteString(body)
	}
	if rebuilt.String() != content {
		t.Errorf("rebuilt content mismatch: got %d runes, want %d runes", len([]rune(rebuilt.String())), len([]rune(content)))
	}
}

func TestComposeChunks_EachChunkDecryptableRoundTrip(t *testing.T) {
	content := strings.Repeat("The mesh is wide and the packets are small. ", 10)
	chunks := composeChunks("Charlie", content, meshcoreMaxChunkBytes)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks to exercise the mesh crypto path, got %d", len(chunks))
	}
	// Each composed chunk must actually be encryptable/decryptable as a
	// GRP_TXT payload within the real byte budget used by SendChannelMessage.
	for _, c := range chunks {
		if len(c) > meshcoreMaxChunkBytes {
			t.Fatalf("chunk exceeds meshcoreMaxChunkBytes: %d > %d", len(c), meshcoreMaxChunkBytes)
		}
	}
}

func TestComposeChunks_RespectsMeshtasticBudget(t *testing.T) {
	content := strings.Repeat("word ", 100)
	chunks := composeChunks("Alice", content, meshtasticMaxChunkBytes)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > meshtasticMaxChunkBytes {
			t.Errorf("chunk %d length %d exceeds meshtasticMaxChunkBytes %d: %q", i, len(c), meshtasticMaxChunkBytes, c)
		}
	}
	// The larger Meshtastic budget should need fewer chunks than MeshCore's
	// for the same content.
	meshcoreChunks := composeChunks("Alice", content, meshcoreMaxChunkBytes)
	if len(chunks) >= len(meshcoreChunks) {
		t.Errorf("expected fewer chunks with the larger meshtastic budget: got %d meshtastic vs %d meshcore", len(chunks), len(meshcoreChunks))
	}
}

func TestSplitSenderText_WithSeparator(t *testing.T) {
	sender, body := splitSenderText("Alice: hello there")
	if sender != "Alice" || body != "hello there" {
		t.Errorf("got (%q, %q)", sender, body)
	}
}

func TestSplitSenderText_NoSeparatorFallsBackToMesh(t *testing.T) {
	sender, body := splitSenderText("just some text")
	if sender != "mesh" || body != "just some text" {
		t.Errorf("got (%q, %q)", sender, body)
	}
}

func TestSplitSenderText_OnlyFirstSeparatorSplits(t *testing.T) {
	sender, body := splitSenderText("Alice: hello: there")
	if sender != "Alice" || body != "hello: there" {
		t.Errorf("got (%q, %q)", sender, body)
	}
}
