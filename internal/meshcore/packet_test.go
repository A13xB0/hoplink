package meshcore

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestBuildParsePacket_FloodNoPath(t *testing.T) {
	p := Packet{
		Route:       RouteFlood,
		PayloadType: PayloadTypeGrpTxt,
		Version:     PayloadVer1,
		Payload:     []byte{1, 2, 3, 4},
	}
	raw, err := BuildPacket(p)
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	// header, path_len, payload... = 1 + 1 + 4 = 6 bytes
	if len(raw) != 6 {
		t.Fatalf("len(raw) = %d, want 6", len(raw))
	}
	wantHeader := byte(RouteFlood) | (byte(PayloadTypeGrpTxt) << 2)
	if raw[0] != wantHeader {
		t.Errorf("header = %#x, want %#x", raw[0], wantHeader)
	}
	if raw[1] != 0 {
		t.Errorf("path_len = %d, want 0", raw[1])
	}

	got, err := ParsePacket(raw)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if got.Route != RouteFlood || got.PayloadType != PayloadTypeGrpTxt || got.Version != PayloadVer1 {
		t.Errorf("parsed header fields mismatch: %+v", got)
	}
	if !bytes.Equal(got.Payload, p.Payload) {
		t.Errorf("payload = %v, want %v", got.Payload, p.Payload)
	}
	if len(got.Path) != 0 {
		t.Errorf("path = %v, want empty", got.Path)
	}
}

func TestBuildParsePacket_WithPath(t *testing.T) {
	p := Packet{
		Route:       RouteDirect,
		PayloadType: PayloadTypeTxtMsg,
		Path:        []byte{0xAA, 0xBB, 0xCC},
		HashSize:    1,
		Payload:     []byte("hello"),
	}
	raw, err := BuildPacket(p)
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	got, err := ParsePacket(raw)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if !bytes.Equal(got.Path, p.Path) {
		t.Errorf("path = %v, want %v", got.Path, p.Path)
	}
	if !bytes.Equal(got.Payload, p.Payload) {
		t.Errorf("payload = %q, want %q", got.Payload, p.Payload)
	}
}

func TestBuildParsePacket_TransportCodesRequired(t *testing.T) {
	_, err := BuildPacket(Packet{
		Route:       RouteTransportFlood,
		PayloadType: PayloadTypeGrpTxt,
		Payload:     []byte{1},
	})
	if err == nil {
		t.Fatal("expected error when TRANSPORT_FLOOD route is missing transport codes")
	}
}

func TestBuildParsePacket_TransportCodesRoundTrip(t *testing.T) {
	p := Packet{
		Route:          RouteTransportDirect,
		PayloadType:    PayloadTypeGrpTxt,
		TransportCodes: []byte{1, 2, 3, 4},
		Payload:        []byte{9, 9},
	}
	raw, err := BuildPacket(p)
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	got, err := ParsePacket(raw)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if !bytes.Equal(got.TransportCodes, p.TransportCodes) {
		t.Errorf("transportCodes = %v, want %v", got.TransportCodes, p.TransportCodes)
	}
}

func TestBuildPacket_PayloadTooLarge(t *testing.T) {
	_, err := BuildPacket(Packet{
		Route:       RouteFlood,
		PayloadType: PayloadTypeGrpTxt,
		Payload:     make([]byte, MaxPacketPayload+1),
	})
	if err == nil {
		t.Fatal("expected error for oversized payload")
	}
}

func TestBuildSendRawPacketFrame(t *testing.T) {
	packet := []byte{1, 2, 3}
	frame, err := BuildSendRawPacketFrame(packet, 3)
	if err != nil {
		t.Fatalf("BuildSendRawPacketFrame: %v", err)
	}
	want := []byte{CmdSendRawPacket, 3, 1, 2, 3}
	if !bytes.Equal(frame, want) {
		t.Errorf("frame = %v, want %v", frame, want)
	}
}

func TestBuildSendRawPacketFrame_DefaultsAndRejectsOversized(t *testing.T) {
	huge := make([]byte, MaxRawPacketLen+1)
	if _, err := BuildSendRawPacketFrame(huge, 0); err == nil {
		t.Fatal("expected error for oversized serialised packet")
	}
}

func TestParseLogRxData(t *testing.T) {
	packet := Packet{
		Route:       RouteFlood,
		PayloadType: PayloadTypeGrpTxt,
		Payload:     []byte{0xAA},
	}
	raw, err := BuildPacket(packet)
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	var snrRaw int8 = -40
	var rssiRaw int8 = -90
	frame := append([]byte{PushLogRxData, byte(snrRaw), byte(rssiRaw)}, raw...)

	got, err := ParseLogRxData(frame)
	if err != nil {
		t.Fatalf("ParseLogRxData: %v", err)
	}
	if got.SNR != -10.0 {
		t.Errorf("SNR = %v, want -10.0", got.SNR)
	}
	if got.RSSI != -90 {
		t.Errorf("RSSI = %v, want -90", got.RSSI)
	}
	if got.Packet.PayloadType != PayloadTypeGrpTxt {
		t.Errorf("PayloadType = %v, want GrpTxt", got.Packet.PayloadType)
	}
}

func TestPacket_MatchesAnyScope_UnscopedNeverMatches(t *testing.T) {
	p := Packet{Route: RouteFlood, PayloadType: PayloadTypeGrpTxt, Payload: []byte{1, 2, 3}}
	got, err := p.MatchesAnyScope([]string{"myregion"})
	if err != nil {
		t.Fatalf("MatchesAnyScope: %v", err)
	}
	if got {
		t.Error("an unscoped packet must never match a non-empty scope list")
	}
}

