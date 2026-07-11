package meshcore

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBuildGetChannelFrame(t *testing.T) {
	got := BuildGetChannelFrame(3)
	want := []byte{CmdGetChannel, 3}
	if !bytes.Equal(got, want) {
		t.Errorf("BuildGetChannelFrame(3) = %v, want %v", got, want)
	}
}

func TestBuildSetChannelFrame(t *testing.T) {
	secret := make([]byte, 16)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	got, err := BuildSetChannelFrame(2, "general", secret)
	if err != nil {
		t.Fatalf("BuildSetChannelFrame: %v", err)
	}
	if len(got) != 2+channelNameFieldLen+16 {
		t.Fatalf("frame length = %d, want %d", len(got), 2+channelNameFieldLen+16)
	}
	if got[0] != CmdSetChannel || got[1] != 2 {
		t.Errorf("header = %v, want [%#x 2]", got[:2], CmdSetChannel)
	}
	nameField := got[2 : 2+channelNameFieldLen]
	if string(nameField[:7]) != "general" {
		t.Errorf("name field = %q, want %q", nameField[:7], "general")
	}
	for _, b := range nameField[7:] {
		if b != 0 {
			t.Fatalf("expected null-padding after name, got %v", nameField)
		}
	}
	if !bytes.Equal(got[2+channelNameFieldLen:], secret) {
		t.Errorf("secret = %v, want %v", got[2+channelNameFieldLen:], secret)
	}
}

func TestBuildSetChannelFrame_RejectsWrongSecretLength(t *testing.T) {
	if _, err := BuildSetChannelFrame(1, "x", []byte{1, 2, 3}); err == nil {
		t.Fatal("expected an error for a non-16-byte secret")
	}
}

func TestBuildSetChannelFrame_RejectsOverlongName(t *testing.T) {
	longName := make([]byte, channelNameFieldLen)
	for i := range longName {
		longName[i] = 'a'
	}
	if _, err := BuildSetChannelFrame(1, string(longName), make([]byte, 16)); err == nil {
		t.Fatal("expected an error for a name that doesn't leave room for a null terminator")
	}
}

func TestParseChannelInfo_RoundTrip(t *testing.T) {
	secret := HashtagChannelSecret("#general")
	frame := make([]byte, 2+channelNameFieldLen+16)
	frame[0] = FrameChannelInfo
	frame[1] = 4
	copy(frame[2:], "general")
	copy(frame[2+channelNameFieldLen:], secret)

	info, ok := ParseChannelInfo(frame)
	if !ok {
		t.Fatal("ParseChannelInfo returned ok=false")
	}
	if info.Index != 4 {
		t.Errorf("Index = %d, want 4", info.Index)
	}
	if info.Name != "general" {
		t.Errorf("Name = %q, want %q", info.Name, "general")
	}
	if !bytes.Equal(info.Secret, secret) {
		t.Errorf("Secret = %x, want %x", info.Secret, secret)
	}
}

func TestParseChannelInfo_RejectsWrongFrameType(t *testing.T) {
	frame := make([]byte, 2+channelNameFieldLen+16)
	frame[0] = FrameOK
	if _, ok := ParseChannelInfo(frame); ok {
		t.Error("expected ok=false for a non-CHANNEL_INFO frame")
	}
}

func TestParseChannelInfo_RejectsTooShort(t *testing.T) {
	if _, ok := ParseChannelInfo([]byte{FrameChannelInfo, 0}); ok {
		t.Error("expected ok=false for a truncated frame")
	}
}

func TestChannelInfo_IsEmptyChannelSlot(t *testing.T) {
	empty := ChannelInfo{Name: "", Secret: make([]byte, 16)}
	if !empty.IsEmptyChannelSlot() {
		t.Error("expected an all-zero, unnamed slot to be reported empty")
	}

	named := ChannelInfo{Name: "general", Secret: make([]byte, 16)}
	if named.IsEmptyChannelSlot() {
		t.Error("expected a named slot to not be reported empty")
	}

	nonZeroSecret := ChannelInfo{Name: "", Secret: []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}}
	if nonZeroSecret.IsEmptyChannelSlot() {
		t.Error("expected a slot with a non-zero secret to not be reported empty, even with no name")
	}
}

func TestBuildSyncNextMessageFrame(t *testing.T) {
	got := BuildSyncNextMessageFrame()
	want := []byte{CmdSyncNextMessage}
	if !bytes.Equal(got, want) {
		t.Errorf("BuildSyncNextMessageFrame() = %v, want %v", got, want)
	}
}

func TestParseChannelMsgRecv_Standard(t *testing.T) {
	frame := []byte{FrameChannelMsgRecv, 3, 0xFF, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(frame[4:8], 1700000000)
	frame = append(frame, []byte("Alice: hi")...)

	got, ok := ParseChannelMsgRecv(frame)
	if !ok {
		t.Fatal("ParseChannelMsgRecv returned ok=false")
	}
	if got.ChannelIndex != 3 {
		t.Errorf("ChannelIndex = %d, want 3", got.ChannelIndex)
	}
	if got.TimestampUnix != 1700000000 {
		t.Errorf("TimestampUnix = %d, want 1700000000", got.TimestampUnix)
	}
	if got.Text != "Alice: hi" {
		t.Errorf("Text = %q, want %q", got.Text, "Alice: hi")
	}
}

func TestParseChannelMsgRecv_V3(t *testing.T) {
	frame := []byte{FrameChannelMsgRecvV3, 0xEC, 0, 0, 5, 0xFF, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(frame[7:11], 1700000001)
	frame = append(frame, []byte("Bob: hello")...)

	got, ok := ParseChannelMsgRecv(frame)
	if !ok {
		t.Fatal("ParseChannelMsgRecv returned ok=false")
	}
	if got.ChannelIndex != 5 {
		t.Errorf("ChannelIndex = %d, want 5", got.ChannelIndex)
	}
	if got.TimestampUnix != 1700000001 {
		t.Errorf("TimestampUnix = %d, want 1700000001", got.TimestampUnix)
	}
	if got.Text != "Bob: hello" {
		t.Errorf("Text = %q, want %q", got.Text, "Bob: hello")
	}
}

func TestParseChannelMsgRecv_RejectsWrongType(t *testing.T) {
	if _, ok := ParseChannelMsgRecv([]byte{FrameOK, 1, 2, 3}); ok {
		t.Error("expected ok=false for a non-channel-message frame")
	}
}

func TestParseChannelMsgRecv_RejectsTooShort(t *testing.T) {
	if _, ok := ParseChannelMsgRecv([]byte{FrameChannelMsgRecv, 1, 2}); ok {
		t.Error("expected ok=false for a truncated standard frame")
	}
	if _, ok := ParseChannelMsgRecv([]byte{FrameChannelMsgRecvV3, 1, 0, 0, 1, 2}); ok {
		t.Error("expected ok=false for a truncated V3 frame")
	}
}

func TestParseChannelMsgRecv_EmptyFrame(t *testing.T) {
	if _, ok := ParseChannelMsgRecv(nil); ok {
		t.Error("expected ok=false for an empty frame")
	}
}
