package meshcore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Vectors pinned from meshcore/docs/companion_protocol.md:
//   - "#test" -> secret 9cd8fcf22a47333b591d96a2b848b73f
//   - public channel key 8b3387e9c5cdea6ac9e5edbaa115cd72
func TestHashtagChannelSecret_KnownVector(t *testing.T) {
	got := HashtagChannelSecret("#test")
	want, _ := hex.DecodeString("9cd8fcf22a47333b591d96a2b848b73f")
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("HashtagChannelSecret(#test) = %x, want %x", got, want)
	}
}

func TestHashtagChannelSecret_NoLeadingHashSameAsWithHash(t *testing.T) {
	withHash := HashtagChannelSecret("#general")
	withoutHash := HashtagChannelSecret("general")
	if hex.EncodeToString(withHash) != hex.EncodeToString(withoutHash) {
		t.Fatalf("secret should be identical whether or not caller includes '#': %x vs %x", withHash, withoutHash)
	}
}

func TestHashtagChannelSecret_NormalizesCaseAndWhitespace(t *testing.T) {
	a := HashtagChannelSecret("  #General  ")
	b := HashtagChannelSecret("general")
	if hex.EncodeToString(a) != hex.EncodeToString(b) {
		t.Fatalf("secret should be case/whitespace insensitive: %x vs %x", a, b)
	}
}

func TestPublicChannelKey_KnownVector(t *testing.T) {
	want, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	if hex.EncodeToString(PublicChannelKey) != hex.EncodeToString(want) {
		t.Fatalf("PublicChannelKey = %x, want %x", PublicChannelKey, want)
	}
}

func TestGroupTextPayload_RoundTrip(t *testing.T) {
	secret := HashtagChannelSecret("#general")
	const ts = uint32(1234567890)
	const text = "Alice: hello from the mesh!"

	payload, err := BuildGroupTextPayload(secret, ts, 0, text)
	if err != nil {
		t.Fatalf("BuildGroupTextPayload: %v", err)
	}

	got, ok := DecryptGroupText(secret, payload)
	if !ok {
		t.Fatalf("DecryptGroupText: MAC did not verify")
	}
	if got.TimestampUnix != ts {
		t.Errorf("timestamp = %d, want %d", got.TimestampUnix, ts)
	}
	if got.Flags != 0 {
		t.Errorf("flags = %d, want 0", got.Flags)
	}
	if got.Text != text {
		t.Errorf("text = %q, want %q", got.Text, text)
	}
}

func TestGroupTextPayload_WrongSecretFailsMAC(t *testing.T) {
	secret := HashtagChannelSecret("#general")
	other := HashtagChannelSecret("#emergency")

	payload, err := BuildGroupTextPayload(secret, 1000, 0, "Bob: hi")
	if err != nil {
		t.Fatalf("BuildGroupTextPayload: %v", err)
	}

	if _, ok := DecryptGroupText(other, payload); ok {
		t.Fatalf("DecryptGroupText succeeded with the wrong channel secret")
	}
}

func TestGroupTextPayload_ChannelHashMatchesFirstByte(t *testing.T) {
	secret := HashtagChannelSecret("#test")
	payload, err := BuildGroupTextPayload(secret, 1, 0, "x")
	if err != nil {
		t.Fatalf("BuildGroupTextPayload: %v", err)
	}
	wantHash, err := ChannelHash(secret)
	if err != nil {
		t.Fatalf("ChannelHash: %v", err)
	}
	if payload[0] != wantHash {
		t.Errorf("payload channel_hash byte = %#x, want %#x", payload[0], wantHash)
	}
}

func TestGroupTextPayload_EmptyTextStillEncrypts(t *testing.T) {
	secret := HashtagChannelSecret("#test")
	payload, err := BuildGroupTextPayload(secret, 42, 0, "")
	if err != nil {
		t.Fatalf("BuildGroupTextPayload: %v", err)
	}
	got, ok := DecryptGroupText(secret, payload)
	if !ok {
		t.Fatalf("DecryptGroupText failed on empty-text payload")
	}
	if got.Text != "" {
		t.Errorf("text = %q, want empty", got.Text)
	}
	if got.TimestampUnix != 42 {
		t.Errorf("timestamp = %d, want 42", got.TimestampUnix)
	}
}