func TestPacket_MatchesAnyScope_MatchesConfiguredScope(t *testing.T) {
	payload := []byte{1, 2, 3}
	code, err := CalcTransportCode(FloodScopeKey("myregion"), PayloadTypeGrpTxt, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	p := Packet{
		Route:          RouteTransportFlood,
		PayloadType:    PayloadTypeGrpTxt,
		Payload:        payload,
		TransportCodes: EncodeTransportCodes(code, 0),
	}
	got, err := p.MatchesAnyScope([]string{"otherregion", "myregion"})
	if err != nil {
		t.Fatalf("MatchesAnyScope: %v", err)
	}
	if !got {
		t.Error("expected a match against the packet's actual scope")
	}
}

func TestPacket_MatchesAnyScope_RejectsWrongScope(t *testing.T) {
	payload := []byte{1, 2, 3}
	code, err := CalcTransportCode(FloodScopeKey("myregion"), PayloadTypeGrpTxt, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	p := Packet{
		Route:          RouteTransportFlood,
		PayloadType:    PayloadTypeGrpTxt,
		Payload:        payload,
		TransportCodes: EncodeTransportCodes(code, 0),
	}
	got, err := p.MatchesAnyScope([]string{"differentregion"})
	if err != nil {
		t.Fatalf("MatchesAnyScope: %v", err)
	}
	if got {
		t.Error("expected no match against an unconfigured scope")
	}
}

func TestPacket_Hash_MatchesFirmwareDefinition(t *testing.T) {
	payload := []byte{0x5a, 0x01, 0x02, 0xaa, 0xbb, 0xcc}
	p := Packet{Route: RouteFlood, PayloadType: PayloadTypeGrpTxt, HashSize: 3, Payload: payload}

	want := sha256.Sum256(append([]byte{byte(PayloadTypeGrpTxt)}, payload...))
	got := p.Hash()
	if !bytes.Equal(got[:], want[:8]) {
		t.Fatalf("Hash() = %x, want %x", got, want[:8])
	}
	if s := got.String(); len(s) != 16 {
		t.Errorf("String() = %q, want 16 hex chars", s)
	}
}

func TestPacket_Hash_IgnoresRouteTransportAndPath(t *testing.T) {
	payload := []byte{0x5a, 0x01, 0x02, 0xaa}
	orig := Packet{Route: RouteFlood, PayloadType: PayloadTypeGrpTxt, HashSize: 3, Payload: payload}
	relayed := Packet{
		Route: RouteTransportFlood, PayloadType: PayloadTypeGrpTxt, HashSize: 3,
		TransportCodes: []byte{1, 2, 3, 4}, Path: []byte{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6},
		Payload: payload,
	}
	if orig.Hash() != relayed.Hash() {
		t.Error("a relayed copy (different route/transport/path) must hash the same as the original")
	}
	other := orig
	other.Payload = []byte{0x5a, 0x01, 0x02, 0xab}
	if orig.Hash() == other.Hash() {
		t.Error("different payloads must hash differently")
	}
}

func TestPacket_Hash_SurvivesWireRoundTrip(t *testing.T) {
	pkt, err := BuildChannelPacket(HashtagChannelSecret("#test"), 3, FloodScopeKey("region"), "Alice: hi", 1_700_000_000)
	if err != nil {
		t.Fatalf("BuildChannelPacket: %v", err)
	}
	raw, err := BuildPacket(pkt)
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	parsed, err := ParsePacket(raw)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if parsed.Hash() != pkt.Hash() {
		t.Error("hash changed across serialise/parse")
	}
}

func TestPacket_Hops(t *testing.T) {
	p := Packet{HashSize: 2, Path: []byte{0xa1, 0xa2, 0xb1, 0xb2}}
	hops := p.Hops()
	if len(hops) != 2 || !bytes.Equal(hops[0], []byte{0xa1, 0xa2}) || !bytes.Equal(hops[1], []byte{0xb1, 0xb2}) {
		t.Errorf("Hops() = %x", hops)
	}
	if (Packet{HashSize: 3, Path: []byte{1, 2}}).Hops() != nil {
		t.Error("malformed path should yield nil")
	}
	if (Packet{HashSize: 3}).Hops() != nil {
		t.Error("empty path should yield nil")
	}
}

func TestPacket_MatchScope_NamesTheMatchingScope(t *testing.T) {
	payload := []byte{1, 2, 3}
	code, err := CalcTransportCode(FloodScopeKey("sco"), PayloadTypeGrpTxt, payload)
	if err != nil {
		t.Fatalf("CalcTransportCode: %v", err)
	}
	p := Packet{
		Route:          RouteTransportFlood,
		PayloadType:    PayloadTypeGrpTxt,
		Payload:        payload,
		TransportCodes: EncodeTransportCodes(code, 0),
	}
	name, ok, err := p.MatchScope([]string{"fif", "sco", "sco-admin"})
	if err != nil || !ok || name != "sco" {
		t.Errorf("MatchScope = %q, %v, %v; want \"sco\", true, nil", name, ok, err)
	}
	if _, ok, _ := p.MatchScope([]string{"fif", "sco-admin"}); ok {
		t.Error("MatchScope matched a scope the packet wasn't flooded in")
	}
}
