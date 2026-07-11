package meshcore

import (
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// PublicChannelKey is the well-known 16-byte secret for MeshCore's default
// public group chat (docs/companion_protocol.md).
var PublicChannelKey = mustHex("8b3387e9c5cdea6ac9e5edbaa115cd72")

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// NormalizeChannelName lowercases (ASCII only) and trims ASCII whitespace
// from raw, mirroring firmware's flood-scope-name normalisation
// (meshcim_normalize_flood_scope_name).
func NormalizeChannelName(raw string) string {
	trimmed := strings.Trim(raw, " \t\n\r")
	b := []byte(trimmed)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// HashtagChannelSecret derives the 16-byte channel secret for a hashtag
// channel name: sha256("#" + normalize(name))[:16]. A leading '#' in name is
// not duplicated. Matches docs/companion_protocol.md's formula (verified
// against the pinned vector "#test" -> 9cd8fcf22a47333b591d96a2b848b73f).
func HashtagChannelSecret(name string) []byte {
	norm := NormalizeChannelName(name)
	if !strings.HasPrefix(norm, "#") {
		norm = "#" + norm
	}
	digest := sha256.Sum256([]byte(norm))
	return append([]byte(nil), digest[:16]...)
}

// ChannelHash is the 1-byte channel routing hash MeshCore derives from a
// channel secret: sha256(secret16)[0] (firmware BaseChatMesh::addChannel).
func ChannelHash(secret16 []byte) (byte, error) {
	if len(secret16) < 16 {
		return 0, fmt.Errorf("meshcore: secret must be >= 16 bytes, got %d", len(secret16))
	}
	digest := sha256.Sum256(secret16[:16])
	return digest[0], nil
}

// hmacSHA256Truncated computes HMAC-SHA256(key16, data)[0:2] — the
// "cipher_mac" used to authenticate GRP_TXT payloads (Encrypt-then-MAC).
func hmacSHA256Truncated(key16, data []byte) [2]byte {
	mac := hmac.New(sha256.New, key16)
	mac.Write(data)
	sum := mac.Sum(nil)
	return [2]byte{sum[0], sum[1]}
}

// FloodScopeKey derives the 16-byte "flood scope" key for a named
// scope/region: the identical formula and normalisation as
// HashtagChannelSecret (confirmed against firmware
// TransportKeyStore::getAutoKeyFor's call site,
// examples/companion_radio/MyMesh.cpp: `getAutoKeyFor(0, "#" DEFAULT_FLOOD_SCOPE_NAME, key)`
// — same sha256("#"+name)[0:16] construction). Named separately from
// HashtagChannelSecret because the two serve different protocol purposes
// even though the math is identical.
func FloodScopeKey(name string) []byte {
	return HashtagChannelSecret(name)
}

// CalcTransportCode computes the MeshCore "transport code" used to scope a
// ROUTE_TYPE_TRANSPORT_FLOOD/_DIRECT packet to a named flood scope/region:
// HMAC-SHA256(scopeKey16, [payloadTypeByte] || payload)[0:2] as a
// little-endian uint16, with 0 and 0xFFFF reserved (nudged to 1 / 0xFFFE).
// Matches firmware TransportKey::calcTransportCode
// (src/helpers/TransportKeyStore.cpp).
//
// Repeaters configured with the same scope (CMD_SET_DEFAULT_FLOOD_SCOPE)
// compute this same value to recognise and relay the packet. This matters:
// repeaters running in "scope-only" mode reject unscoped ROUTE_TYPE_FLOOD
// packets outright (examples/simple_repeater/MyMesh.cpp,
// MyMesh::allowPacketForward: "unknown transport code, or wildcard not
// allowed for FLOOD packet") — so on such a mesh, a scope must be set for
// messages to propagate past the first hop.
func CalcTransportCode(scopeKey16 []byte, payloadType RfPayloadType, payload []byte) (uint16, error) {
	if len(scopeKey16) < 16 {
		return 0, fmt.Errorf("meshcore: scope key must be >= 16 bytes, got %d", len(scopeKey16))
	}
	mac := hmac.New(sha256.New, scopeKey16[:16])
	mac.Write([]byte{byte(payloadType)})
	mac.Write(payload)
	digest := mac.Sum(nil)
	code := uint16(digest[0]) | uint16(digest[1])<<8
	switch code {
	case 0:
		code = 1
	case 0xFFFF:
		code = 0xFFFE
	}
	return code, nil
}

// EncodeTransportCodes packs two uint16 transport codes into the 4-byte
// TransportCodes field of a Packet (little-endian, matching firmware
// Packet::writeTo).
func EncodeTransportCodes(code0, code1 uint16) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint16(out[0:2], code0)
	binary.LittleEndian.PutUint16(out[2:4], code1)
	return out
}

// aesECBEncrypt encrypts pt (length must be a multiple of the AES block
// size) with AES-128-ECB, no padding. Used for GRP_TXT payloads, which
// zero-pad their own plaintext to a block boundary before this step.
func aesECBEncrypt(key16, pt []byte) ([]byte, error) {
	block, err := aes.NewCipher(key16)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if len(pt)%bs != 0 {
		return nil, fmt.Errorf("meshcore: plaintext length %d not a multiple of block size %d", len(pt), bs)
	}
	out := make([]byte, len(pt))
	for off := 0; off < len(pt); off += bs {
		block.Encrypt(out[off:off+bs], pt[off:off+bs])
	}
	return out, nil
}

// aesECBDecrypt is the inverse of aesECBEncrypt.
func aesECBDecrypt(key16, ct []byte) ([]byte, error) {
	block, err := aes.NewCipher(key16)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if len(ct)%bs != 0 {
		return nil, fmt.Errorf("meshcore: ciphertext length %d not a multiple of block size %d", len(ct), bs)
	}
	out := make([]byte, len(ct))
	for off := 0; off < len(ct); off += bs {
		block.Decrypt(out[off:off+bs], ct[off:off+bs])
	}
	return out, nil
}

// BuildGroupTextPayload builds a GRP_TXT (channel) RF payload — the bytes
// that go after header/transport_codes/path in the RF packet:
//
//	[channel_hash(1)][cipher_mac(2)][AES-128-ECB ciphertext]
//
// text is the full plaintext exactly as it should appear, e.g. the caller
// composes "<sender>: <message>" (mirrors firmware
// BaseChatMesh::sendGroupMessage). flags is the post-timestamp flag byte (0
// = TXT_TYPE_PLAIN).
func BuildGroupTextPayload(secret16 []byte, timestampUnix uint32, flags byte, text string) ([]byte, error) {
	if len(secret16) < 16 {
		return nil, fmt.Errorf("meshcore: secret must be >= 16 bytes, got %d", len(secret16))
	}
	key := secret16[:16]
	textBytes := []byte(text)

	ptLen := 5 + len(textBytes) // timestamp(4) + flags(1) + text
	ctLen := ((ptLen + 15) / 16) * 16
	if ctLen == 0 {
		ctLen = 16
	}

	pt := make([]byte, ctLen) // zero-padded
	binary.LittleEndian.PutUint32(pt[0:4], timestampUnix)
	pt[4] = flags
	copy(pt[5:], textBytes)

	ct, err := aesECBEncrypt(key, pt)
	if err != nil {
		return nil, err
	}

	mac := hmacSHA256Truncated(key, ct)
	chHash, err := ChannelHash(key)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, 3+len(ct))
	out = append(out, chHash, mac[0], mac[1])
	out = append(out, ct...)
	return out, nil
}