func TestGroupTextPayload_LongTextSpanningMultipleBlocks(t *testing.T) {
	secret := HashtagChannelSecret("#test")
	// Build a long but printable message (avoid embedded NUL, which would
	// truncate the decoded text early).
	body := make([]byte, 150)
	for i := range body {
		body[i] = 'a' + byte(i%26)
	}
	longText := "Alice: " + string(body)

	payload, err := BuildGroupTextPayload(secret, 1, 0, longText)
	if err != nil {
		t.Fatalf("BuildGroupTextPayload: %v", err)
	}
	got, ok := DecryptGroupText(secret, payload)
	if !ok {
		t.Fatalf("DecryptGroupText failed")
	}
	if got.Text != longText {
		t.Errorf("text mismatch: got %d bytes, want %d bytes", len(got.Text), len(longText))
	}
}

func TestFloodScopeKey_MatchesHashtagChannelSecretFormula(t *testing.T) {
	// Firmware derives both from the identical sha256("#"+name)[0:16]
	// construction (examples/companion_radio/MyMesh.cpp:
	// getAutoKeyFor(0, "#" DEFAULT_FLOOD_SCOPE_NAME, key)), so these two
	// functions must agree even though they serve different purposes.
	if hex.EncodeToString(FloodScopeKey("myregion")) != hex.EncodeToString(HashtagChannelSecret("myregion")) {
		t.Errorf("FloodScopeKey and HashtagChannelSecret diverged for the same name")
	}
}

// TestCalcTransportCode_MatchesIndependentlyComputedHMAC computes the
// expected value directly with stdlib crypto/hmac+sha256 (not via any of
// this package's own helpers) to avoid a tautological check against
// CalcTransportCode's own implementation.
func TestCalcTransportCode_MatchesIndependentlyComputedHMAC(t *testing.T) {
	scopeKey := HashtagChannelSecret("#test")
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	mac := hmac.New(sha256.New, scopeKey)
	mac.Write([]byte{byte(PayloadTypeGrpTxt)})
	mac.Write(payload)
	digest := mac.Sum(nil)
	want := uint16(digest[0]) | uint16(digest[1])<<8
	if want == 0 {
		want = 1
	} else if want == 0xFFFF {
		want = 0xFFFE
	}

	got, err := CalcTransportCode(scopeKey, PayloadTypeGrpTxt, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	if got != want {
		t.Errorf("CalcTransportCode = %#x, want %#x", got, want)
	}
}

func TestCalcTransportCode_DifferentPayloadTypeGivesDifferentCode(t *testing.T) {
	scopeKey := HashtagChannelSecret("#test")
	payload := []byte("same payload bytes")

	codeGrpTxt, err := CalcTransportCode(scopeKey, PayloadTypeGrpTxt, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	codeTxtMsg, err := CalcTransportCode(scopeKey, PayloadTypeTxtMsg, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	if codeGrpTxt == codeTxtMsg {
		t.Errorf("expected different payload types to (almost certainly) yield different transport codes")
	}
}

func TestCalcTransportCode_DifferentScopeGivesDifferentCode(t *testing.T) {
	payload := []byte("hello")
	codeA, err := CalcTransportCode(HashtagChannelSecret("#region-a"), PayloadTypeGrpTxt, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	codeB, err := CalcTransportCode(HashtagChannelSecret("#region-b"), PayloadTypeGrpTxt, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	if codeA == codeB {
		t.Errorf("expected different scopes to (almost certainly) yield different transport codes")
	}
}

func TestCalcTransportCode_RejectsShortKey(t *testing.T) {
	if _, err := CalcTransportCode([]byte{1, 2, 3}, PayloadTypeGrpTxt, []byte("x")); err == nil {
		t.Fatal("expected error for a scope key shorter than 16 bytes")
	}
}

func TestEncodeTransportCodes(t *testing.T) {
	got := EncodeTransportCodes(0x1234, 0x5678)
	want := []byte{0x34, 0x12, 0x78, 0x56}
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Errorf("EncodeTransportCodes = %x, want %x", got, want)
	}
}

func TestRfRouteType_WithTransportCodes(t *testing.T) {
	cases := []struct {
		in, want RfRouteType
	}{
		{RouteFlood, RouteTransportFlood},
		{RouteDirect, RouteTransportDirect},
		{RouteTransportFlood, RouteTransportFlood},
		{RouteTransportDirect, RouteTransportDirect},
	}
	for _, c := range cases {
		if got := c.in.WithTransportCodes(); got != c.want {
			t.Errorf("RfRouteType(%v).WithTransportCodes() = %v, want %v", c.in, got, c.want)
		}
	}
}