// GroupTextDecrypt is a GRP_TXT payload decrypted with a channel secret.
type GroupTextDecrypt struct {
	TimestampUnix uint32
	Flags         byte
	Text          string // full "sender: message" plaintext
}

// DecryptGroupText decrypts a GRP_TXT RF payload (the bytes after
// header/transport_codes/path) with a channel's 16-byte secret16.
//
// Returns ok=false when the MAC doesn't match — this simply means payload is
// not for this channel's secret; callers try each candidate channel secret
// in turn.
func DecryptGroupText(secret16, payload []byte) (result GroupTextDecrypt, ok bool) {
	if len(secret16) < 16 || len(payload) < 3+16 {
		return GroupTextDecrypt{}, false
	}
	key := secret16[:16]
	ct := payload[3:]
	if len(ct)%16 != 0 || len(ct) > 4096 {
		return GroupTextDecrypt{}, false
	}

	mac := hmacSHA256Truncated(key, ct)
	if mac[0] != payload[1] || mac[1] != payload[2] {
		return GroupTextDecrypt{}, false
	}

	pt, err := aesECBDecrypt(key, ct)
	if err != nil || len(pt) < 5 {
		return GroupTextDecrypt{}, false
	}

	timestamp := binary.LittleEndian.Uint32(pt[0:4])
	flags := pt[4]

	textEnd := len(pt)
	for i := 5; i < len(pt); i++ {
		if pt[i] == 0 {
			textEnd = i
			break
		}
	}
	text := string(pt[5:textEnd])

	return GroupTextDecrypt{
		TimestampUnix: timestamp,
		Flags:         flags,
		Text:          text,
	}, true
}
